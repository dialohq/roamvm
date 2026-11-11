//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/state"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestHibernation(t *testing.T) {
	l := newLab(t)
	if !api.IsV2(l.Scheme()) {
		t.Skip("set ROAMVM_TEST_V2=1 and install the experimental operator")
	}
	nodeName := os.Getenv("ROAMVM_TEST_NODE")
	if nodeName == "" {
		t.Fatal("set ROAMVM_TEST_NODE")
	}
	l.exclusiveNode(nodeName, "")
	node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	l.get(node)
	var ip string
	for _, address := range node.Status.Addresses {
		if address.Type == core.NodeInternalIP {
			ip = address.Address
		}
	}
	v := l.spec("hibernate")
	v.Spec.Hibernate = true
	v.Spec.Memory = "1Gi"
	v.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": nodeName}
	svc := l.service(v, core.ServiceTypeNodePort)
	address := fmt.Sprintf("http://%s:%d", ip, svc.Spec.Ports[0].NodePort)
	// A new Pod gets new NAT state; pre-nap TCP sessions are not portable. Test
	// guest state and new requests, rather than reusing a previous incarnation's
	// pooled connection (which can stall even after readiness succeeds).
	httpClient := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	request := func(method, path string, body io.Reader) ([]byte, error) {
		req, err := http.NewRequestWithContext(l.ctx, method, address+path, body)
		if err != nil {
			return nil, err
		}
		res, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, b)
		}
		return b, err
	}
	ready := func() {
		previous := httpClient.Timeout
		httpClient.Timeout = 100 * time.Millisecond
		defer func() { httpClient.Timeout = previous }()
		l.wait("actual guest response", func() (bool, error) { _, err := request("GET", "/ready", nil); return err == nil, err })
	}
	logs := func() {
		current := l.vm(v.Name)
		b, err := l.kube.CoreV1().Pods("default").GetLogs(current.Status.PodName, &core.PodLogOptions{Container: "runner"}).DoRaw(l.ctx)
		must(t, err)
		t.Logf("restored runner logs:\n%s", b)
		t.Logf("restored disk: %s", l.run(nil, "kubectl", "exec", current.Status.PodName, "-c", "runtime", "--", "stat", "-c", "logical_bytes=%s allocated_blocks=%b", "/var/lib/roamvm/running/"+string(v.UID)+"/overlay.qcow2"))
	}
	l.create(v)
	require.True(t, l.vm(v.Name).Spec.Hibernate, "install the current CRD; old schemas prune hibernate")
	ready()
	// Generating incompressible RAM under nested KVM is setup, not resume time.
	httpClient.Timeout = time.Minute
	sentinel, err := request("POST", "/memory?bytes=536870912", nil)
	must(t, err)
	httpClient.Timeout = 10 * time.Second
	var want map[string]any
	must(t, json.Unmarshal(sentinel, &want))
	require.EqualValues(t, 536870912, want["bytes"])
	diskSentinel := "synced disk generation 0"
	_, err = request("POST", "/data", strings.NewReader(diskSentinel))
	must(t, err)
	kubernetes := &core.Service{ObjectMeta: meta("kubernetes")}
	l.get(kubernetes)
	verify := func() {
		got, err := request("GET", "/memory", nil)
		must(t, err)
		var actual map[string]any
		must(t, json.Unmarshal(got, &actual))
		require.Equal(t, want, actual, "guest boot, process or in-memory random payload changed")
		got, err = request("GET", "/data", nil)
		must(t, err)
		require.Equal(t, diskSentinel, string(got), "disk generation did not match captured RAM")
		got, err = request("GET", "/dns?name=kubernetes.default.svc.cluster.local", nil)
		must(t, err)
		var addresses []string
		must(t, json.Unmarshal(got, &addresses))
		require.Contains(t, addresses, kubernetes.Spec.ClusterIP)
	}
	for i := 0; i < 3; i++ {
		head := l.stop(v.Name)
		require.NotNil(t, head.Checkpoint.Hibernation)
		require.Equal(t, "chunks-v1", head.Checkpoint.Format)
		started := time.Now()
		l.power(v.Name, "Running")
		ready()
		t.Logf("warm resume %d actual guest response: %s", i, time.Since(started))
		logs()
		verify()
		diskSentinel = fmt.Sprintf("synced disk generation %d", i+1)
		_, err = request("POST", "/data", strings.NewReader(diskSentinel))
		must(t, err)
	}
	// A contradictory node selector forces the existing controller's cold
	// fallback: it cannot satisfy this start using the retained local PVC.
	l.stop(v.Name)
	other := l.cluster + "-worker"
	if other == nodeName {
		other = l.cluster + "-worker2"
	}
	l.exclusiveNode(other, "")
	must(t, l.Patch(l.ctx, l.vm(v.Name), client.RawPatch(types.MergePatchType, []byte(fmt.Sprintf(`{"spec":{"nodeSelector":{"kubernetes.io/hostname":%q},"powerState":"Running"}}`, other)))))
	started := time.Now()
	ready()
	t.Logf("cross-node cold recovery actual guest response: %s", time.Since(started))
	logs()
	require.Equal(t, other, l.vm(v.Name).Status.NodeName)
	verify()
	l.stop(v.Name)
	// Explicitly opting out must boot the retained disk, not restore old RAM.
	must(t, l.Patch(l.ctx, l.vm(v.Name), client.RawPatch(types.MergePatchType, []byte(`{"spec":{"hibernate":false,"powerState":"Running"}}`))))
	ready()
	got, err := request("GET", "/memory", nil)
	must(t, err)
	var actual map[string]any
	must(t, json.Unmarshal(got, &actual))
	require.EqualValues(t, 0, actual["bytes"])
	require.NotEqual(t, want["bootID"], actual["bootID"])
	got, err = request("GET", "/data", nil)
	must(t, err)
	require.Equal(t, diskSentinel, string(got))
	head := l.stop(v.Name)
	require.Nil(t, head.Checkpoint.Hibernation)
	// VM deletion, unlike a stop, must remove its private objects and head.
	prefix := "vm/" + string(v.UID) + "/"
	keys, err := l.store.List(l.ctx, prefix)
	must(t, err)
	require.NotEmpty(t, keys)
	must(t, l.Delete(l.ctx, v))
	l.gone(v)
	keys, err = l.store.List(l.ctx, prefix)
	must(t, err)
	require.Empty(t, keys)
	_, err = l.state.Read(l.ctx, string(v.UID))
	require.ErrorIs(t, err, state.ErrNotFound)
}
