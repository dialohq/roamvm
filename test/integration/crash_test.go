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
	for _, failure := range []string{"hypervisor", "runner", "oom"} {
		t.Run(failure, func(t *testing.T) {
			l := newLab(t)
			l.networkClient()
			l.storage()
			v := l.spec("crash")
			if failure == "oom" {
				v.Spec.Resources.Limits = core.ResourceList{core.ResourceMemory: resource.MustParse("2Gi")}
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
				expectedMax = "2147483648"
			}
			equal(t, "runner memory limit", strings.TrimSpace(string(memoryMax)), expectedMax)
			command := `for p in /proc/[0-9]*; do read -r name < "$p/comm" || continue; case "$name" in qemu-system-*) kill -KILL "${p##*/}"; exit 0;; esac; done; exit 1`

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
		must(l.t, json.Unmarshal(l.run(nil, "docker", "exec", pod.Spec.NodeName, "crictl", "inspect", id), &container))
		if container.Info.PID <= 1 {
			l.t.Fatal("invalid container PID", container.Info.PID)
		}
		l.run(nil, "docker", "exec", pod.Spec.NodeName, "kill", "-KILL", strconv.Itoa(container.Info.PID))
		return
	}
	l.t.Fatal("container not found", name)
}
