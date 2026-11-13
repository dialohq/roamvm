package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

// Each hash has a CAS-protected set of VM/epoch references. Retiring the last
// reference rotates the blob incarnation before deletion, preventing delayed
// collection from deleting identical content introduced by a later writer.
type sharedChunk struct {
	Schema     int              `json:"schema"`
	Generation string           `json:"generation"`
	Size       int64            `json:"size"`
	Ready      bool             `json:"ready"`
	References map[string]int64 `json:"references"`
}

func sharedHead(hash string) string            { return "shared/" + hash + "/head.json" }
func sharedKey(hash, generation string) string { return "chunks/" + hash + "/" + generation }
func referenceKey(id string, epoch int64, hash, generation string) string {
	return fmt.Sprintf("vm/%s/refs/%020d-%s-%s", id, epoch, generation, hash)
}

func (m Manager) readShared(ctx context.Context, hash string) (sharedChunk, string, error) {
	var out sharedChunk
	o, err := m.Store.Get(ctx, sharedHead(hash))
	if errors.Is(err, ErrNotFound) {
		return sharedChunk{Schema: 1, References: map[string]int64{}}, "", nil
	}
	if err != nil {
		return out, "", err
	}
	defer o.Body.Close()
	if err := json.NewDecoder(io.LimitReader(o.Body, 65537)).Decode(&out); err != nil {
		return out, "", err
	}
	if out.Schema != 1 || out.References == nil || o.ETag == "" || out.Size < 0 || out.Size > chunkSize {
		return out, "", errors.New("invalid shared chunk ownership")
	}
	return out, o.ETag, nil
}

func (m Manager) writeShared(ctx context.Context, hash string, record sharedChunk, etag string) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(b) > 65536 {
		return errors.New("shared chunk reference record exceeds metadata capacity")
	}
	_, err = m.Store.Put(ctx, sharedHead(hash), bytes.NewReader(b), int64(len(b)), etag)
	return err
}

func (m Manager) shareChunk(ctx context.Context, s Session, c chunk, data []byte) (chunk, error) {
	for {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		record, etag, err := m.readShared(ctx, c.SHA256)
		if err != nil {
			return c, err
		}
		if len(record.References) == 0 {
			var generation [16]byte
			if _, err := rand.Read(generation[:]); err != nil {
				return c, err
			}
			record.Generation, record.Size, record.Ready = hex.EncodeToString(generation[:]), c.Size, false
		}
		if record.Size != c.Size {
			return c, errors.New("shared chunk length mismatch")
		}
		c.Shared = record.Generation
		// The intent precedes the reference CAS, so failed publication and lost
		// replies remain discoverable by VM deletion without a bucket-wide scan.
		key := referenceKey(s.Head.VMID, s.Head.Epoch, c.SHA256, c.Shared)
		_, err = m.Store.Put(ctx, key, bytes.NewReader(nil), 0, "")
		if err != nil && !errors.Is(err, ErrConflict) {
			return c, err
		}
		if record.References[s.Head.VMID] < s.Head.Epoch {
			record.References[s.Head.VMID] = s.Head.Epoch
			if err = m.writeShared(ctx, c.SHA256, record, etag); errors.Is(err, ErrConflict) {
				continue
			}
			if err != nil {
				return c, err
			}
		}
		if record.Ready {
			return c, nil
		}
		if _, err = m.putVerified(ctx, sharedKey(c.SHA256, c.Shared), data); err != nil {
			return c, err
		}
		for {
			record, etag, err = m.readShared(ctx, c.SHA256)
			if err != nil {
				return c, err
			}
			if record.Generation != c.Shared || record.References[s.Head.VMID] < s.Head.Epoch {
				return c, ErrConflict
			}
			if record.Ready {
				return c, nil
			}
			record.Ready = true
			if err = m.writeShared(ctx, c.SHA256, record, etag); errors.Is(err, ErrConflict) {
				continue
			}
			return c, err
		}
	}
}

func (m Manager) releaseChunk(ctx context.Context, id string, epoch int64, hash, generation string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, etag, err := m.readShared(ctx, hash)
		if err != nil {
			return err
		}
		if record.Generation != generation {
			break
		}
		if ref, ok := record.References[id]; ok && ref <= epoch {
			delete(record.References, id)
		} else if len(record.References) > 0 {
			return nil
		}
		if len(record.References) == 0 {
			record.Ready = false
		}
		if err = m.writeShared(ctx, hash, record, etag); errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		if len(record.References) > 0 {
			return nil
		}
		break
	}
	return m.Store.Delete(ctx, sharedKey(hash, generation))
}

var sharedReference = regexp.MustCompile(`^vm/([^/]+)/refs/([0-9]{20})-([0-9a-f]{32})-([0-9a-f]{64})$`)

func (m Manager) pruneShared(ctx context.Context, s Session, deleting bool) error {
	keep := map[string]bool{}
	epoch := s.Head.Epoch
	if !deleting {
		if s.Head.Checkpoint == nil {
			return nil
		}
		var err error
		epoch, err = checkpointEpoch(s.Head.Checkpoint.Key, s.Head.VMID)
		if err != nil {
			return err
		}
		if s.Head.Checkpoint.Format == "chunks-v2" {
			description, err := m.readManifest(ctx, s.Head.VMID, s.Head.Checkpoint)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			for _, file := range description.files() {
				for _, c := range file.Chunks {
					keep[c.SHA256+"/"+c.Shared] = true
				}
			}
		}
	}
	keys, err := m.Store.List(ctx, "vm/"+s.Head.VMID+"/refs/")
	if err != nil {
		return err
	}
	for _, key := range keys {
		parts := sharedReference.FindStringSubmatch(key)
		if parts == nil || parts[1] != s.Head.VMID {
			continue
		}
		born, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return err
		}
		if born > epoch {
			continue
		}
		retained := keep[parts[4]+"/"+parts[3]]
		if !retained {
			if err := m.releaseChunk(ctx, s.Head.VMID, born, parts[4], parts[3]); err != nil {
				return err
			}
		}
		if deleting || !retained || born < epoch {
			if err := m.Store.Delete(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}
