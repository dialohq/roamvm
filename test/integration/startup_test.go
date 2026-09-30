//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func BenchmarkStartup(b *testing.B) {
	l := newLab(b)
	nodeName := os.Getenv("ROAMVM_TEST_NODE")
	if nodeName == "" {
		b.Fatal("set ROAMVM_TEST_NODE to hold placement constant")
	}
	l.exclusiveNode(nodeName, "")
	node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	l.get(node)
	ip := ""
	for _, a := range node.Status.Addresses {
		if a.Type == core.NodeInternalIP {
			ip = a.Address
		}
	}
	v := l.spec("startup")
	v.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": nodeName}
	if p := os.Getenv("ROAMVM_TEST_PORT"); p != "" {
		n, e := strconv.ParseInt(p, 10, 32)
		must(b, e)
		v.Spec.ReadinessPort = int32(n)
	}
	if memory := os.Getenv("ROAMVM_TEST_MEMORY"); memory != "" {
		v.Spec.Memory = memory
	}
	if cpus := os.Getenv("ROAMVM_TEST_CPUS"); cpus != "" {
		n, e := strconv.ParseInt(cpus, 10, 32)
		must(b, e)
		v.Spec.CPUs = int32(n)
	}
	service := l.service(v, core.ServiceTypeNodePort)
	address := net.JoinHostPort(ip, strconv.Itoa(int(service.Spec.Ports[0].NodePort)))
	ready := func() bool {
		conn, e := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if e != nil {
			return false
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(100 * time.Millisecond))
		if v.Spec.ReadinessPort == 22 {
			line, e := bufio.NewReader(conn).ReadString('\n')
			return e == nil && strings.HasPrefix(line, "SSH-2.0-")
		}
		if _, e = fmt.Fprint(conn, "GET /ready HTTP/1.0\r\nHost: guest\r\n\r\n"); e != nil {
			return false
		}
		response, e := http.ReadResponse(bufio.NewReader(conn), nil)
		if e != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == 200
	}
	b.ResetTimer()
	b.StopTimer()
	var total time.Duration
	for i := 0; i < b.N; i++ {
		if os.Getenv("ROAMVM_TEST_COLD_CACHE") == "1" {
			l.exclusiveNode(nodeName, "")
			l.run(nil, "docker", "exec", nodeName, "crictl", "rmi", l.image)
		}
		observations := map[string]time.Duration{}
		started := time.Now()
		b.StartTimer()
		if i == 0 {
			l.create(v)
		} else {
			l.power(v.Name, "Running")
		}
		var current *api.VirtualMachine
		var lastObserved time.Time
		err := wait.PollUntilContextTimeout(
			l.ctx,
			20*time.Millisecond,
			180*time.Second,
			true,
			func(context.Context) (bool, error) {
				if time.Since(lastObserved) > 100*time.Millisecond {
					current = l.vm(v.Name)
					lastObserved = time.Now()
					record := func(key string, occurred bool) {
						if _, ok := observations[key]; !ok && occurred {
							observations[key] = time.Since(started)
						}
					}
					record(current.Status.Phase, current.Status.Phase != "")
					switch current.Status.Phase {
					case "RecoveryRequired", "Error", "Blocked":
						return false, fmt.Errorf("%s: %s", current.Status.Phase, current.Status.Message)
					}
					if current.Status.PodName != "" {
						p := &core.Pod{ObjectMeta: meta(current.Status.PodName)}
						if l.Get(l.ctx, client.ObjectKeyFromObject(p), p) == nil {
							record("podCreated", true)
							record("scheduled", p.Spec.NodeName != "")
							for _, c := range p.Status.ContainerStatuses {
								record("containerRunning", c.State.Running != nil)
							}
						}
					}
					for _, c := range current.Status.Conditions {
						record("vmReady", c.Type == "Ready" && c.Status == metav1.ConditionTrue)
					}
				}
				return ready(), nil
			},
		)
		b.StopTimer()
		must(b, err)
		elapsed := time.Since(started)
		total += elapsed
		b.Logf("boot %d actual guest response: %s; observed stages: %v", i, elapsed, observations)
		current = l.vm(v.Name)
		logs, e := l.kube.CoreV1().
			Pods("default").
			GetLogs(current.Status.PodName, &core.PodLogOptions{Container: "runner"}).
			DoRaw(l.ctx)
		must(b, e)
		for _, line := range strings.Split(string(logs), "\n") {
			if strings.Contains(line, "stage=") {
				b.Log(line)
			}
		}
		l.stop(v.Name)
	}
	b.ReportMetric(total.Seconds()/float64(b.N), "guest-seconds/op")
}

func TestExistingVM(t *testing.T) {
	name := os.Getenv("ROAMVM_TEST_EXISTING_VM")
	if name == "" {
		t.Skip("set ROAMVM_TEST_EXISTING_VM to an existing stopped SSH VM")
	}
	l := newLab(t)
	v := l.vm(name)
	equal(t, "existing VM initially stopped", v.Spec.PowerState, "Stopped")
	t.Cleanup(func() { l.power(name, "Stopped"); l.phase(name, "Stopped") })
	identity := ""
	for range 2 {
		l.power(name, "Running")
		v = l.phase(name, "Running")
		l.wait("SSH readiness", func() (bool, error) { p := l.pod(v.Status.PodName); return podReady(p), nil })
		address, closeForward := l.forward(v.Status.PodName, 22)
		_, port, err := net.SplitHostPort(address)
		must(t, err)
		data := l.run(nil, "ssh-keyscan", "-T", "5", "-t", "ed25519", "-p", port, "127.0.0.1")
		key := ""
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if len(f) == 3 && f[1] == "ssh-ed25519" {
				key = f[1] + " " + f[2]
			}
		}
		if key == "" {
			t.Fatal("missing SSH host identity")
		}
		if identity != "" {
			equal(t, "persistent SSH host identity", key, identity)
		}
		identity = key
		closeForward()
		l.stop(name)
	}
}
