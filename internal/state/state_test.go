package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type memoryObject struct {
	body []byte
	etag string
}
type memoryStore struct {
	failure  string
	mu       sync.Mutex
	objects  map[string]memoryObject
	revision int
}

func newMemory() *memoryStore { return &memoryStore{objects: map[string]memoryObject{}} }
func (s *memoryStore) List(ctx context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == "list" {
		return nil, errors.New("injected list failure")
	}
	var keys []string
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func (s *memoryStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == "delete" {
		return errors.New("injected delete failure")
	}
	delete(s.objects, key)
	if s.failure == "lost-delete-response" {
		return errors.New("response lost after successful delete")
	}
	return nil
}

func (s *memoryStore) Get(ctx context.Context, key string) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.objects[key]
	if !ok {
		return Object{}, ErrNotFound
	}
	b := append([]byte(nil), v.body...)
	if (s.failure == "verification") && strings.Contains(key, "/overlay/") {
		b = append(b, '!')
	}
	return Object{Body: io.NopCloser(bytes.NewReader(b)), ETag: v.etag, Size: int64(len(b))}, nil
}

func (s *memoryStore) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, match string) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if (s.failure == "upload") && strings.Contains(key, "/overlay/") {
		return Object{}, errors.New("injected upload failure")
	}
	old, exists := s.objects[key]
	if (match == "" && exists) || (match != "" && (!exists || old.etag != match)) {
		return Object{}, ErrConflict
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return Object{}, err
	}
	if int64(len(b)) != size {
		return Object{}, errors.New("size mismatch")
	}
	s.revision++
	etag := fmt.Sprintf("\"%d\"", s.revision)
	s.objects[key] = memoryObject{b, etag}
	if (s.failure == "lost-response") && strings.Contains(string(b), `"state":"Stopped"`) &&
		strings.Contains(string(b), `"checkpoint"`) {
		return Object{}, errors.New("response lost after successful commit")
	}
	if s.failure == "lost-resume-response" && strings.Contains(string(b), `"state":"Running"`) &&
		strings.Contains(string(b), `"owner":"pod-2"`) {
		return Object{}, errors.New("response lost after successful resume")
	}
	return Object{ETag: etag, Size: size}, nil
}

func fixture(t *testing.T) (context.Context, Manager, Session, string) {
	t.Helper()
	ctx := context.Background()
	m := Manager{newMemory()}
	s, e := m.Acquire(ctx, "vm-1", "registry/base@sha256:abc", "pod-1", "node-a")
	require.NoError(t, e)
	file := filepath.Join(t.TempDir(), "overlay")
	require.NoError(t, os.WriteFile(file, []byte("persistent project and installed packages"), 0o600))
	return ctx, m, s, file
}

func TestExclusiveOwnership(t *testing.T) {
	ctx := context.Background()
	m := Manager{newMemory()}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := m.Acquire(ctx, "vm", "base", fmt.Sprint(i), "node")
			if e == nil {
				winners.Add(1)
			} else if !errors.Is(e, ErrOwned) && !errors.Is(e, ErrConflict) {
				t.Errorf("unexpected: %v", e)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("writers=%d", winners.Load())
	}
}

func TestStopRestoreOnAnotherNode(t *testing.T) {
	ctx, m, s, file := fixture(t)
	committed, e := m.Commit(ctx, s, file)
	require.NoError(t, e)
	if committed.Head.State != "Stopped" || committed.Head.Owner != "" || committed.Head.Checkpoint.Generation != 1 {
		t.Fatalf("bad commit %+v", committed)
	}
	next, e := m.Acquire(ctx, s.Head.VMID, s.Head.Base, "pod-2", "node-b")
	require.NoError(t, e)
	restored := filepath.Join(t.TempDir(), "overlay")
	require.NoError(t, m.Restore(ctx, next, restored))
	want, _ := os.ReadFile(file)
	got, _ := os.ReadFile(restored)
	if !bytes.Equal(want, got) {
		t.Fatal("state changed")
	}
	if next.Head.Epoch <= s.Head.Epoch {
		t.Fatal("epoch did not increase")
	}
	if _, e = m.Commit(ctx, s, file); !errors.Is(e, ErrOwned) {
		t.Fatalf("old writer accepted: %v", e)
	}
}

