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
)

func TestWarmEvictionAndDiskClone(t *testing.T) {
	l := newLab(t)
	if !api.IsV2(l.Scheme()) {
		t.Skip("requires experimental v2 operator")
	}
	nodeName := os.Getenv("ROAMVM_TEST_NODE")
	require.NotEmpty(t, nodeName)
	l.exclusiveNode(nodeName, "")
	node := &core.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
	l.get(node)
	var ip string
	for _, address := range node.Status.Addresses {
		if address.Type == core.NodeInternalIP {
			ip = address.Address
		}
	}
	v := l.spec("warm-storage")
	v.Spec.Hibernate = true
	v.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": nodeName}
	svc := l.service(v, core.ServiceTypeNodePort)
	httpClient := &http.Client{Timeout: time.Minute, Transport: &http.Transport{DisableKeepAlives: true}}
	request := func(service *core.Service, method, path, body string) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(l.ctx, method, fmt.Sprintf("http://%s:%d%s", ip, service.Spec.Ports[0].NodePort, path), strings.NewReader(body))
		must(t, err)
		res, err := httpClient.Do(req)
		must(t, err)
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		must(t, err)
		require.Equal(t, 200, res.StatusCode, string(data))
		return data
	}
	ready := func(vm *api.VirtualMachine) {
		service := &core.Service{ObjectMeta: meta(vm.Name)}
		l.get(service)
		probe := &http.Client{Timeout: 200 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
		l.wait(vm.Name+" actual readiness", func() (bool, error) {
			current := l.vm(vm.Name)
			for _, condition := range current.Status.Conditions {
				if condition.Type == "Ready" && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == current.Generation {
					req, err := http.NewRequestWithContext(l.ctx, "GET", fmt.Sprintf("http://%s:%d/ready", ip, service.Spec.Ports[0].NodePort), nil)
					if err != nil {
						return false, err
					}
					res, err := probe.Do(req)
					if err != nil {
						return false, err
					}
					res.Body.Close()
					return res.StatusCode == 200, nil
				}
			}
			return false, nil
		})
	}
	l.create(v)
	ready(v)
	var memory map[string]any
	must(t, json.Unmarshal(request(svc, "POST", "/memory?bytes=67108864", ""), &memory))
	const sentinel = "project prepared: writes survive cold eviction and independent cloning"
	request(svc, "POST", "/data", sentinel)
	l.stop(v.Name)
	paused := l.vm(v.Name)
	claim := &core.PersistentVolumeClaim{ObjectMeta: meta(paused.Status.Local.ClaimName)}
	// The collector's real clock is unchanged. Age the stopped cache rather than
	// weakening retention globally or sleeping for the production TTL.
	age := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	paused.Status.Local.StoppedAt = &age
	must(t, l.Status().Update(l.ctx, paused))
	l.wait("automatic warm cache eviction", func() (bool, error) { return l.vm(v.Name).Status.Local == nil, nil })
	l.gone(claim)
	require.NotNil(t, l.vm(v.Name).Status.Checkpoint)
	started := time.Now()
	l.power(v.Name, "Running")
	ready(v)
	var restored map[string]any
	must(t, json.Unmarshal(request(svc, "GET", "/memory", ""), &restored))
	require.Equal(t, memory, restored, "cold restore lost process, boot identity or random RAM")
	require.Equal(t, sentinel, string(request(svc, "GET", "/data", "")))
	t.Logf("automatic-eviction cold resume to actual guest response: %s", time.Since(started))
	l.stop(v.Name)

	clone := l.spec("project-clone")
	clone.Spec.PowerState = "Stopped"
	clone.Spec.CloneFrom = &api.CloneSource{Name: v.Name, UID: string(v.UID)}
	l.create(clone)
	l.wait("independent disk-only clone committed", func() (bool, error) { return l.vm(clone.Name).Status.Checkpoint != nil, nil })
	cp := l.vm(clone.Name).Status.Checkpoint
	require.Nil(t, cp.Hibernation, "project template retained source RAM")
	require.True(t, strings.HasPrefix(cp.Key, "vm/"+string(clone.UID)+"/"))
	sourceUID := v.UID
	must(t, l.Delete(l.ctx, v))
	l.gone(v)
	keys, err := l.store.List(l.ctx, "vm/"+string(sourceUID)+"/")
	must(t, err)
	require.Empty(t, keys, "source objects leaked after deletion")
	_, err = l.state.Read(l.ctx, string(sourceUID))
	require.ErrorIs(t, err, state.ErrNotFound)
	cloneService := l.service(clone, core.ServiceTypeNodePort)
	started = time.Now()
	l.power(clone.Name, "Running")
	ready(clone)
	require.Equal(t, sentinel, string(request(cloneService, "GET", "/data", "")))
	var fresh map[string]any
	must(t, json.Unmarshal(request(cloneService, "GET", "/memory", ""), &fresh))
	require.EqualValues(t, 0, fresh["bytes"], "clone restored source RAM")
	require.NotEqual(t, memory["bootID"], fresh["bootID"])
	t.Logf("independent project clone boot to actual guest response: %s", time.Since(started))
}
