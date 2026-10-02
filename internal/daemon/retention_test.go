package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
	"github.com/dialohq/roamvm/internal/disk"
	"github.com/dialohq/roamvm/internal/images"
	"github.com/dialohq/roamvm/internal/state"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type committedStore struct {
	state.Store
	head state.Head
	old  string
	fail bool
}

func (s *committedStore) Get(context.Context, string) (state.Object, error) {
	b, err := json.Marshal(s.head)
	return state.Object{Body: io.NopCloser(bytes.NewReader(b)), ETag: "committed"}, err
}

func (s *committedStore) List(context.Context, string) ([]string, error) {
	return []string{s.old, s.head.Checkpoint.Key}, nil
}

func (s *committedStore) Delete(_ context.Context, key string) error {
	if key != s.old {
		return errors.New("attempted to delete current checkpoint")
	}
	if s.fail {
		return errors.New("delete unavailable")
	}
	s.old = ""
	return nil
}

func TestRestartedSidecarCompletesCleanupBeforeStopped(t *testing.T) {
	ctx := context.Background()
	key := func(epoch int) string {
		return fmt.Sprintf("vm/vm-uid/overlay/%020d-%020d-%s.qcow2", epoch, epoch, strings.Repeat("a", 64))
	}
	store := &committedStore{
		head: state.Head{
			Schema:     1,
			VMID:       "vm-uid",
			Epoch:      2,
			State:      "Stopped",
			Checkpoint: &api.Checkpoint{Key: key(2), Generation: 2},
		},
		old:  key(1),
		fail: true,
	}
	scheme := runtime.NewScheme()
	core.AddToScheme(scheme)
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod",
			Namespace:   "default",
			UID:         "pod-uid",
			Annotations: map[string]string{controller.Phase: "Checkpointing"},
		},
	}
	vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{UID: "vm-uid"}}
	server := &Server{
		Root:   t.TempDir(),
		State:  state.Manager{Store: store},
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
	}
	working := filepath.Join(server.Root, "overlay.qcow2")
	require.NoError(t, os.WriteFile(working, []byte("working copy"), 0o600))
	require.NoError(t, server.save(Prepared{Session: state.Session{Head: state.Head{Epoch: 2, State: "Running"}}, PodUID: string(pod.UID), Dir: server.Root}))
	require.Error(t, server.finish(ctx, pod, vm), "cleanup failure ignored")
	require.NoError(t, server.Client.Get(ctx, client.ObjectKeyFromObject(pod), pod))
	if pod.Annotations[controller.Phase] != "Checkpointing" {
		t.Fatal("reported Stopped before cleanup")
	}
	if _, err := os.Stat(working); err != nil {
		t.Fatal("discarded working copy", err)
	}
	store.fail = false
	// No prepare/check/upload occurs on this retry; only persisted session/head state is available.
	require.NoError(t, server.finish(ctx, pod, vm))
	require.NoError(t, server.Client.Get(ctx, client.ObjectKeyFromObject(pod), pod))
	if pod.Annotations[controller.Phase] != "Stopped" || store.old != "" {
		t.Fatal("cleanup did not complete")
	}
	if _, err := os.Stat(working); !os.IsNotExist(err) {
		t.Fatal("working copy retained after completion", err)
	}
}

type overlayStore struct {
	state.Store
	t       *testing.T
	path    string
	key     string
	data    []byte
	failure string
}

func (s *overlayStore) Put(_ context.Context, key string, body io.ReadSeeker, size int64, match string) (state.Object, error) {
	s.t.Helper()
	f, ok := body.(*os.File)
	require.True(s.t, ok, "checkpoint must stream from a file")
	require.Equal(s.t, s.path, f.Name(), "must upload the original overlay, not a copy")
	entries, err := os.ReadDir(filepath.Dir(s.path))
	require.NoError(s.t, err)
	for _, entry := range entries {
		require.Contains(s.t, []string{"overlay.qcow2", "runner.lock"}, entry.Name(), "checkpoint must not create a second local disk")
	}
	require.Empty(s.t, match)
	if s.failure == "upload" {
		return state.Object{}, errors.New("upload unavailable")
	}
	if s.key == key {
		return state.Object{}, state.ErrConflict
	}
	s.data, err = io.ReadAll(body)
	require.NoError(s.t, err)
	require.Equal(s.t, size, int64(len(s.data)))
	s.key = key
	return state.Object{}, nil
}

func (s *overlayStore) Get(_ context.Context, key string) (state.Object, error) {
	require.Equal(s.t, s.key, key)
	data := s.data
	if s.failure == "verification" {
		data = append(append([]byte(nil), data...), '!')
	}
	return state.Object{Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func (s *overlayStore) List(context.Context, string) ([]string, error) {
	return []string{s.key}, nil
}

func TestFinishStreamsWorkingOverlayAndRetainsItUntilVerified(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	ctx := t.Context()
	scheme := runtime.NewScheme()
	require.NoError(t, core.AddToScheme(scheme))
	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runner", Namespace: "default", UID: "pod-uid"}}
	vm := &api.VirtualMachine{ObjectMeta: metav1.ObjectMeta{UID: "vm-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	dir := t.TempDir()
	base := images.Base{Dir: t.TempDir(), Manifest: images.Manifest{Format: "raw"}}
	require.NoError(t, os.WriteFile(base.Disk(), bytes.Repeat([]byte{0x55}, 4<<20), 0o444))
	path := filepath.Join(dir, "overlay.qcow2")
	require.NoError(t, disk.Create(ctx, base, path))
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	objects := &overlayStore{t: t, path: path}
	m := state.Manager{Store: &state.Kubernetes{Client: c, Namespace: "default", Objects: objects}}
	session, err := m.Acquire(ctx, string(vm.UID), "base", string(pod.UID), "node")
	require.NoError(t, err)
	s := &Server{Root: t.TempDir(), Client: c, State: m}
	require.NoError(t, s.save(Prepared{Session: session, Base: base, Dir: dir, PodUID: string(pod.UID)}))
	for _, failure := range []string{"upload", "verification"} {
		objects.failure = failure
		require.Error(t, s.finish(ctx, pod, vm), failure)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, want, got, "failed checkpoint must preserve the working disk")
		require.NoError(t, m.Check(ctx, session), "failed checkpoint must retain ownership")
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pod), pod))
		require.Equal(t, "Checkpointing", pod.Annotations[controller.Phase])
	}
	objects.failure = ""
	require.NoError(t, s.finish(ctx, pod, vm))
	require.Equal(t, want, objects.data)
	require.NoFileExists(t, path)
	stopped, err := m.Read(ctx, string(vm.UID))
	require.NoError(t, err)
	require.Equal(t, "Stopped", stopped.Head.State)
	require.Empty(t, stopped.Head.Owner)
	restored := filepath.Join(t.TempDir(), "overlay.qcow2")
	require.NoError(t, m.Restore(ctx, stopped, restored))
	got, err := os.ReadFile(restored)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.NoError(t, s.finish(ctx, pod, vm), "retry after local cleanup")
}
