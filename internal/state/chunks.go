package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/fileio"
	"github.com/dialohq/roamvm/internal/hibernate"
	"github.com/dialohq/roamvm/internal/timing"
	"github.com/restic/chunker"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

const (
	chunkSize   = 4 << 20
	maxManifest = 256 << 20
)

type chunk struct {
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	Epoch  int64  `json:"epoch"`
	SHA256 string `json:"sha256"`
	Shared string `json:"shared,omitempty"`
}

type manifest struct {
	Schema  int       `json:"schema"`
	Size    int64     `json:"size"`
	Chunks  []chunk   `json:"chunks"`
	RAM     *manifest `json:"ram,omitempty"`
	Devices []byte    `json:"devices,omitempty"`
}

func (d manifest) files() []manifest {
	files := []manifest{d}
	if d.RAM != nil {
		files = append(files, *d.RAM)
	}
	return files
}

func chunkKey(id string, c chunk) string {
	if c.Shared != "" {
		return sharedKey(c.SHA256, c.Shared)
	}
	return fmt.Sprintf("vm/%s/chunks/%020d-%s", id, c.Epoch, c.SHA256)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (m Manager) verified(ctx context.Context, key, hash string, limit int64) (Object, []byte, error) {
	o, err := m.Store.Get(ctx, key)
	if err != nil {
		return o, nil, err
	}
	defer o.Body.Close()
	b, err := io.ReadAll(io.LimitReader(o.Body, limit+1))
	if err == nil && (int64(len(b)) > limit || sum(b) != hash) {
		err = errors.New("checkpoint integrity mismatch")
	}
	return o, b, err
}

func (m Manager) putVerified(ctx context.Context, key string, b []byte) (Object, error) {
	_, err := m.Store.Put(ctx, key, bytes.NewReader(b), int64(len(b)), "")
	if err != nil && !errors.Is(err, ErrConflict) {
		return Object{}, err
	}
	o, _, err := m.verified(ctx, key, sum(b), int64(len(b)))
	return o, err
}

func (m Manager) readManifest(ctx context.Context, id string, cp *api.Checkpoint) (manifest, error) {
	var out manifest
	epoch, err := checkpointEpoch(cp.Key, id)
	if err != nil {
		return out, err
	}
	o, b, err := m.verified(ctx, cp.Key, cp.SHA256, maxManifest)
	if err != nil {
		return out, err
	}
	if cp.VersionID != "" && cp.VersionID != o.VersionID {
		return out, errors.New("checkpoint object version changed")
	}
	if err = json.Unmarshal(b, &out); err != nil {
		return out, err
	}
	if (out.Schema != 1 && out.Schema != 2) || (out.Schema == 2) != (cp.Format == "chunks-v2") || out.Size != cp.Size || out.Size < 0 || out.Size > 64<<40 {
		return out, errors.New("invalid checkpoint manifest")
	}
	external := hibernate.HasExternalRAM(cp.Hibernation)
	if external != (out.RAM != nil) || len(out.Devices) > 16<<20 || (external && len(out.Devices) == 0) || (!external && len(out.Devices) != 0) {
		return out, errors.New("invalid external RAM/device checkpoint")
	}
	if out.RAM != nil && (out.RAM.RAM != nil || len(out.RAM.Devices) != 0) {
		return out, errors.New("nested RAM checkpoint")
	}
	for _, file := range out.files() {
		if file.Size < 0 || file.Size > 64<<40 || file.Schema != out.Schema {
			return out, errors.New("invalid checkpoint file")
		}
		var end int64
		for _, c := range file.Chunks {
			hash, e := hex.DecodeString(c.SHA256)
			generation, ge := hex.DecodeString(c.Shared)
			if (out.Schema == 1 && c.Shared != "") || (out.Schema == 2 && (ge != nil || len(generation) != 16 || hex.EncodeToString(generation) != c.Shared)) {
				return out, errors.New("invalid shared chunk incarnation")
			}
			if e != nil || len(hash) != sha256.Size || c.SHA256 != hex.EncodeToString(hash) ||
				c.Epoch <= 0 || c.Epoch > epoch || c.Offset < end || c.Offset < 0 || c.Offset >= file.Size ||
				c.Size <= 0 || c.Size > chunkSize || c.Size > file.Size-c.Offset {
				return out, errors.New("invalid checkpoint chunk extent or identity")
			}
			end = c.Offset + c.Size
		}
	}
	return out, nil
}

// Snapshot publishes a sparse, content-addressed disk generation. Private chunks
// reuse only the previous committed manifest. Shared chunks are pinned through
// CAS-protected ownership before their verified blob can enter a manifest.
// The caller holds runner.lock over the stopped file until publication ends.
func (m Manager) Snapshot(ctx context.Context, s Session, path string, saved *api.Hibernation) (Session, error) {
	defer timing.Start("checkpoint.snapshot")()
	if err := m.Check(ctx, s); err != nil {
		return Session{}, err
	}
	previous := map[string]chunk{}
	if cp := s.Head.Checkpoint; cp != nil && cp.Format == "chunks-v1" && !m.Shared {
		old, err := m.readManifest(ctx, s.Head.VMID, cp)
		if err != nil {
			return Session{}, err
		}
		for _, c := range old.Chunks {
			previous[c.SHA256] = c
		}
	}
	description, err := m.capture(ctx, s, path, previous)
	if err != nil {
		return Session{}, err
	}
	if hibernate.HasExternalRAM(saved) {
		ram, err := m.capture(ctx, s, filepath.Join(filepath.Dir(path), hibernate.RAM), nil)
		if err != nil {
			return Session{}, err
		}
		description.RAM = &ram
		f, err := os.Open(filepath.Join(filepath.Dir(path), hibernate.Devices))
		if err != nil {
			return Session{}, err
		}
		description.Devices, err = io.ReadAll(io.LimitReader(f, (16<<20)+1))
		f.Close()
		if err != nil {
			return Session{}, err
		}
		if len(description.Devices) == 0 || len(description.Devices) > 16<<20 {
			return Session{}, errors.New("invalid device-state size")
		}
	}
	b, err := json.Marshal(description)
	if err != nil {
		return Session{}, err
	}
	if len(b) > maxManifest {
		return Session{}, errors.New("checkpoint manifest too large")
	}
	generation := int64(1)
	if s.Head.Checkpoint != nil {
		generation = s.Head.Checkpoint.Generation + 1
	}
	cp := &api.Checkpoint{Generation: generation, Size: description.Size, SHA256: sum(b), Format: "chunks-v1", Hibernation: saved}
	if m.Shared {
		cp.Format = "chunks-v2"
	}
	cp.Key = fmt.Sprintf("vm/%s/overlay/%020d-%020d-%s.json", s.Head.VMID, generation, s.Head.Epoch, cp.SHA256)
	o, err := m.putVerified(ctx, cp.Key, b)
	if err != nil {
		return Session{}, err
	}
	cp.VersionID = o.VersionID
	return m.publishCheckpoint(ctx, s, cp)
}

func (m Manager) capture(ctx context.Context, s Session, path string, previous map[string]chunk) (manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return manifest{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return manifest{}, err
	}
	description := manifest{Schema: 1, Size: info.Size()}
	if m.Shared {
		description.Schema = 2
	}
	sharedResults := map[int64]chunk{}
	var sharedMu sync.Mutex
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	group, uploadCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	zero := make([]byte, chunkSize)
	const polynomial = chunker.Pol(0x3DA3358B4DC173)
	splitter := chunker.New(f, polynomial, chunker.WithBoundaries(256<<10, chunkSize))
	var buffer []byte
	var reused, introduced int64
	scanErr := func() error {
		for offset := int64(0); offset < info.Size(); {
			data, err := unix.Seek(int(f.Fd()), offset, unix.SEEK_DATA)
			if errors.Is(err, unix.ENXIO) {
				break
			}
			if err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTSUP) {
				return err
			}
			end := info.Size()
			if err == nil {
				offset = data
				end, err = unix.Seek(int(f.Fd()), offset, unix.SEEK_HOLE)
				if err != nil {
					return err
				}
			}
			splitter.Reset(io.NewSectionReader(f, offset, end-offset), polynomial, chunker.WithBoundaries(256<<10, chunkSize))
			for {
				if err := uploadCtx.Err(); err != nil {
					return err
				}
				part, err := splitter.Next(buffer)
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				buffer = part.Data
				if bytes.Equal(part.Data, zero[:len(part.Data)]) {
					continue
				}
				c := chunk{Offset: offset + int64(part.Start), Size: int64(part.Length), Epoch: s.Head.Epoch, SHA256: sum(part.Data)}
				if m.Shared {
					b := bytes.Clone(part.Data)
					group.Go(func() error {
						out, err := m.shareChunk(uploadCtx, s, c, b)
						if err == nil {
							sharedMu.Lock()
							sharedResults[c.Offset] = out
							sharedMu.Unlock()
						}
						return err
					})
				} else if old, ok := previous[c.SHA256]; ok && old.Size == c.Size {
					c.Epoch = old.Epoch
					reused += c.Size
				} else {
					introduced += c.Size
					b := bytes.Clone(part.Data)
					group.Go(func() error { _, err := m.putVerified(uploadCtx, chunkKey(s.Head.VMID, c), b); return err })
				}
				description.Chunks = append(description.Chunks, c)
			}
			offset = end
		}
		return nil
	}()
	if scanErr != nil {
		cancel()
	}
	if err = group.Wait(); scanErr != nil {
		return manifest{}, scanErr
	}
	if err != nil {
		return manifest{}, err
	}
	if m.Shared {
		for i := range description.Chunks {
			description.Chunks[i] = sharedResults[description.Chunks[i].Offset]
		}
		log.Printf("checkpoint vm=%s logical_bytes=%d chunks=%d shared=true", s.Head.VMID, info.Size(), len(description.Chunks))
	} else {
		log.Printf("checkpoint vm=%s logical_bytes=%d chunks=%d reused_bytes=%d new_bytes=%d", s.Head.VMID, info.Size(), len(description.Chunks), reused, introduced)
	}
	return description, nil
}

