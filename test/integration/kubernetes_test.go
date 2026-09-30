//go:build integration

package integration

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/state"
	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestKubernetes(t *testing.T) {
	l := newLab(t)
	pending := l.spec("pending")
	pending.Spec.NodeSelector = map[string]string{"roamvm.test/nonexistent": "true"}
	l.create(pending)
	l.phase(pending.Name, "Pending")
	l.power(pending.Name, "Stopped")
	l.phase(pending.Name, "Stopped")
	var pods core.PodList
	must(
		t,
		l.List(l.ctx, &pods, client.InNamespace("default"), client.MatchingLabels{"vm.roamvm.io/name": pending.Name}),
	)
	equal(t, "unscheduled VM cancelled without a Pod", len(pods.Items), 0)
	l.storage()
	_, err := l.state.Read(l.ctx, string(pending.UID))
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("unscheduled VM must have no durable state: %v", err)
	}
	bad := l.spec("unpinned")
	bad.Spec.Image = "registry.invalid/test:latest"
	if err = l.Create(l.ctx, bad); !apierrors.IsInvalid(err) {
		t.Fatalf("mutable image should be invalid, got %v", err)
	}

	l.networkClient()
	v := l.spec("kubernetes")
	name := v.Name
	cm := &core.ConfigMap{ObjectMeta: meta(name), Data: map[string]string{"setting": "first"}}
	secret := &core.Secret{ObjectMeta: meta(name), StringData: map[string]string{"credential": "local-fixture-only"}}
	l.create(cm)
	l.create(secret)
	configSource := core.VolumeProjection{
		ConfigMap: &core.ConfigMapProjection{LocalObjectReference: core.LocalObjectReference{Name: name}},
	}
	secretSource := core.VolumeProjection{
		Secret: &core.SecretProjection{LocalObjectReference: core.LocalObjectReference{Name: name}},
	}
	v.Spec.ConfigDisks = []api.ConfigDisk{
		{
			Name:       "agent",
			Label:      "TEST_AGENT",
			Projection: core.ProjectedVolumeSource{Sources: []core.VolumeProjection{secretSource}},
		},
		{
			Name:       "tool",
			Label:      "TEST_TOOL",
			Projection: core.ProjectedVolumeSource{Sources: []core.VolumeProjection{configSource}},
		},
	}
	bad = v.DeepCopy()
	bad.Name = unique("duplicate-label")
	bad.Spec.ConfigDisks[1].Label = "TEST_AGENT"
	if err = l.Create(l.ctx, bad, &client.CreateOptions{DryRun: []string{metav1.DryRunAll}}); !apierrors.IsInvalid(
		err,
	) {
		t.Fatalf("duplicate label should be invalid: %v", err)
	}
	pvc := &core.PersistentVolumeClaim{ObjectMeta: meta(name), Spec: core.PersistentVolumeClaimSpec{
		AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
		Resources: core.VolumeResourceRequirements{
			Requests: core.ResourceList{core.ResourceStorage: resource.MustParse("256Mi")},
		},
	}}
	l.create(pvc)
	helper := &core.Pod{ObjectMeta: meta(name + "-prepare"), Spec: core.PodSpec{
		RestartPolicy: core.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
		Containers: []core.Container{
			{
				Name:  "prepare",
				Image: "alpine:3.23",
				Command: []string{
					"sh",
					"-ec",
					`apk add --no-cache e2fsprogs; mkdir /tmp/root; printf prepared-by-kubernetes > /tmp/root/marker; truncate -s 64M /volume/disk.img; mke2fs -q -t ext4 -F -d /tmp/root /volume/disk.img`,
				},
				VolumeMounts: []core.VolumeMount{{Name: "disk", MountPath: "/volume"}},
			},
		},
		Volumes: []core.Volume{
			{
				Name: "disk",
				VolumeSource: core.VolumeSource{
					PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: name},
				},
			},
		},
	}}
	l.create(helper)
	l.wait("secondary disk preparation", func() (bool, error) {
		p := l.pod(helper.Name)
		if p.Status.Phase == core.PodFailed {
			return false, io.ErrUnexpectedEOF
		}
		return p.Status.Phase == core.PodSucceeded, nil
	})
	must(t, l.Delete(l.ctx, helper))
	l.gone(helper)
	v.Spec.Disks = []api.SecondaryDisk{{Name: "workspace", ClaimName: name, VolumeMode: "Filesystem"}}
	v.Spec.Config = &core.ProjectedVolumeSource{Sources: []core.VolumeProjection{configSource, secretSource}}
	v.Spec.Resources.Limits = core.ResourceList{core.ResourceCPU: resource.MustParse("2")}
	// Keep the numeric JSON quantity regression covered through a real SSA request.
	encoded, err := json.Marshal(v)
	must(t, err)
	encoded = []byte(strings.Replace(string(encoded), `"limits":{"cpu":"2"}`, `"limits":{"cpu":2}`, 1))
	apply := func(dryRun bool) error {
		options := []client.PatchOption{client.FieldOwner("roamvm-test")}
		if dryRun {
			options = append(options, client.DryRunAll)
		}
		return l.Patch(
			l.ctx,
			&api.VirtualMachine{ObjectMeta: meta(name)},
			client.RawPatch(types.ApplyPatchType, encoded),
			options...)
	}
	must(t, apply(false))
	t.Cleanup(func() {
		if !t.Failed() {
			must(t, client.IgnoreNotFound(l.Delete(l.ctx, v)))
			l.gone(v)
		}
	})
	l.service(v, core.ServiceTypeClusterIP)
	v = l.ready(name)
	must(t, apply(true))
	config := func() map[string]string {
		var data map[string]string
		must(t, json.Unmarshal(l.request(name, "/config", nil), &data))
		return data
	}
	equal(
		t,
		"projected ConfigMap and Secret",
		config(),
		map[string]string{"setting": "first", "credential": "local-fixture-only"},
	)
	var disks map[string]string
	must(t, json.Unmarshal(l.request(name, "/config-disks", nil), &disks))
	equal(
		t,
		"separate read-only config disks",
		disks,
		map[string]string{"agent": "local-fixture-only", "tool": "first"},
	)
	equal(t, "secondary disk initial bytes", string(l.request(name, "/secondary", nil)), "prepared-by-kubernetes")
	equal(
		t,
		"secondary disk write",
		string(l.request(name, "/secondary", []byte("guest-pvc-write"))),
		"guest-pvc-write",
	)

	if os.Getenv("ROAMVM_TEST_NETWORK_POLICY") == "1" {
		policy := &networking.NetworkPolicy{
			ObjectMeta: meta(name),
			Spec: networking.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"vm.roamvm.io/name": name}},
				PolicyTypes: []networking.PolicyType{networking.PolicyTypeIngress},
			},
		}
		l.create(policy)
		l.wait("CNI denies ingress", func() (bool, error) {
			_, e := l.exec(l.probe, "curl", nil, "curl", "-fsS", "--max-time", "2", "http://"+name+":8080/ready")
			return e != nil, nil
		})
		base := policy.DeepCopy()
		policy.Spec.Ingress = []networking.NetworkPolicyIngressRule{
			{
				From: []networking.NetworkPolicyPeer{
					{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"roamvm.test/client": l.probe}}},
				},
			},
		}
		must(t, l.Patch(l.ctx, policy, client.MergeFrom(base)))
		l.ready(name)
		t.Log("CNI deny/allow enforcement passed")
	} else {
		t.Log("NetworkPolicy disabled; enable on a CNI that enforces it")
	}
	err = l.Patch(
		l.ctx,
		v,
		client.RawPatch(
			types.MergePatchType,
			[]byte(`{"spec":{"image":"registry.invalid/test@sha256:`+strings.Repeat("0", 64)+`"}}`),
		),
	)
	if !apierrors.IsInvalid(err) {
		t.Fatalf("base-image mutation must be invalid: %v", err)
	}
	address, closeForward := l.forward(v.Status.PodName, 8080)
	hc := &http.Client{Timeout: 3 * time.Second}
	response, err := hc.Get("http://" + address + "/ready")
	must(t, err)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	equal(t, "guest through Pod port forwarding", string(body), "ready\n")
	closeForward()
	must(t, l.Patch(l.ctx, cm, client.RawPatch(types.MergePatchType, []byte(`{"data":{"setting":"second"}}`))))
	equal(t, "configuration is a boot-time snapshot", config()["setting"], "first")
	l.stop(name)
	l.start(name)
	equal(t, "configuration refreshes at next boot", config()["setting"], "second")
	equal(t, "secondary disk survives restart", string(l.request(name, "/secondary", nil)), "guest-pvc-write")
	l.request(name, "/data", []byte("finalizer-commits-this"))
	must(t, l.Delete(l.ctx, v))
	l.gone(v)
	h := l.head(v)
	equal(t, "deletion durably stopped VM", h.State, "Stopped")
	equal(t, "deletion released owner", h.Owner, "")
	equal(t, "deletion committed new generation", h.Checkpoint.Generation, int64(2))
	l.get(pvc)
}
