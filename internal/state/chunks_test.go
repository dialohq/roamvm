package state

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSparseSnapshotsReuseChunksAndRestoreHoles(t *testing.T) {
	ctx, m, s, path := fixture(t)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	const size = int64(64<<30) + 137
	require.NoError(t, f.Truncate(size))
	_, err = f.WriteAt([]byte("head sentinel"), 71)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("tail sentinel"), size-37)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	first, err := m.Snapshot(ctx, s, path, nil)
	require.NoError(t, err)
	before, err := m.Store.List(ctx, "vm/vm-1/chunks/")
	require.NoError(t, err)
	require.Len(t, before, 2, "holes must not become remote objects")
	next, err := m.Acquire(ctx, s.Head.VMID, s.Head.Base, "two", "node-b")
	require.NoError(t, err)
	f, err = os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("new head"), 71)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	second, err := m.Snapshot(ctx, next, path, nil)
	require.NoError(t, err)
	require.NoError(t, m.Prune(ctx, first), "already collected generations must be safe to collect again")
	after, err := m.Store.List(ctx, "vm/vm-1/chunks/")
	require.NoError(t, err)
	require.Len(t, after, 2)
	common := 0
	for _, key := range after {
		for _, old := range before {
			if key == old {
				common++
			}
		}
	}
	require.Equal(t, 1, common, "only the changed chunk should be replaced")
	require.EqualValues(t, 2, second.Head.Checkpoint.Generation)
	require.NotEqual(t, first.Head.Checkpoint.Key, second.Head.Checkpoint.Key)
	output := filepath.Join(t.TempDir(), "restored")
	require.NoError(t, m.Restore(ctx, second, output))
	f, err = os.Open(output)
	require.NoError(t, err)
	defer f.Close()
	info, err := f.Stat()
	require.NoError(t, err)
	require.Equal(t, size, info.Size())
	for _, probe := range []struct {
		offset int64
		want   []byte
	}{
		{71, []byte("new headtinel")},
		{32 << 30, make([]byte, 111)},
		{size - 37, []byte("tail sentinel")},
		{size - 24, make([]byte, 24)},
	} {
		got := make([]byte, len(probe.want))
		_, err = f.ReadAt(got, probe.offset)
		require.NoError(t, err)
		require.Equal(t, probe.want, got)
	}
}

func TestShiftedSnapshotPayloadReusesStoredChunks(t *testing.T) {
	ctx, m, s, path := fixture(t)
	payload := make([]byte, 32<<20)
	_, err := rand.New(rand.NewSource(471)).Read(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, payload, 0o600))
	_, err = m.Snapshot(ctx, s, path, nil)
	require.NoError(t, err)
	keys, err := m.Store.List(ctx, "vm/vm-1/chunks/")
	require.NoError(t, err)
	previous := map[string]bool{}
	for _, key := range keys {
		previous[key] = true
	}
	next, err := m.Acquire(ctx, s.Head.VMID, s.Head.Base, "two", "node-b")
	require.NoError(t, err)
	// Snapshot serialization can insert a small header before otherwise
	// unchanged RAM. Fixed-offset blocks would retransmit the entire payload.
	want := append(bytes.Repeat([]byte{0x73}, 137), payload...)
	require.NoError(t, os.WriteFile(path, want, 0o600))
	next, err = m.Snapshot(ctx, next, path, nil)
	require.NoError(t, err)
	keys, err = m.Store.List(ctx, "vm/vm-1/chunks/")
	require.NoError(t, err)
	var reused int64
	for _, key := range keys {
		if previous[key] {
			o, err := m.Store.Get(ctx, key)
			require.NoError(t, err)
			reused += o.Size
			require.NoError(t, o.Body.Close())
		}
	}
	require.Greater(t, reused, int64(len(payload)/2), "shifting unchanged content must not force a full upload")
	output := filepath.Join(t.TempDir(), "restored")
	require.NoError(t, m.Restore(ctx, next, output))
	got, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestChunkCorruptionNeverExposesRestore(t *testing.T) {
	ctx, m, s, path := fixture(t)
	committed, err := m.Snapshot(ctx, s, path, nil)
	require.NoError(t, err)
	keys, err := m.Store.List(ctx, "vm/vm-1/chunks/")
	require.NoError(t, err)
	require.Len(t, keys, 1)
	store := m.Store.(*memoryStore)
	store.objects[keys[0]] = memoryObject{body: []byte("corrupted")}
	output := filepath.Join(t.TempDir(), "restored")
	require.NoError(t, os.WriteFile(output, []byte("previous valid disk"), 0o600))
	require.Error(t, m.Restore(ctx, committed, output))
	b, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, "previous valid disk", string(b))
	require.NoFileExists(t, output+".partial")
}

func TestDelayedChunkGCDoesNotResurrectOldReferences(t *testing.T) {
	ctx, base, s, path := fixture(t)
	objects := base.Store.(*memoryStore)
	paused := &pausedDelete{Store: objects, entered: make(chan struct{}), resume: make(chan struct{})}
	m := Manager{Store: paused}
	a, b, c := bytes.Repeat([]byte{0x31}, 4<<20), bytes.Repeat([]byte{0x72}, 4<<20), bytes.Repeat([]byte{0x93}, 4<<20)
	commit := func(s Session, second []byte) Session {
		t.Helper()
		require.NoError(t, os.WriteFile(path, append(append([]byte(nil), a...), second...), 0o600))
		out, err := m.Snapshot(ctx, s, path, nil)
		require.NoError(t, err)
		return out
	}
	commit(s, b)
	second, err := m.Acquire(ctx, s.Head.VMID, s.Head.Base, "two", "node-b")
	require.NoError(t, err)
	second = commit(second, c)
	hash := sha256.Sum256(b)
	paused.key = fmt.Sprintf("vm/vm-1/chunks/%020d-%x", s.Head.Epoch, hash)
	_, err = objects.Put(ctx, paused.key, bytes.NewReader(b), int64(len(b)), "")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- m.Prune(ctx, second) }()
	<-paused.entered
	third, err := m.Acquire(ctx, s.Head.VMID, s.Head.Base, "three", "node-c")
	require.NoError(t, err)
	third = commit(third, b)
	close(paused.resume)
	require.NoError(t, <-done)
	output := filepath.Join(t.TempDir(), "restored")
	require.NoError(t, m.Restore(ctx, third, output))
	got, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, append(a, b...), got)
	keys, err := m.Store.List(ctx, "vm/vm-1/chunks/")
	require.NoError(t, err)
	require.Len(t, keys, 2, "GC must retain only the newest checkpoint's chunks")
}
