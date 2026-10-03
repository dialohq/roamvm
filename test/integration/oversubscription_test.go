//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/cpuset"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestOversubscription(t *testing.T) {
	nodeName := os.Getenv("ROAMVM_TEST_NODE")
	if nodeName == "" {
		t.Skip("set ROAMVM_TEST_NODE to an idle kind worker")
	}
	l := newLab(t)
	l.exclusiveNode(nodeName, "")
	node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	l.get(node)
	capacity := node.Status.Capacity.Cpu().Value()
	if capacity <= 2 {
		t.Fatal("test requires more than two host CPUs")
	}
	config := l.nodeExec(nil, nodeName, "cat", "/var/lib/kubelet/config.yaml")
	if strings.Contains(string(config), "kubeReserved:") {
		t.Fatal("use an unmodified kind worker")
	}
	originalSet := ""
	if !l.libvirt() {
		originalSet = strings.TrimSpace(string(l.run(nil, "docker", "inspect", nodeName, "--format", "{{.HostConfig.CpusetCpus}}")))
	}
	effective, err := cpuset.Parse(
		strings.TrimSpace(
			string(l.nodeExec(nil, nodeName, "cat", "/sys/fs/cgroup/cpuset.cpus.effective")),
		),
	)
	must(t, err)
	cpus := effective.List()
	if len(cpus) < 2 {
		t.Fatal("fewer than two effective CPUs")
	}
	cpus = cpus[:2]
	writeConfig := func(data []byte) {
		l.nodeExec(data, nodeName, "sh", "-c", "cat > /var/lib/kubelet/config.yaml")
		service := "kubelet"
		if l.libvirt() {
			service = "k3s"
		}
		l.nodeExec(nil, nodeName, "systemctl", "restart", service)
	}
	allocatable := func(expected int64) {
		l.wait(
			"CPU allocation and schedulable worker",
			func() (bool, error) {
				l.get(node)
				slots := node.Status.Allocatable["vm.roamvm.io/kvm"]
				if node.Status.Allocatable.Cpu().Value() != expected || slots.Value() < 2 {
					return false, nil
				}
				// K3s publishes CPU capacity before CRI/CNI and device slots are ready.
				// Submitting Pods then sends them through scheduler failure/backoff.
				for _, taint := range node.Spec.Taints {
					if taint.Key == core.TaintNodeNotReady || taint.Key == core.TaintNodeUnreachable {
						return false, nil
					}
				}
				for _, condition := range node.Status.Conditions {
					if condition.Type == core.NodeReady {
						return condition.Status == core.ConditionTrue, nil
					}
				}
				return false, nil
			},
		)
	}
	restoreCPUs := func() { l.run(nil, "docker", "update", "--cpuset-cpus", originalSet, nodeName) }
	if l.libvirt() {
		l.pinLibvirtCPUs(nodeName)
		restoreCPUs = func() {}
	}
	// Register before fixture cleanup so Pods stop while the constrained node is still running.
	t.Cleanup(func() {
		writeConfig(config)
		restoreCPUs()
		allocatable(capacity)
	})
	if !l.libvirt() {
		l.run(nil, "docker", "update", "--cpuset-cpus", cpuset.New(cpus...).String(), nodeName)
	}
	writeConfig(
		append(
			slices.Clone(config),
			[]byte(fmt.Sprintf("\nkubeReserved:\n  cpu: %q\n", strconv.FormatInt(capacity-2, 10)))...,
		),
	)
	allocatable(2)
	l.networkClient()
	names := []string{}
	for range 2 {
		v := l.spec("overcommit")
		v.Spec.CPUs = 4
		v.Spec.Memory = "256Mi"
		v.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": nodeName}
		v.Spec.Resources = core.ResourceRequirements{
			Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("500m")},
			Limits:   core.ResourceList{core.ResourceCPU: resource.MustParse("4")},
		}
		l.create(v)
		l.service(v, core.ServiceTypeClusterIP)
		names = append(names, v.Name)
	}
	podNames := map[string]string{}
	cgroup := func(name, file string) (string, error) {
		p := podNames[name]
		data, e := l.exec(
			p,
			"runner",
			nil,
			"/bin/sh",
			"-c",
			`for p in /nix/store/*-coreutils-*/bin/cat /bin/cat /usr/bin/cat; do if [ -x "$p" ]; then exec "$p" "$1"; fi; done; exit 1`,
			"cat",
			"/sys/fs/cgroup/"+file,
		)
		return strings.TrimSpace(string(data)), e
	}
	read := func(name, file string) string { data, e := cgroup(name, file); must(t, e); return data }
	runnerCPUs := cpus
	if l.libvirt() {
		// Host pinning limits physical execution, not the node's visible vCPUs.
		runnerCPUs = effective.List()
	}
	for _, name := range names {
		v := l.ready(name)
		podNames[name] = v.Status.PodName
		p := l.pod(v.Status.PodName)
		equal(t, "placement", p.Spec.NodeName, nodeName)
		equal(t, "scheduler", p.Spec.SchedulerName, "default-scheduler")
		equal(t, "shared accounting", p.Status.QOSClass, core.PodQOSBurstable)
		equal(t, "CPU request", p.Spec.Containers[0].Resources.Requests.Cpu().String(), "500m")
		actual, e := cpuset.Parse(read(name, "cpuset.cpus.effective"))
		must(t, e)
		equal(t, "runner CPU affinity", actual.List(), runnerCPUs)
		equal(t, "four-CPU ceiling", strings.Fields(read(name, "cpu.max")), []string{"400000", "100000"})
		equal(t, "guest writes", string(l.request(name, "/data", []byte(name))), name)
	}
	usage := func(name string) (float64, error) {
		data, e := cgroup(name, "cpu.stat")
		if e != nil {
			return 0, e
		}
		fields := strings.Fields(data)
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] == "usage_usec" {
				v, e := strconv.ParseFloat(fields[i+1], 64)
				return v / 1e6, e
			}
		}
		return 0, fmt.Errorf("missing usage_usec: %s", data)
	}
	load := func(name string) (float64, error) {
		before, e := usage(name)
		if e != nil {
			return 0, e
		}
		started := time.Now()
		data, e := l.http(name, "/cpu?seconds=10", []byte{})
		if e != nil {
			return 0, e
		}
		var report struct {
			CPUs       int      `json:"cpus"`
			Iterations []uint64 `json:"iterations"`
		}
		if e = json.Unmarshal(data, &report); e != nil {
			return 0, e
		}
		if report.CPUs != 4 || len(report.Iterations) != 4 || slices.Contains(report.Iterations, 0) {
			return 0, fmt.Errorf("guest did not exercise all CPUs: %s", data)
		}
		after, e := usage(name)
		return (after - before) / time.Since(started).Seconds(), e
	}
	burst, err := load(names[0])
	must(t, err)
	t.Logf("idle bursting: %.3f cores", burst)
	if burst <= 0.75 {
		t.Fatal("guest did not burst above its 500m request")
	}
	type result struct {
		cores float64
		err   error
	}
	results := make(chan result, 2)
	for _, name := range names {
		go func() { v, e := load(name); results <- result{v, e} }()
	}
	for range names {
		r := <-results
		must(t, r.err)
		t.Logf("concurrent guest: %.3f cores", r.cores)
		if r.cores <= 0.25 {
			t.Fatal("guest starved under contention")
		}
	}
	pending := l.spec("excessive-request")
	pending.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": nodeName}
	pending.Spec.Resources.Requests[core.ResourceCPU] = resource.MustParse("2")
	l.create(pending)
	l.phase(pending.Name, "Pending")
	l.wait("scheduler rejects excessive CPU request", func() (bool, error) {
		p := &core.Pod{ObjectMeta: meta(l.vm(pending.Name).Status.PodName)}
		if err := l.Get(l.ctx, client.ObjectKeyFromObject(p), p); err != nil {
			return false, err
		}
		for _, c := range p.Status.Conditions {
			if c.Reason == "Unschedulable" && strings.Contains(c.Message, "Insufficient cpu") {
				return true, nil
			}
		}
		return false, nil
	})
	l.power(pending.Name, "Stopped")
	l.phase(pending.Name, "Stopped")
	for _, name := range names {
		equal(t, "checkpoint after contention", l.stop(name).Checkpoint.Generation, int64(1))
	}
	v := l.vm(names[0])
	base := v.DeepCopy()
	v.Spec.Resources = core.ResourceRequirements{
		Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("125m")},
		Limits:   core.ResourceList{core.ResourceCPU: resource.MustParse("250m")},
	}
	must(t, l.Patch(l.ctx, v, client.MergeFrom(base)))
	podNames[v.Name] = l.start(v.Name).Status.PodName
	equal(t, "VMM CPU cap", strings.Fields(read(v.Name, "cpu.max")), []string{"25000", "100000"})
	capped, err := load(v.Name)
	must(t, err)
	t.Logf("capped guest: %.3f cores", capped)
	if capped < 0.1 || capped > 0.4 {
		t.Fatal("250m CPU limit not enforced")
	}
	for _, name := range names {
		l.start(name)
		equal(t, "restored private disk", string(l.request(name, "/data", nil)), name)
		l.stop(name)
	}
}
