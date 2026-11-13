package state

import (
	"bytes"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/hibernate"
	"github.com/stretchr/testify/require"
)

func TestExternalRAMCheckpointAndReplacement(t *testing.T) {
	ctx, m, session, source := fixture(t)
	m.Shared = true
	ram := bytes.Repeat([]byte{0x87, 0x52, 0x1a}, 1500000)
	devices := []byte("captured CPU and device state")
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(source), hibernate.RAM), ram, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(source), hibernate.Devices), devices, 0o600))
	saved := &api.Hibernation{Layout: hibernate.External}
	session, err := m.Snapshot(ctx, session, source, saved)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "disk")
	require.NoError(t, m.RestoreLazy(ctx, session, path))
	rawRAM := filepath.Join(filepath.Dir(path), hibernate.RAM)
	f, err := OpenLazyFile(m.Store, rawRAM)
	require.NoError(t, err)
	actual := make([]byte, len(ram))
	_, err = f.ReadAt(ctx, actual, 0)
	require.NoError(t, err)
	require.Equal(t, ram, actual)
	_, err = f.WriteAt(ctx, []byte("changed after resume"), 137)
	require.NoError(t, err)
	copy(ram[137:], "changed after resume")
	require.NoError(t, f.Close())
	for _, file := range []string{path, rawRAM} {
		require.NoError(t, m.Materialize(ctx, file))
	}
	session, err = m.Acquire(ctx, session.Head.VMID, session.Head.Base, "resumed", "node-b")
	require.NoError(t, err)
	saved.Layout = hibernate.Private
	session, err = m.Snapshot(ctx, session, path, saved)
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "next-disk")
	require.NoError(t, m.RestoreLazy(ctx, session, destination))
	f, err = OpenLazyFile(m.Store, filepath.Join(filepath.Dir(destination), hibernate.RAM))
	require.NoError(t, err)
	_, err = f.ReadAt(ctx, actual, 0)
	require.NoError(t, err)
	require.Equal(t, ram, actual)
	require.NoError(t, f.Close())
	actual, err = os.ReadFile(filepath.Join(filepath.Dir(destination), hibernate.Devices))
	require.NoError(t, err)
	require.Equal(t, devices, actual)
	require.NoError(t, m.DeleteVM(ctx, session.Head.VMID))
	keys, err := m.Store.List(ctx, "chunks/")
	require.NoError(t, err)
	require.Empty(t, keys, "disk or RAM chunks leaked after the last owner disappeared")
}

func TestLazyCheckpointWritesAndReopen(t *testing.T) {
	ctx, m, session, source := fixture(t)
	m.Shared = true
	want := make([]byte, 12<<20)
	_, err := rand.New(rand.NewSource(47)).Read(want)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(source, want, 0o600))
	session, err = m.Snapshot(ctx, session, source, nil)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "lazy")
	require.NoError(t, m.RestoreLazy(ctx, session, path))
	f, err := OpenLazyFile(m.Store, path)
	require.NoError(t, err)
	require.Zero(t, f.Fetched())
	part := make([]byte, 8193)
	n, err := f.ReadAt(ctx, part, 4101)
	require.NoError(t, err)
	require.Equal(t, len(part), n)
	require.Equal(t, want[4101:4101+len(part)], part)
	require.Greater(t, f.Fetched(), int64(0))
	require.Less(t, f.Fetched(), int64(len(want)), "first read eagerly fetched the entire checkpoint")
	patch := bytes.Repeat([]byte{0xf3, 0x16, 0x80}, 700000)
	const offset = 5<<20 + 131
	_, err = f.WriteAt(ctx, patch, offset)
	require.NoError(t, err)
	copy(want[offset:], patch)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
	f, err = OpenLazyFile(m.Store, path)
	require.NoError(t, err)
	actual := make([]byte, len(patch))
	_, err = f.ReadAt(ctx, actual, offset)
	require.NoError(t, err)
	require.Equal(t, patch, actual)
	require.Zero(t, f.Fetched(), "reopening redownloaded modified chunks")
	require.NoError(t, f.Close())
	require.NoError(t, m.Materialize(ctx, path))
	actual, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, actual)
}

func TestLazyReadRejectsBadChunksAndRecovers(t *testing.T) {
	ctx, m, s, source := fixture(t)
	m.Shared = true
	payload := bytes.Repeat([]byte("verified checkpoint bytes"), 16384)
	require.NoError(t, os.WriteFile(source, payload, 0o600))
	s, err := m.Snapshot(ctx, s, source, nil)
	require.NoError(t, err)
	keys, err := m.Store.List(ctx, "chunks/")
	require.NoError(t, err)
	require.Len(t, keys, 1)
	object, err := m.Store.Get(ctx, keys[0])
	require.NoError(t, err)
	require.NoError(t, object.Body.Close())
	bad := bytes.Repeat([]byte{0xb7}, len(payload))
	_, err = m.Store.Put(ctx, keys[0], bytes.NewReader(bad), int64(len(bad)), object.ETag)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "lazy")
	require.NoError(t, m.RestoreLazy(ctx, s, path))
	f, err := OpenLazyFile(m.Store, path)
	require.NoError(t, err)
	defer f.Close()
	actual := bytes.Repeat([]byte{0xf9}, 8193)
	n, err := f.ReadAt(ctx, actual, 137)
	require.Error(t, err, "corrupted remote data was exposed")
	require.Zero(t, n)
	require.Zero(t, f.Fetched())
	require.Equal(t, bytes.Repeat([]byte{0xf9}, len(actual)), actual)
	require.NoError(t, m.Store.Delete(ctx, keys[0]))
	_, err = f.ReadAt(ctx, actual, 137)
	require.ErrorIs(t, err, ErrNotFound, "a missing chunk silently became zeros")
	require.Zero(t, f.Fetched())
	_, err = m.Store.Put(ctx, keys[0], bytes.NewReader(payload), int64(len(payload)), "")
	require.NoError(t, err)
	_, err = f.ReadAt(ctx, actual, 137)
	require.NoError(t, err)
	require.Equal(t, payload[137:137+len(actual)], actual)
}

func TestLazyTruncateDoesNotResurrectCheckpointBytes(t *testing.T) {
	ctx, m, s, source := fixture(t)
	require.NoError(t, os.WriteFile(source, bytes.Repeat([]byte{0x68}, 8<<20), 0o600))
	s, err := m.Snapshot(ctx, s, source, nil)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "lazy")
	require.NoError(t, m.RestoreLazy(ctx, s, path))
	f, err := OpenLazyFile(m.Store, path)
	require.NoError(t, err)
	const cutoff = 1<<20 + 113
	require.NoError(t, f.Truncate(cutoff))
	require.NoError(t, f.Close())
	f, err = OpenLazyFile(m.Store, path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(8<<20))
	b := make([]byte, 8193)
	_, err = f.ReadAt(ctx, b, cutoff-17)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat([]byte{0x68}, 17), b[:17])
	require.Equal(t, make([]byte, len(b)-17), b[17:])
	n, err := f.ReadAt(ctx, b, 8<<20-1)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 1, n)
	require.Zero(t, b[0])
	require.NoError(t, f.Close())
}