func TestAcquisitionRetryAndBasePin(t *testing.T) {
	ctx, m, s, _ := fixture(t)
	again, e := m.Acquire(ctx, s.Head.VMID, s.Head.Base, s.Head.Owner, s.Head.Node)
	if e != nil || again.ETag != s.ETag {
		t.Fatal("acquisition not idempotent", e)
	}
	if _, e = m.Acquire(ctx, s.Head.VMID, "different-base", s.Head.Owner, s.Head.Node); e == nil {
		t.Fatal("base changed")
	}
	if _, e = m.Acquire(ctx, s.Head.VMID, s.Head.Base, "another-owner", s.Head.Node); !errors.Is(e, ErrOwned) {
		t.Fatal("owner stolen", e)
	}
}

func TestUploadFailureNeverAdvancesHead(t *testing.T) {
	ctx, m, s, file := fixture(t)
	m.Store.(*memoryStore).failure = "upload"
	if _, e := m.Commit(ctx, s, file); e == nil {
		t.Fatal("upload unexpectedly succeeded")
	}
	now, e := m.Read(ctx, s.Head.VMID)
	if e != nil || now.ETag != s.ETag || now.Head.State != "Running" {
		t.Fatal("head advanced", e)
	}
	m.Store.(*memoryStore).failure = ""
	if _, e = m.Commit(ctx, s, file); e != nil {
		t.Fatal("retry failed", e)
	}
}

func TestCorruptUploadNeverCommits(t *testing.T) {
	ctx, m, s, file := fixture(t)
	m.Store.(*memoryStore).failure = "verification"
	if _, e := m.Commit(ctx, s, file); e == nil {
		t.Fatal("corrupt object committed")
	}
	now, _ := m.Read(ctx, s.Head.VMID)
	if now.ETag != s.ETag {
		t.Fatal("head advanced")
	}
}

func TestLostCommitResponse(t *testing.T) {
	ctx, m, s, file := fixture(t)
	m.Store.(*memoryStore).failure = "lost-response"
	committed, e := m.Commit(ctx, s, file)
	if e != nil || committed.Head.State != "Stopped" {
		t.Fatal("lost response not reconciled", e)
	}
}

func TestRestoreCorruptionDoesNotExposeFile(t *testing.T) {
	ctx, m, s, file := fixture(t)
	committed, e := m.Commit(ctx, s, file)
	require.NoError(t, e)
	m.Store.(*memoryStore).failure = "verification"
	path := filepath.Join(t.TempDir(), "overlay")
	require.Error(t, m.Restore(ctx, committed, path), "corruption not detected")
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("unverified file exposed")
	}
	if _, e = os.Stat(path + ".partial"); !os.IsNotExist(e) {
		t.Fatal("partial file leaked")
	}
}

func TestRecoveryFencesOldEpoch(t *testing.T) {
	ctx, m, s, file := fixture(t)
	if _, e := m.Recover(ctx, s.Head.VMID, "wrong-pod"); !errors.Is(e, ErrConflict) {
		t.Fatal("wrong owner recovered", e)
	}
	recovered, e := m.Recover(ctx, s.Head.VMID, s.Head.Owner)
	require.NoError(t, e)
	if recovered.Head.Checkpoint != nil || recovered.Head.Epoch <= s.Head.Epoch {
		t.Fatal("bad recovery")
	}
	if _, e = m.Commit(ctx, s, file); !errors.Is(e, ErrOwned) {
		t.Fatal("stale runtime committed", e)
	}
	if _, e = m.Acquire(ctx, s.Head.VMID, s.Head.Base, "pod-b", "node-b"); e != nil {
		t.Fatal(e)
	}
}

func TestStopReplacesPreviousCheckpoint(t *testing.T) {
	ctx, m, s, file := fixture(t)
	first, e := m.Commit(ctx, s, file)
	require.NoError(t, e)
	original := *first.Head.Checkpoint
	second, e := m.Acquire(ctx, s.Head.VMID, s.Head.Base, "pod-2", "node-b")
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(file, []byte("next version"), 0o600))
	final, e := m.Commit(ctx, second, file)
	require.NoError(t, e)
	if final.Head.Checkpoint.Generation != 2 || final.Head.Checkpoint.Key == original.Key {
		t.Fatal("generation overwritten")
	}
	if _, e = m.Store.Get(ctx, original.Key); !errors.Is(e, ErrNotFound) {
		t.Fatal("previous stop retained", e)
	}
}

