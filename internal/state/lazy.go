package state

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/dialohq/roamvm/internal/fileio"
	"github.com/dialohq/roamvm/internal/hibernate"
)

// LazyFile is a persistent writable view over an immutable checkpoint. A filled
// byte is durable only after its original chunk is durable locally. Writes first
// materialize overlapping chunks, so later faults cannot overwrite modifications.
// The caller holds runner.lock for the lifetime of the view.
type LazyFile struct {
	mu      sync.Mutex
	store   Store
	file    *os.File
	index   *os.File
	meta    lazyMetadata
	filled  []byte
	fetched int64
}

type lazyMetadata struct {
	VMID     string   `json:"vmID"`
	Manifest manifest `json:"manifest"`
	Limit    int64    `json:"limit"`
}

// RestoreLazy authenticates the manifest but defers chunk downloads until I/O.
// Legacy whole-file checkpoints retain their eager restore path.
func (m Manager) RestoreLazy(ctx context.Context, s Session, path string) error {
	if cp := s.Head.Checkpoint; cp == nil || (cp.Format != "chunks-v1" && cp.Format != "chunks-v2") {
		return m.Restore(ctx, s, path)
	}
	description, err := m.readManifest(ctx, s.Head.VMID, s.Head.Checkpoint)
	if err != nil {
		return err
	}
	if description.RAM != nil {
		if err := m.createLazy(ctx, s.Head.VMID, filepath.Join(filepath.Dir(path), hibernate.RAM), *description.RAM); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(filepath.Dir(path), hibernate.Devices), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(description.Devices)
		if err = fileio.SyncClose(f, err); err != nil {
			return err
		}
	}
	description.RAM, description.Devices = nil, nil
	return m.createLazy(ctx, s.Head.VMID, path, description)
}

func (m Manager) createLazy(ctx context.Context, id, path string, description manifest) error {
	// No view has been exposed yet. Failed initializations can be replaced.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err = fileio.SyncClose(f, f.Truncate(description.Size)); err != nil {
		return err
	}
	f, err = os.OpenFile(path+".filled", os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err = fileio.SyncClose(f, f.Truncate(int64(len(description.Chunks)))); err != nil {
		return err
	}
	return writeLazyMetadata(path, lazyMetadata{VMID: id, Manifest: description, Limit: description.Size})
}

func writeLazyMetadata(path string, meta lazyMetadata) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lazy.partial", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err = fileio.SyncClose(f, err); err != nil {
		return err
	}
	if err = os.Rename(path+".lazy.partial", path+".lazy"); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return fileio.SyncClose(dir, nil)
}

