package state

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesOwnershipWithObjectOnlyStore(t *testing.T) {
	scheme := runtime.NewScheme()
	core.AddToScheme(scheme)
	objects := newMemory()
	store := &Kubernetes{
		Client:    fake.NewClientBuilder().WithScheme(scheme).Build(),
		Namespace: "runtime",
		Objects:   objects,
	}
	m := Manager{Store: store}
	ctx := context.Background()
	var winners atomic.Int32
	var wg sync.WaitGroup
	sessions := make(chan Session, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, e := m.Acquire(ctx, "vm-1", "base", "pod-"+string(rune('a'+i)), "node-a")
			if e == nil {
				winners.Add(1)
				sessions <- s
			}
		}(i)
	}
	wg.Wait()
	close(sessions)
	if winners.Load() != 1 {
		t.Fatal("multiple ownership winners", winners.Load())
	}
	s := <-sessions
	p := filepath.Join(t.TempDir(), "overlay")
	os.WriteFile(p, []byte("durable bytes"), 0o600)
	committed, e := m.Commit(ctx, s, p)
	require.NoError(t, e)
	if committed.Head.Checkpoint.Generation != 1 {
		t.Fatal(committed)
	}
	if _, e = objects.Get(ctx, HeadKey("vm-1")); !errors.Is(e, ErrNotFound) {
		t.Fatal("S3 used for mutable coordination")
	}
	next, e := m.Acquire(ctx, "vm-1", "base", "next-pod", "node-b")
	require.NoError(t, e)
	restored := filepath.Join(t.TempDir(), "restored")
	require.NoError(t, m.Restore(ctx, next, restored))
	got, _ := os.ReadFile(restored)
	if string(got) != "durable bytes" {
		t.Fatal("restore differs")
	}
	if _, e = m.Commit(ctx, s, p); !errors.Is(e, ErrOwned) {
		t.Fatal("old epoch committed", e)
	}
}

type pausedUpload struct {
	Store
	entered chan struct{}
	resume  chan struct{}
}

func (s *pausedUpload) Put(
	ctx context.Context,
	key string,
	body io.ReadSeeker,
	size int64,
	match string,
) (Object, error) {
	if strings.Contains(key, "/overlay/") {
		close(s.entered)
		<-s.resume
	}
	return s.Store.Put(ctx, key, body, size, match)
}

func TestOwnershipChangeDuringUploadCannotPublishStaleCheckpoint(t *testing.T) {
	for _, backend := range []string{"s3", "kubernetes"} {
		t.Run(backend, func(t *testing.T) {
			objects := newMemory()
			paused := &pausedUpload{Store: objects, entered: make(chan struct{}), resume: make(chan struct{})}
			var store Store = paused
			if backend == "kubernetes" {
				scheme := runtime.NewScheme()
				core.AddToScheme(scheme)
				store = &Kubernetes{
					Client:    fake.NewClientBuilder().WithScheme(scheme).Build(),
					Namespace: "runtime",
					Objects:   paused,
				}
			}
			ctx := context.Background()
			m := Manager{Store: store}
			first, e := m.Acquire(ctx, "vm-race", "base", "old-pod", "node-a")
			require.NoError(t, e)
			path := filepath.Join(t.TempDir(), "overlay")
			os.WriteFile(path, []byte("stale overlay"), 0o600)
			result := make(chan error, 1)
			go func() { _, e := m.Commit(ctx, first, path); result <- e }()
			<-paused.entered
			if _, e = m.Recover(ctx, "vm-race", "old-pod"); e != nil {
				t.Fatal(e)
			}
			replacement, e := m.Acquire(ctx, "vm-race", "base", "new-pod", "node-b")
			require.NoError(t, e)
			close(paused.resume)
			if e = <-result; !errors.Is(e, ErrConflict) {
				t.Fatalf("stale upload committed: %v", e)
			}
			current, e := m.Read(ctx, "vm-race")
			require.NoError(t, e)
			if current.ETag != replacement.ETag || current.Head.Checkpoint != nil || current.Head.Owner != "new-pod" {
				t.Fatal("new owner was overwritten", current)
			}
		})
	}
}
