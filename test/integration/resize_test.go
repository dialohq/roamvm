//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type guestStorage struct {
	BootID          string
	PID             int
	Uptime          float64
	FilesystemBytes int64
	DiskSectors     string
	GrowthSize      int64
	GrowthHash      string
	GrowthHeld      bool
	GrowthPasses    int
}

func TestOnlineResize(t *testing.T) {
	image := os.Getenv("ROAMVM_TEST_RESIZE_IMAGE")
	if image == "" {
		t.Skip("set ROAMVM_TEST_RESIZE_IMAGE to the NixOS resize fixture")
	}
	l := newLab(t)
	l.networkClient()
	v := l.spec("resize")
	v.Spec.Image = image
	v.Spec.Memory = "1Gi"
	l.create(v)
	l.service(v, core.ServiceTypeClusterIP)
	l.ready(v.Name)
	inspect := func() guestStorage {
		var result guestStorage
		must(t, json.Unmarshal(l.request(v.Name, "/storage", nil), &result))
		return result
	}
	before := inspect()
	if before.BootID == "" || before.FilesystemBytes <= 0 {
		t.Fatalf("invalid guest identity: %+v", before)
	}
	for _, size := range []int64{2 << 30, 3 << 30, 4 << 30} {
		l.request(v.Name, "/growth?action=hold", []byte{})
		l.wait("growth pass held after filesystem resize", func() (bool, error) { return inspect().GrowthHeld, nil })
		v = l.vm(v.Name)
		old := v.DeepCopy()
		v.Spec.RootDiskSize = fmt.Sprint(size)
		started := time.Now()
		must(t, l.Patch(l.ctx, v, client.MergeFrom(old)))
		l.wait("new block capacity while grow service is busy", func() (bool, error) {
			current := inspect()
			equal(t, "grow service remains held", current.GrowthHeld, true)
			return current.DiskSectors == fmt.Sprintln(size/512), nil
		})
		l.request(v.Name, "/growth?action=release", []byte{})
		// The periodic timer is disabled. A notification delivered while the
		// oneshot was active must cause another pass as soon as it finishes.
		must(t, wait.PollUntilContextTimeout(l.ctx, 100*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
			current := inspect()
			equal(t, "same guest boot", current.BootID, before.BootID)
			equal(t, "same running process", current.PID, before.PID)
			if current.Uptime <= before.Uptime {
				t.Fatal("process uptime reset")
			}
			return current.FilesystemBytes > size*9/10 && current.DiskSectors == fmt.Sprintln(size/512), nil
		}))
		l.request(v.Name, "/growth?action=check", []byte{})
		l.wait("reported disk capacity", func() (bool, error) {
			vm := l.vm(v.Name)
			return vm.Status.RootDiskSize == size && apimeta.IsStatusConditionTrue(vm.Status.Conditions, "DiskReady"), nil
		})
		t.Logf("live growth to %d bytes including ext4: %s; boot/process preserved", size, time.Since(started))
	}
	passes := inspect().GrowthPasses
	if passes == 0 {
		t.Fatal("fixture did not count growth passes")
	}
	// The last growpart can queue one final no-op pass. It must not repeatedly
	// reopen the disk writable and feed its own udev watch indefinitely.
	time.Sleep(3 * time.Second)
	if current := inspect().GrowthPasses; current > passes+1 {
		t.Fatalf("growth service keeps retriggering: %d -> %d passes", passes, current)
	}
	for _, size := range []string{"1Gi", "0", "513", "17Ti"} {
		vm := l.vm(v.Name)
		old := vm.DeepCopy()
		vm.Spec.RootDiskSize = size
		if err := l.Patch(l.ctx, vm, client.MergeFrom(old)); !apierrors.IsInvalid(err) {
			t.Fatalf("size %s accepted: %v", size, err)
		}
	}
	vm := l.vm(v.Name)
	old := vm.DeepCopy()
	vm.Spec.RootDiskSize = ""
	if err := l.Patch(l.ctx, vm, client.MergeFrom(old)); !apierrors.IsInvalid(err) {
		t.Fatalf("size removal accepted: %v", err)
	}
	// Allocate more real filesystem blocks than the entire original filesystem,
	// then write and fsync a random marker at the end of that allocation.
	l.request(v.Name, fmt.Sprintf("/fill?bytes=%d", before.FilesystemBytes+(64<<20)), []byte{})
	grown := inspect()
	if grown.GrowthSize <= before.FilesystemBytes || grown.GrowthHash == "" {
		t.Fatalf("missing growth proof: %+v", grown)
	}
	node := l.vm(v.Name).Status.NodeName
	l.stop(v.Name)
	l.cordon(node, true)
	defer l.cordon(node, false)
	v = l.start(v.Name)
	if v.Status.NodeName == node {
		t.Fatal("restart did not move VM")
	}
	restored := inspect()
	if restored.BootID == before.BootID {
		t.Fatal("cold restart retained boot ID")
	}
	equal(t, "restored enlarged block device", restored.DiskSectors, grown.DiskSectors)
	equal(t, "restored filesystem", restored.FilesystemBytes, grown.FilesystemBytes)
	equal(t, "restored file allocation", restored.GrowthSize, grown.GrowthSize)
	equal(t, "restored marker beyond original capacity", restored.GrowthHash, grown.GrowthHash)
	l.stop(v.Name)
	t.Logf("grew partitioned root online and restored on %s after cordoning %s", v.Status.NodeName, node)
}

func TestResizeWithoutExpandableStorage(t *testing.T) {
	l := newLab(t)
	l.networkClient()
	v := l.spec("resize-denied")
	l.create(v)
	l.service(v, core.ServiceTypeClusterIP)
	l.ready(v.Name)
	before := l.request(v.Name, "/data", []byte("resize failure must preserve this running guest"))
	v = l.vm(v.Name)
	pod := l.pod(v.Status.PodName)
	var pvc core.PersistentVolumeClaim
	must(t, l.Get(l.ctx, client.ObjectKey{Namespace: "default", Name: pod.Name + "-working"}, &pvc))
	if pvc.Spec.StorageClassName == nil || (*pvc.Spec.StorageClassName != "standard" && *pvc.Spec.StorageClassName != "local-path") {
		t.Skip("this test targets the lab's non-expandable local-path class")
	}
	old := v.DeepCopy()
	v.Spec.RootDiskSize = "80Gi" // Requires 81Gi, exceeding the default 64Gi PVC.
	must(t, l.Patch(l.ctx, v, client.MergeFrom(old)))
	l.wait("PVC expansion refused without stopping VM", func() (bool, error) {
		current := l.vm(v.Name)
		condition := apimeta.FindStatusCondition(current.Status.Conditions, "DiskReady")
		return current.Status.Phase == "Running" && condition != nil && condition.Status == "False" && strings.Contains(condition.Message, "support resize"), nil
	})
	equal(t, "guest still responds with original data", l.request(v.Name, "/data", nil), before)
	equal(t, "same runner", l.vm(v.Name).Status.PodName, pod.Name)
	l.stop(v.Name)
}