func (m Manager) restoreChunks(ctx context.Context, s Session, path string) error {
	description, err := m.readManifest(ctx, s.Head.VMID, s.Head.Checkpoint)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".partial", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(path + ".partial")
	err = f.Truncate(description.Size)
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	if err == nil {
		for _, c := range description.Chunks {
			group.Go(func() error {
				_, b, err := m.verified(ctx, chunkKey(s.Head.VMID, c), c.SHA256, c.Size)
				if err == nil && int64(len(b)) != c.Size {
					err = errors.New("checkpoint chunk length mismatch")
				}
				if err != nil {
					return err
				}
				_, err = f.WriteAt(b, c.Offset)
				return err
			})
		}
		err = group.Wait()
	}
	if err = fileio.SyncClose(f, err); err != nil {
		return err
	}
	return os.Rename(path+".partial", path)
}

var chunkObject = regexp.MustCompile(`^vm/([^/]+)/chunks/([0-9]{20})-[0-9a-f]{64}$`)

func (m Manager) pruneChunks(ctx context.Context, s Session, epoch int64) error {
	keep := map[string]bool{}
	if s.Head.Checkpoint.Format == "chunks-v1" {
		description, err := m.readManifest(ctx, s.Head.VMID, s.Head.Checkpoint)
		if errors.Is(err, ErrNotFound) {
			// A newer collector already removed this generation. Without its
			// references, skip chunk deletion rather than guess what to retain.
			return nil
		}
		if err != nil {
			return err
		}
		for _, file := range description.files() {
			for _, c := range file.Chunks {
				keep[chunkKey(s.Head.VMID, c)] = true
			}
		}
	}
	keys, err := m.Store.List(ctx, "vm/"+s.Head.VMID+"/chunks/")
	if err != nil {
		return err
	}
	for _, key := range keys {
		parts := chunkObject.FindStringSubmatch(key)
		if parts == nil || parts[1] != s.Head.VMID || keep[key] {
			continue
		}
		born, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return err
		}
		if born <= epoch {
			if err = m.Store.Delete(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}
