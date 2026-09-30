package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/controller"
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
	// No prepare/compact/upload occurs on this retry; only persisted session/head state is available.
	require.NoError(t, server.finish(ctx, pod, vm))
	require.NoError(t, server.Client.Get(ctx, client.ObjectKeyFromObject(pod), pod))
	if pod.Annotations[controller.Phase] != "Stopped" || store.old != "" {
		t.Fatal("cleanup did not complete")
	}
	if _, err := os.Stat(working); !os.IsNotExist(err) {
		t.Fatal("working copy retained after completion", err)
	}
}
