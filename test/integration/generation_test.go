//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNixOSGenerations(t *testing.T) {
	for _, tc := range []struct {
		name, image, mode string
	}{
		{"migrate", os.Getenv("ROAMVM_TEST_GENERATION_IMAGE"), ""},
		{"firmware", os.Getenv("ROAMVM_TEST_FIRMWARE_IMAGE"), "Disk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.image == "" {
				t.Skip("set ROAMVM_TEST_GENERATION_IMAGE / ROAMVM_TEST_FIRMWARE_IMAGE to the generation fixtures")
			}
			l := newLab(t)
			l.networkClient()
			v := l.spec("generation")
			v.Spec.Image = tc.image
			v.Spec.BootMode = tc.mode
			v.Spec.Hostname = "generation-workspace"
			v.Spec.Memory = "1Gi"
			l.create(v)
			l.service(v, core.ServiceTypeClusterIP)
			l.ready(v.Name)
			inspect := func() map[string]string {
				var result map[string]string
				must(t, json.Unmarshal(l.request(v.Name, "/generation", nil), &result))
				equal(t, "workspace hostname", result["hostname"], v.Spec.Hostname)
				return result
			}
			initial := inspect()
			equal(t, "initial generation", initial["generation"], "initial")
			marker := []byte("workspace files survive generation changes")
			l.request(v.Name, "/data", marker)
			l.request(v.Name, "/rebuild?action=test", []byte{})
			equal(t, "test activation", inspect()["generation"], "updated")
			l.stop(v.Name)
			l.start(v.Name)
			equal(t, "test is not the boot default", inspect()["generation"], "initial")

			l.request(v.Name, "/rebuild?action=switch", []byte{})
			switched := inspect()
			equal(t, "switch activation", switched["generation"], "updated")
			require.Contains(t, switched["cmdline"], "generation=initial")
			node := l.vm(v.Name).Status.NodeName
			l.stop(v.Name)
			v = l.vm(v.Name)
			old := v.DeepCopy()
			v.Spec.BootMode = "Disk"
			must(t, l.Patch(l.ctx, v, client.MergeFrom(old)))
			l.cordon(node, true)
			defer l.cordon(node, false)
			restarted := l.start(v.Name)
			require.NotEqual(t, node, restarted.Status.NodeName)
			equal(t, "same workspace identity", restarted.UID, v.UID)
			equal(t, "same immutable base", restarted.Spec.Image, tc.image)
			after := inspect()
			equal(t, "booted switched generation", after["generation"], "updated")
			require.Contains(t, after["cmdline"], "generation=updated")
			equal(t, "booted selected system", after["booted-system"], switched["current-system"])
			require.NotEqual(t, switched["bootID"], after["bootID"])
			equal(t, "workspace data", strings.TrimSpace(string(l.request(v.Name, "/data", nil))), string(marker))

			l.request(v.Name, "/rebuild?action=rollback", []byte{})
			equal(t, "rollback activation", inspect()["generation"], "initial")
			l.stop(v.Name)
			l.start(v.Name)
			rolledBack := inspect()
			equal(t, "rollback persisted", rolledBack["generation"], "initial")
			require.Contains(t, rolledBack["cmdline"], "generation=initial")
			equal(t, "rollback data", string(l.request(v.Name, "/data", nil)), string(marker))
			l.stop(v.Name)
		})
	}
}