// OpenLazyFile reopens a view without downloading any checkpoint data. The
// metadata was authenticated when the checkpoint was acquired and initialized.
func OpenLazyFile(store Store, path string) (*LazyFile, error) {
	b, err := os.ReadFile(path + ".lazy")
	if err != nil {
		return nil, err
	}
	var meta lazyMetadata
	if err = json.Unmarshal(b, &meta); err != nil {
		return nil, err
	}
	filled, err := os.ReadFile(path + ".filled")
	if err != nil {
		return nil, err
	}
	if meta.Limit < 0 || meta.Limit > meta.Manifest.Size || len(filled) != len(meta.Manifest.Chunks) {
		return nil, errors.New("invalid lazy checkpoint cache")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	index, err := os.OpenFile(path+".filled", os.O_RDWR, 0)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &LazyFile{store: store, file: f, index: index, meta: meta, filled: filled}, nil
}

func (f *LazyFile) load(ctx context.Context, offset, end int64) error {
	chunks := f.meta.Manifest.Chunks
	start := sort.Search(len(chunks), func(i int) bool { return chunks[i].Offset+chunks[i].Size > offset })
	for i := start; i < len(chunks) && chunks[i].Offset < min(end, f.meta.Limit); i++ {
		if f.filled[i] != 0 {
			continue
		}
		c := chunks[i]
		_, b, err := (Manager{Store: f.store}).verified(ctx, chunkKey(f.meta.VMID, c), c.SHA256, c.Size)
		if err != nil {
			return err
		}
		if int64(len(b)) != c.Size {
			return errors.New("lazy checkpoint chunk length mismatch")
		}
		if _, err = f.file.WriteAt(b[:min(c.Size, f.meta.Limit-c.Offset)], c.Offset); err != nil {
			return err
		}
		if err = f.file.Sync(); err != nil {
			return err
		}
		if _, err = f.index.WriteAt([]byte{1}, int64(i)); err != nil {
			return err
		}
		if err = f.index.Sync(); err != nil {
			return err
		}
		f.filled[i] = 1
		f.fetched += c.Size
	}
	return nil
}

func (f *LazyFile) ReadAt(ctx context.Context, b []byte, offset int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if offset < 0 || int64(len(b)) > 1<<63-1-offset {
		return 0, errors.New("invalid lazy read range")
	}
	info, err := f.file.Stat()
	if err != nil {
		return 0, err
	}
	if err = f.load(ctx, offset, min(offset+int64(len(b)), info.Size())); err != nil {
		return 0, err
	}
	return f.file.ReadAt(b, offset)
}

func (f *LazyFile) WriteAt(ctx context.Context, b []byte, offset int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if offset < 0 || int64(len(b)) > 1<<63-1-offset {
		return 0, errors.New("invalid lazy write range")
	}
	if err := f.load(ctx, offset, offset+int64(len(b))); err != nil {
		return 0, err
	}
	return f.file.WriteAt(b, offset)
}

func (f *LazyFile) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if size < 0 {
		return errors.New("invalid lazy file size")
	}
	// Persist the cutoff before truncation. Retrying an interrupted shrink can
	// lose unacknowledged bytes but must never resurrect discarded checkpoint data.
	if size < f.meta.Limit {
		meta := f.meta
		meta.Limit = size
		if err := writeLazyMetadata(f.file.Name(), meta); err != nil {
			return err
		}
		f.meta = meta
	}
	return f.file.Truncate(size)
}

func (f *LazyFile) Stat() (os.FileInfo, error) { return f.file.Stat() }
func (f *LazyFile) Sync() error                { return f.file.Sync() }

func (f *LazyFile) Fetched() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetched
}

func (f *LazyFile) Close() error {
	return errors.Join(f.file.Close(), f.index.Close())
}

// Materialize finishes an inactive view before snapshot publication can retire
// its original backing references. Never call this beside a live FUSE mapping.
func (m Manager) Materialize(ctx context.Context, path string) error {
	_, err := os.Stat(path + ".lazy")
	if os.IsNotExist(err) {
		return nil
	}
	f, err := OpenLazyFile(m.Store, path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.load(ctx, 0, f.meta.Limit); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = os.Remove(path + ".lazy"); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err = fileio.SyncClose(dir, nil); err != nil {
		return err
	}
	return os.Remove(path + ".filled")
}

// ChunkReader limits a pager to the immutable data in this VM's checkpoint.
// The closure retains the authenticated manifest, not arbitrary bucket access.
func (m Manager) ChunkReader(ctx context.Context, s Session) (func(context.Context, string) ([]byte, error), error) {
	description, err := m.readManifest(ctx, s.Head.VMID, s.Head.Checkpoint)
	if err != nil {
		return nil, err
	}
	allowed := map[string]chunk{}
	for _, file := range description.files() {
		for _, c := range file.Chunks {
			allowed[chunkKey(s.Head.VMID, c)] = c
		}
	}
	return func(ctx context.Context, key string) ([]byte, error) {
		c, ok := allowed[key]
		if !ok {
			return nil, ErrNotFound
		}
		_, b, err := m.verified(ctx, key, c.SHA256, c.Size)
		if err == nil && int64(len(b)) != c.Size {
			err = io.ErrUnexpectedEOF
		}
		return b, err
	}, nil
}
