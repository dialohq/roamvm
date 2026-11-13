package state

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestConcurrentSharedCheckpoints(t *testing.T) {
	for _, backend := range []string{"objects", "kubernetes"} {
		t.Run(backend, func(t *testing.T) {
			ctx, m, _, path := fixture(t)
			objects := m.Store
			if backend == "kubernetes" {
				scheme := runtime.NewScheme()
				require.NoError(t, core.AddToScheme(scheme))
				m.Store = &Kubernetes{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "runtime", Objects: objects}
			}
			m.Shared = true
			payload := bytes.Repeat([]byte{0x31, 0x72, 0x18}, 300000)
			require.NoError(t, os.WriteFile(path, payload, 0o600))
			var writers errgroup.Group
			for i := range 12 {
				writers.Go(func() error {
					s, err := m.Acquire(ctx, fmt.Sprintf("concurrent-%d", i), "base", "pod", "node")
					if err == nil {
						_, err = m.Snapshot(ctx, s, path, nil)
					}
					return err
				})
			}
			require.NoError(t, writers.Wait())
			keys, err := objects.List(ctx, "chunks/")
			require.NoError(t, err)
			require.Len(t, keys, 1, "concurrent writers duplicated the shared blob")
			for i := range 11 {
				require.NoError(t, m.DeleteVM(ctx, fmt.Sprintf("concurrent-%d", i)))
			}
			last, err := m.Read(ctx, "concurrent-11")
			require.NoError(t, err)
			destination := filepath.Join(t.TempDir(), "restored")
			require.NoError(t, m.Restore(ctx, last, destination))
			actual, err := os.ReadFile(destination)
			require.NoError(t, err)
			require.Equal(t, payload, actual)
			require.NoError(t, m.DeleteVM(ctx, "concurrent-11"))
			keys, err = objects.List(ctx, "chunks/")
			require.NoError(t, err)
			require.Empty(t, keys)
		})
	}
}

func TestSharedChunksSurviveSourceDeletion(t *testing.T) {
	ctx, m, first, path := fixture(t)
	m.Shared = true
	payload := append(bytes.Repeat([]byte("shared payload"), 300000), bytes.Repeat([]byte{0x47}, 1<<20)...)
	require.NoError(t, os.WriteFile(path, payload, 0o600))
	_, err := m.Snapshot(ctx, first, path, nil)
	require.NoError(t, err)
	before, err := m.Store.List(ctx, "chunks/")
	require.NoError(t, err)
	require.NotEmpty(t, before)
	second, err := m.Acquire(ctx, "independent", first.Head.Base, "second", "node-b")
	require.NoError(t, err)
	second, err = m.Snapshot(ctx, second, path, nil)
	require.NoError(t, err)
	after, err := m.Store.List(ctx, "chunks/")
	require.NoError(t, err)
	require.ElementsMatch(t, before, after, "identical VMs duplicated chunk objects")
	require.NoError(t, m.DeleteVM(ctx, first.Head.VMID))
	output := filepath.Join(t.TempDir(), "restored")
	require.NoError(t, m.Restore(ctx, second, output))
	got, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, payload, got)
	require.NoError(t, m.DeleteVM(ctx, second.Head.VMID))
	remaining, err := m.Store.List(ctx, "chunks/")
	require.NoError(t, err)
	require.Empty(t, remaining, "last owner deletion leaked chunk data")
}

type delayedSharedDelete struct {
	Store
	once            sync.Once
	entered, resume chan struct{}
}

func (s *delayedSharedDelete) Delete(ctx context.Context, key string) error {
	if strings.HasPrefix(key, "chunks/") {
		s.once.Do(func() { close(s.entered); <-s.resume })
	}
	return s.Store.Delete(ctx, key)
}

func TestSharedChunkDeletionCannotDeleteReintroducedContent(t *testing.T) {
	ctx, base, first, path := fixture(t)
	store := &delayedSharedDelete{Store: base.Store, entered: make(chan struct{}), resume: make(chan struct{})}
	m := Manager{Store: store, Shared: true}
	_, err := m.Snapshot(ctx, first, path, nil)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- m.DeleteVM(ctx, first.Head.VMID) }()
	<-store.entered
	second, err := m.Acquire(ctx, "new-owner", first.Head.Base, "new-pod", "node-b")
	require.NoError(t, err)
	second, err = m.Snapshot(ctx, second, path, nil)
	require.NoError(t, err)
	close(store.resume)
	require.NoError(t, <-done)
	output := filepath.Join(t.TempDir(), "restored")
	require.NoError(t, m.Restore(ctx, second, output))
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	got, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

type sharedUploadFailure struct{ Store }

func (s sharedUploadFailure) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, match string) (Object, error) {
	if strings.Contains(key, "/overlay/") {
		return Object{}, io.ErrUnexpectedEOF
	}
	return s.Store.Put(ctx, key, body, size, match)
}

func TestFailedSharedCheckpointCanBeDeleted(t *testing.T) {
	ctx, m, session, path := fixture(t)
	m.Shared, m.Store = true, sharedUploadFailure{m.Store}
	_, err := m.Snapshot(ctx, session, path, nil)
	require.Error(t, err)
	_, err = m.Recover(ctx, session.Head.VMID, session.Head.Owner)
	require.NoError(t, err)
	require.NoError(t, m.DeleteVM(ctx, session.Head.VMID))
	keys, err := m.Store.List(ctx, "chunks/")
	require.NoError(t, err)
	require.Empty(t, keys)
}
