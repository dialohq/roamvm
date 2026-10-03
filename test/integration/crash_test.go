//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestLocalCrashRecovery(t *testing.T) {
	// Only the stateless HTTP probe is shared; each failure gets its own VM.
	shared := newLab(t)
	shared.networkClient()
	for _, failure := range []string{"hypervisor", "runner", "oom"} {
		t.Run(failure, func(t *testing.T) {
			l := newLab(t)
			l.probe = shared.probe
			l.storage()
			v := l.spec("crash")
			if failure == "oom" {
				// Exceed the 512 MiB guest plus required hypervisor overhead,
				// but leave room for node services on a 2 GiB lab worker.
				v.Spec.Resources.Limits = core.ResourceList{core.ResourceMemory: resource.MustParse("1280Mi")}
			}
			l.create(v)
			l.service(v, core.ServiceTypeClusterIP)
			v = l.ready(v.Name)
			payload := bytes.Repeat([]byte("saved before first stop\n"), 4096)
			equal(t, "write", l.request(v.Name, "/data", payload), payload)
			if l.head(v).Checkpoint != nil {
				t.Fatal("test requires no previous checkpoint")
			}
			pod := l.pod(v.Status.PodName)
			memoryMax, err := l.exec(pod.Name, "runner", nil, "/bin/sh", "-c", "cat /sys/fs/cgroup/memory.max")
			must(t, err)
			expectedMax := "max"
			if failure == "oom" {
				expectedMax = "1342177280"
			}
			equal(t, "runner memory limit", strings.TrimSpace(string(memoryMax)), expectedMax)
			globalOOMs := 0
			if failure == "oom" && l.libvirt() {
				globalOOMs = strings.Count(string(l.nodeExec(nil, pod.Spec.NodeName, "dmesg")), "global_oom")
			}
			command := `for p in /proc/[0-9]*; do read -r name < "$p/comm" || continue; case "$name" in qemu-system-*|.qemu-system-*) kill -KILL "${p##*/}"; exit 0;; esac; done; exit 1`

			if failure == "oom" {
				command = `awk 'BEGIN { s="xxxxxxxxxxxxxxxx"; for(i=0;i<16;i++) s=s s; for(i=0;i<4096;i++) a[i]=s i }'`
			}
			if failure == "runner" {
				l.killContainer(pod, "runner")
			} else {
				_, _ = l.exec(pod.Name, "runner", nil, "/bin/sh", "-c", command)
			}
			stopped := l.phase(v.Name, "Stopped")
			equal(t, "no automatic boot loop", stopped.Spec.PowerState, "Stopped")
			if stopped.Status.Local == nil {
				t.Fatal("crashed disk was not retained locally")
			}
			if stopped.Status.Message == "" {
				t.Fatal("crash reason lost")
			}
			if failure == "oom" && !bytes.Contains([]byte(stopped.Status.Message), []byte("OOMKilled")) {
				t.Fatal(stopped.Status.Message)
			}
			if failure == "oom" && l.libvirt() {
				equal(t, "container OOM must not exhaust the node", strings.Count(string(l.nodeExec(nil, pod.Spec.NodeName, "dmesg")), "global_oom"), globalOOMs)
			}
			l.gone(pod)
			l.start(v.Name)
			equal(t, "restart preserves uncheckpointed work", l.request(v.Name, "/data", nil), payload)
			l.stop(v.Name)
			t.Logf("%s: first-boot working disk survived crash and normal stop/start", failure)
		})
	}
}

func (l *lab) killContainer(pod *core.Pod, name string) {
	l.t.Helper()
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name != name {
			continue
		}
		id := strings.TrimPrefix(c.ContainerID, "containerd://")
		var container struct {
			Info struct {
				PID int `json:"pid"`
			} `json:"info"`
		}
		args := []string{"crictl", "inspect", id}
		if l.libvirt() {
			args = []string{"crictl", "--config", "/dev/null", "--runtime-endpoint", "unix:///run/k3s/containerd/containerd.sock", "inspect", id}
		}
		must(l.t, json.Unmarshal(l.nodeExec(nil, pod.Spec.NodeName, args...), &container))
		if container.Info.PID <= 1 {
			l.t.Fatal("invalid container PID", container.Info.PID)
		}
		l.nodeExec(nil, pod.Spec.NodeName, "kill", "-KILL", strconv.Itoa(container.Info.PID))
		return
	}
	l.t.Fatal("container not found", name)
}