func TestResumeLocalPendingAndCompletedUpload(t *testing.T) {
	t.Run("pending upload transfers ownership", func(t *testing.T) {
		ctx, m, stopped, _ := fixture(t)
		resumed, err := m.ResumeLocal(ctx, stopped, "pod-2", "node-a")
		require.NoError(t, err)
		require.Equal(t, stopped.Head.Epoch+1, resumed.Head.Epoch)
		require.Equal(t, "pod-2", resumed.Head.Owner)
		require.Nil(t, resumed.Head.Checkpoint)
	})

	t.Run("completed upload preserves latest checkpoint", func(t *testing.T) {
		ctx, m, stopped, file := fixture(t)
		committed, err := m.Commit(ctx, stopped, file)
		require.NoError(t, err)
		resumed, err := m.ResumeLocal(ctx, stopped, "pod-2", "node-a")
		require.NoError(t, err)
		require.Equal(t, committed.Head.Checkpoint, resumed.Head.Checkpoint)
		require.Equal(t, stopped.Head.Epoch+1, resumed.Head.Epoch)
	})
}

func TestResumeLocalRejectsMismatchedProof(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Session)
		owner  string
		node   string
	}{
		{"node", func(*Session) {}, "pod-2", "node-b"},
		{"base", func(s *Session) { s.Head.Base = "other" }, "pod-2", "node-a"},
		{"old owner", func(s *Session) { s.Head.Owner = "other" }, "pod-2", "node-a"},
		{"epoch", func(s *Session) { s.Head.Epoch++ }, "pod-2", "node-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, m, stopped, _ := fixture(t)
			tt.mutate(&stopped)
			_, err := m.ResumeLocal(ctx, stopped, tt.owner, tt.node)
			require.Error(t, err)
			now, readErr := m.Read(ctx, "vm-1")
			require.NoError(t, readErr)
			require.Equal(t, "pod-1", now.Head.Owner)
		})
	}
}

func TestResumeLocalRecoveryInvalidatesProof(t *testing.T) {
	ctx, m, stopped, _ := fixture(t)
	_, err := m.Recover(ctx, stopped.Head.VMID, stopped.Head.Owner)
	require.NoError(t, err)
	_, err = m.ResumeLocal(ctx, stopped, "pod-2", "node-a")
	require.ErrorIs(t, err, ErrConflict)
}

func TestResumeLocalDuplicateAndLostResponse(t *testing.T) {
	ctx, m, stopped, _ := fixture(t)
	m.Store.(*memoryStore).failure = "lost-resume-response"
	first, err := m.ResumeLocal(ctx, stopped, "pod-2", "node-a")
	require.NoError(t, err)
	second, err := m.ResumeLocal(ctx, stopped, "pod-2", "node-a")
	require.NoError(t, err)
	require.Equal(t, first.ETag, second.ETag)
	_, err = m.ResumeLocal(ctx, stopped, "pod-3", "node-a")
	require.Error(t, err)
}

type blockedCommitStore struct {
	*memoryStore
	reached chan struct{}
	release chan struct{}
}

func (s *blockedCommitStore) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, match string) (Object, error) {
	if key == HeadKey("vm-1") {
		pos, _ := r.Seek(0, io.SeekCurrent)
		body, _ := io.ReadAll(r)
		_, _ = r.Seek(pos, io.SeekStart)
		if strings.Contains(string(body), `"state":"Stopped"`) && strings.Contains(string(body), `"checkpoint"`) {
			close(s.reached)
			select {
			case <-ctx.Done():
				return Object{}, ctx.Err()
			case <-s.release:
			}
		}
	}
	return s.memoryStore.Put(ctx, key, r, size, match)
}

func TestResumeLocalFencesCommitAlreadyUploading(t *testing.T) {
	ctx, base, stopped, file := fixture(t)
	store := &blockedCommitStore{memoryStore: base.Store.(*memoryStore), reached: make(chan struct{}), release: make(chan struct{})}
	m := Manager{Store: store}
	result := make(chan error, 1)
	go func() { _, err := m.Commit(ctx, stopped, file); result <- err }()
	<-store.reached
	resumed, err := m.ResumeLocal(ctx, stopped, "pod-2", "node-a")
	require.NoError(t, err)
	close(store.release)
	require.ErrorIs(t, <-result, ErrConflict)
	now, err := m.Read(ctx, stopped.Head.VMID)
	require.NoError(t, err)
	require.Equal(t, resumed.ETag, now.ETag)
	require.Nil(t, now.Head.Checkpoint, "stale upload published its checkpoint")
}

func TestResumeLocalCompetingOwners(t *testing.T) {
	ctx, m, stopped, _ := fixture(t)
	var winners atomic.Int32
	var wg sync.WaitGroup
	for _, owner := range []string{"pod-2", "pod-3"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.ResumeLocal(ctx, stopped, owner, "node-a"); err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, winners.Load())
}
