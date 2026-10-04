//go:build integration

package integration

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"k8s.io/utils/cpuset"
)

func (l *lab) libvirt() bool {
	return l.cluster == "roamvm-libvirt" || strings.HasPrefix(l.cluster, "roamvm-libvirt-")
}

func (l *lab) nodeExec(input []byte, node string, args ...string) []byte {
	l.t.Helper()
	command := []string{"docker", "exec", "-i", node}
	if l.libvirt() {
		command = []string{"bash", "../libvirt/lab.sh", "ssh", node}
		// SSH sends a command string, not an argv vector.
		quoted := make([]string, len(args))
		for i, arg := range args {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
		}
		args = []string{strings.Join(quoted, " ")}
	}
	return l.run(input, append(command, args...)...)
}

func (l *lab) powerNode(node string, running bool) {
	l.t.Helper()
	if l.libvirt() {
		action := "destroy"
		if running {
			action = "start"
		}
		l.run(nil, "virsh", action, node)
	} else if running {
		l.run(nil, "docker", "start", node)
	} else {
		l.run(nil, "docker", "stop", "--time", "0", node)
	}
}

func (l *lab) pauseStore(paused bool) {
	l.t.Helper()
	if l.libvirt() {
		signal := "SIGCONT"
		if paused {
			signal = "SIGSTOP"
		}
		l.nodeExec(nil, l.cluster+"-control-plane", "systemctl", "kill", "--signal="+signal, "--kill-whom=main", "minio")
		return
	}
	container := os.Getenv("ROAMVM_TEST_S3_CONTAINER")
	if container == "" {
		l.t.Fatal("set ROAMVM_TEST_S3_CONTAINER to this lab's object-store container")
	}
	action := "unpause"
	if paused {
		action = "pause"
	}
	l.run(nil, "docker", action, container)
}

// Restrict execution to two host CPUs while keeping all guest vCPUs visible.
func (l *lab) pinLibvirtCPUs(node string) {
	l.t.Helper()
	var available unix.CPUSet
	must(l.t, unix.SchedGetaffinity(0, &available))
	var cpus []string
	for cpu := 0; cpu < 1024 && len(cpus) < 2; cpu++ {
		if available.IsSet(cpu) {
			cpus = append(cpus, strconv.Itoa(cpu))
		}
	}
	if len(cpus) != 2 {
		l.t.Fatal("fewer than two available host CPUs")
	}
	output := string(l.run(nil, "virsh", "vcpupin", node, "--live"))
	var pins [][2]string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			continue
		}
		pins = append(pins, [2]string{fields[0], fields[1]})
	}
	if len(pins) < 3 {
		l.t.Fatal("expected more than two libvirt vCPUs")
	}
	// Restore even if changing one of the later vCPU affinities fails.
	l.t.Cleanup(func() {
		for _, pin := range pins {
			l.run(nil, "virsh", "vcpupin", node, pin[0], pin[1], "--live")
		}
	})
	l.t.Logf("constraining %s to host CPUs %s", node, strings.Join(cpus, ","))
	for _, pin := range pins {
		l.run(nil, "virsh", "vcpupin", node, pin[0], strings.Join(cpus, ","), "--live")
	}
	want, err := cpuset.Parse(strings.Join(cpus, ","))
	must(l.t, err)
	output = string(l.run(nil, "virsh", "vcpupin", node, "--live"))
	checked := 0
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			continue
		}
		actual, err := cpuset.Parse(fields[1])
		must(l.t, err)
		equal(l.t, "physical host CPU affinity", actual.List(), want.List())
		checked++
	}
	equal(l.t, "pinned vCPU count", checked, len(pins))
}
