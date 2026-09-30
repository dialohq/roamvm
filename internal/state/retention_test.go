package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func retentionManager(objects Store, backend string) Manager {
	if backend == "kubernetes" {
		scheme := runtime.NewScheme()
		core.AddToScheme(scheme)
		return Manager{
			&Kubernetes{
				Client:    fake.NewClientBuilder().WithScheme(scheme).Build(),
				Namespace: "state",
				Objects:   objects,
			},
		}
	}
	return Manager{objects}
}

func TestCheckpointReplacementFailures(t *testing.T) {
	for _, backend := range []string{"s3", "kubernetes"} {
		for _, failure := range []string{"upload", "verification", "list", "delete", "lost-response", "lost-delete-response"} {
			t.Run(backend+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				objects := newMemory()
				m := retentionManager(objects, backend)
				path := filepath.Join(t.TempDir(), "overlay")
				if err := os.WriteFile(path, []byte("first stop"), 0o600); err != nil {
					t.Fatal(err)
				}
				first, err := m.Acquire(ctx, "vm", "base", "one", "node-a")
				if err != nil {
					t.Fatal(err)
				}
				first, err = m.Commit(ctx, first, path)
				if err != nil {
					t.Fatal(err)
				}
				second, err := m.Acquire(ctx, "vm", "base", "two", "node-b")
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, []byte("second stop"), 0o600); err != nil {
					t.Fatal(err)
				}
				objects.failure = failure
				_, err = m.Commit(ctx, second, path)
				if failure != "lost-response" && err == nil {
					t.Fatal("injected failure ignored")
				}
				if failure == "lost-response" && err != nil {
					t.Fatal(err)
				}
				objects.failure = ""
				current, err := m.Read(ctx, "vm")
				if err != nil {
					t.Fatal(err)
				}
				if failure == "upload" || failure == "verification" {
					if current.ETag != second.ETag {
						t.Fatal("failed upload replaced current checkpoint")
					}
					checkRestore(t, m, first, "first stop")
					current, err = m.Commit(ctx, second, path)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					// A restarted sidecar resumes cleanup from the durable head.
					if err = m.Prune(ctx, current); err != nil {
						t.Fatal(err)
					}
				}
				keys, err := objects.List(ctx, "vm/vm/overlay/")
				if err != nil || len(keys) != 1 || keys[0] != current.Head.Checkpoint.Key {
					t.Fatal("stop history retained", keys, err)
				}
				checkRestore(t, m, current, "second stop")
			})
		}
	}
}

func checkRestore(t *testing.T, m Manager, s Session, want string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restored")
	if err := m.Restore(context.Background(), s, path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Fatalf("restore: %q, %v", b, err)
	}
}

type pausedDelete struct {
	Store
	key             string
	once            sync.Once
	entered, resume chan struct{}
}

func (s *pausedDelete) Delete(ctx context.Context, key string) error {
	if key == s.key {
		pause := false
		s.once.Do(func() { pause = true })
		if pause {
			close(s.entered)
			<-s.resume
		}
	}
	return s.Store.Delete(ctx, key)
}

func TestDelayedCleanupCannotDeleteNewCheckpoint(t *testing.T) {
	for _, backend := range []string{"s3", "kubernetes"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			objects := newMemory()
			paused := &pausedDelete{Store: objects, entered: make(chan struct{}), resume: make(chan struct{})}
			m := retentionManager(paused, backend)
			path := filepath.Join(t.TempDir(), "overlay")
			commit := func(owner, data string) Session {
				t.Helper()
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
				s, err := m.Acquire(ctx, "vm", "base", owner, owner)
				if err != nil {
					t.Fatal(err)
				}
				s, err = m.Commit(ctx, s, path)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			first := commit("one", "first stop")
			second := commit("two", "second stop")
			// Seed old history left by the previous implementation.
			paused.key = first.Head.Checkpoint.Key
			if _, err := objects.Put(ctx, paused.key, bytes.NewReader([]byte("obsolete")), 8, ""); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- m.Prune(ctx, second) }()
			<-paused.entered
			third := commit("three", "newest stop")
			close(paused.resume)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			keys, err := objects.List(ctx, "vm/vm/overlay/")
			if err != nil || len(keys) != 1 || keys[0] != third.Head.Checkpoint.Key {
				t.Fatal(keys, err)
			}
			checkRestore(t, m, third, "newest stop")
		})
	}
}

func TestCleanupPreservesFutureUploadsAndUnrelatedObjects(t *testing.T) {
	ctx, m, s, path := fixture(t)
	committed, err := m.Commit(ctx, s, path)
	if err != nil {
		t.Fatal(err)
	}
	future := fmt.Sprintf("vm/%s/overlay/%020d-%020d-%s.qcow2", s.Head.VMID, 2, s.Head.Epoch+1, strings.Repeat("a", 64))
	keys := []string{future, "vm/other/overlay/keep", "vm/vm-1/overlay/unknown-format"}
	for _, key := range keys {
		if _, err = m.Store.Put(ctx, key, strings.NewReader("keep"), 4, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err = m.Prune(ctx, committed); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		object, err := m.Store.Get(ctx, key)
		if err != nil {
			t.Fatal("unrelated/in-flight object deleted", key, err)
		}
		object.Body.Close()
	}
	committed.Head.Checkpoint.Key = "vm/other/overlay/invalid"
	if err = m.Prune(ctx, committed); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatal("invalid checkpoint accepted", err)
	}
}
