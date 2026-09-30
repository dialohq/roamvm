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
	return Object{ETag: etag, Size: size}, nil
}

func fixture(t *testing.T) (context.Context, Manager, Session, string) {
	t.Helper()
	ctx := context.Background()
	m := Manager{newMemory()}
	s, e := m.Acquire(ctx, "vm-1", "registry/base@sha256:abc", "pod-1", "node-a")
	if e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(t.TempDir(), "overlay")
	if e = os.WriteFile(file, []byte("persistent project and installed packages"), 0o600); e != nil {
		t.Fatal(e)
	}
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
	if e != nil {
		t.Fatal(e)
	}
	if committed.Head.State != "Stopped" || committed.Head.Owner != "" || committed.Head.Checkpoint.Generation != 1 {
		t.Fatalf("bad commit %+v", committed)
	}
	next, e := m.Acquire(ctx, s.Head.VMID, s.Head.Base, "pod-2", "node-b")
	if e != nil {
		t.Fatal(e)
	}
	restored := filepath.Join(t.TempDir(), "overlay")
	if e = m.Restore(ctx, next, restored); e != nil {
		t.Fatal(e)
	}
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
	if e != nil {
		t.Fatal(e)
	}
	m.Store.(*memoryStore).failure = "verification"
	path := filepath.Join(t.TempDir(), "overlay")
	if e = m.Restore(ctx, committed, path); e == nil {
		t.Fatal("corruption not detected")
	}
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
	if e != nil {
		t.Fatal(e)
	}
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
	if e != nil {
		t.Fatal(e)
	}
	original := *first.Head.Checkpoint
	second, e := m.Acquire(ctx, s.Head.VMID, s.Head.Base, "pod-2", "node-b")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(file, []byte("next version"), 0o600); e != nil {
		t.Fatal(e)
	}
	final, e := m.Commit(ctx, second, file)
	if e != nil {
		t.Fatal(e)
	}
	if final.Head.Checkpoint.Generation != 2 || final.Head.Checkpoint.Key == original.Key {
		t.Fatal("generation overwritten")
	}
	if _, e = m.Store.Get(ctx, original.Key); !errors.Is(e, ErrNotFound) {
		t.Fatal("previous stop retained", e)
	}
}
