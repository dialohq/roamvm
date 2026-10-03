//go:build integration

package integration

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestLifecycle(t *testing.T) {
	l := newLab(t)
	l.networkClient()
	l.storage()
	v := l.spec("lifecycle")
	l.create(v)
	l.service(v, core.ServiceTypeClusterIP)
	started := time.Now()
	v = l.ready(v.Name)
	t.Logf("first guest response: %s", time.Since(started))
	pod := l.pod(v.Status.PodName)
	equal(t, "scheduler", pod.Spec.SchedulerName, "default-scheduler")
	working := false
	for _, vol := range pod.Spec.Volumes {
		if vol.HostPath != nil {
			t.Fatal("VM Pod uses hostPath")
		}
		working = working || vol.Name == "working" && vol.PersistentVolumeClaim != nil
	}
	equal(t, "retained working PVC", working, true)
	pvc := &core.PersistentVolumeClaim{ObjectMeta: meta(pod.Name + "-working")}
	l.get(pvc)
	equal(t, "working PVC owner", pvc.OwnerReferences[0].UID, v.UID)
	equal(t, "CPU request", pod.Spec.Containers[0].Resources.Requests.Cpu().String(), "250m")
	dns := l.request(v.Name, "/dns?name=kubernetes.default.svc.cluster.local", nil)
	if len(dns) < 5 {
		t.Fatalf("empty DNS response: %s", dns)
	}
	payload := make([]byte, 1<<20)
	_, err := rand.Read(payload)
	must(t, err)
	equal(t, "guest write", l.request(v.Name, "/data", payload), payload)
	first := l.stop(v.Name)
	oldNode := v.Status.NodeName
	l.cordon(oldNode, true)
	t.Cleanup(func() { l.cordon(oldNode, false) })
	v = l.start(v.Name)
	if v.Status.NodeName == oldNode {
		t.Fatal("scheduler did not move VM after cordon")
	}
	equal(t, "cross-node restored bytes", l.request(v.Name, "/data", nil), payload)
	l.cordon(oldNode, false)
	t.Log("scheduler moved VM after cordoning; all bytes preserved")

	pod = l.pod(v.Status.PodName)
	restarts := pod.Status.ContainerStatuses[1].RestartCount
	l.killContainer(pod, "runtime")
	l.wait("runtime sidecar restart", func() (bool, error) {
		p := l.pod(pod.Name)
		s := p.Status.ContainerStatuses
		return len(s) > 1 && s[1].RestartCount > restarts && s[1].State.Running != nil, nil
	})
	l.wait("runtime socket serves after restart", func() (bool, error) {
		_, err := l.exec(pod.Name, "runtime", nil, "curl", "--silent", "--max-time", "1", "--unix-socket", "/run/roamvm/runtime.sock", "http://runtime/", "-o", "/dev/null")
		return err == nil, err
	})
	l.ready(v.Name)
	equal(t, "sidecar restart preserves bytes", l.request(v.Name, "/data", nil), payload)
	equal(t, "VMM did not restart", l.pod(pod.Name).Status.ContainerStatuses[0].RestartCount, int32(0))

	l.pauseStore(true)
	paused := true
	t.Cleanup(func() {
		if paused {
			l.pauseStore(false)
		}
	})
	l.power(v.Name, "Stopped")
	l.phase(v.Name, "Stopped")
	// Observe the outage across several reconciliation/heartbeat intervals.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	<-timer.C
	stopped := l.vm(v.Name)
	equal(t, "outage does not block guest stop", stopped.Status.Phase, "Stopped")
	if stopped.Status.Local == nil || apimeta.IsStatusConditionTrue(stopped.Status.Conditions, "CheckpointReady") {
		t.Fatal("outage must retain local state without claiming remote durability")
	}
	l.get(&core.PersistentVolumeClaim{ObjectMeta: meta(stopped.Status.Local.ClaimName)})
	l.pauseStore(false)
	paused = false
	committed := l.stop(v.Name)
	equal(t, "retry commits once", committed.Checkpoint.Generation, first.Checkpoint.Generation+1)
	l.start(v.Name)
	equal(t, "interrupted upload preserved bytes", l.request(v.Name, "/data", nil), payload)
	for range 3 {
		payload = append(payload, byte(len(payload)%251))
		equal(t, "cycle write", l.request(v.Name, "/data", payload), payload)
		l.stop(v.Name)
		l.start(v.Name)
		equal(t, "cycle restore", l.request(v.Name, "/data", nil), payload)
	}
	t.Log("sidecar restart, object-store outage and three checkpoint cycles passed")

	var restoreWorker func()
	if os.Getenv("ROAMVM_TEST_NODE_FAILURE") == "1" {
		v = l.vm(v.Name)
		node := v.Status.NodeName
		l.exclusiveNode(node, v.Name)
		durable := l.head(v)
		owner := string(l.pod(v.Status.PodName).UID)
		l.request(v.Name, "/data", []byte("uncommitted changes may be discarded"))
		l.cordon(node, true)
		nodeStopped := true
		var restartedAt time.Time
		restoreWorker = func() {
			if nodeStopped {
				if restartedAt.IsZero() {
					restartedAt = time.Now()
					l.powerNode(node, true)
				}
				l.wait("worker Ready with fresh KVM capacity", func() (bool, error) {
					n := &core.Node{}
					err := l.Get(l.ctx, client.ObjectKey{Name: node}, n)
					if err != nil {
						return false, err
					}
					// Ready/allocatable can survive a powered-off worker in the API.
					// Require a new kubelet status update with healthy device slots.
					kvm := n.Status.Allocatable["vm.roamvm.io/kvm"]
					for _, c := range n.Status.Conditions {
						if c.Type == core.NodeReady && c.Status == core.ConditionTrue && c.LastHeartbeatTime.After(restartedAt) && kvm.Sign() > 0 {
							return true, nil
						}
					}
					return false, nil
				})
				l.cordon(node, false)
				nodeStopped = false
			}
		}
		t.Cleanup(restoreWorker)
		l.powerNode(node, false)
		equal(t, "node death retains checkpoint", l.head(v).Checkpoint, durable.Checkpoint)
		equal(t, "node death retains owner", l.head(v).Owner, owner)
		l.recover(v, owner)
		// Recovery has revoked the old owner and deleted its Pod. Reboot the
		// still-cordoned worker while the guest starts on the other node.
		restartedAt = time.Now()
		l.powerNode(node, true)
		v = l.start(v.Name)
		if v.Status.NodeName == node {
			t.Fatal("recovery reused fenced node")
		}
		equal(t, "node recovery uses last durable bytes", l.request(v.Name, "/data", nil), payload)
		t.Log("worker death and explicit fenced recovery passed")
	} else {
		t.Log("worker failure disabled; set ROAMVM_TEST_NODE_FAILURE=1 to exercise it")
	}

	v = l.vm(v.Name)
	durable := l.head(v)
	_, err = l.exec(v.Status.PodName, "runner", nil, "/bin/sh", "-ec", `
 for process in /proc/[0-9]*; do
   read -r name < "$process/comm" || continue
   case "$name" in qemu-system-*|.qemu-system-*) kill -KILL "${process##*/}"; exit 0;; esac
 done
 exit 1`)
	must(t, err)
	l.stop(v.Name)
	equal(t, "VMM crash checkpoints working bytes", l.head(v).Checkpoint.Generation, durable.Checkpoint.Generation+1)
	equal(t, "VMM crash releases owner", l.head(v).Owner, "")
	l.start(v.Name)
	equal(t, "crash recovery", l.request(v.Name, "/data", nil), payload)
	durable = l.stop(v.Name)
	// The rebooted worker is not needed for the preceding local crash checks.
	// Keep it cordoned while those run, then require fresh health before the
	// cross-node restore below. Cleanup also waits if an earlier assertion fails.
	if restoreWorker != nil {
		restoreWorker()
	}
	// Force the remote restore path; a healthy local cache deliberately does
	// not read the S3 object at all.
	cacheNode := l.vm(v.Name).Status.Local.NodeName
	l.cordon(cacheNode, true)
	t.Cleanup(func() { l.cordon(cacheNode, false) })
	object, err := l.store.Get(l.ctx, durable.Checkpoint.Key)
	must(t, err)
	original, err := io.ReadAll(object.Body)
	object.Body.Close()
	must(t, err)
	overwrite := func(data []byte) {
		current, e := l.store.Get(l.ctx, durable.Checkpoint.Key)
		must(t, e)
		current.Body.Close()
		_, e = l.store.Put(l.ctx, durable.Checkpoint.Key, bytes.NewReader(data), int64(len(data)), current.ETag)
		must(t, e)
	}
	overwrite([]byte("injected corruption"))
	damaged := true
	t.Cleanup(func() {
		if damaged {
			overwrite(original)
		}
	})
	l.power(v.Name, "Running")
	v = l.phase(v.Name, "RecoveryRequired")
	log, err := l.kube.CoreV1().
		Pods("default").
		GetLogs(v.Status.PodName, &core.PodLogOptions{Container: "runner"}).
		DoRaw(l.ctx)
	must(t, err)
	// Quiet test kernels can suppress the banner; the runner's spawn marker cannot.
	if !strings.Contains(string(log), "integrity mismatch") || strings.Contains(string(log), "startup stage=hypervisor") || strings.Contains(string(log), "Linux version") {
		t.Fatalf("corruption was not rejected before boot: %s", log)
	}
	owner := l.head(v).Owner
	overwrite(original)
	damaged = false
	l.recover(v, owner)
	l.start(v.Name)
	equal(t, "repaired checkpoint restore", l.request(v.Name, "/data", nil), payload)
	l.stop(v.Name)
	t.Log("VMM crash and corrupt-checkpoint refusal/recovery passed")
}
