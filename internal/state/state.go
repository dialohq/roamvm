// Package state implements the durable state machine. Kubernetes status is only
// a projection: the conditionally updated head is authoritative, including ownership.
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
	"os"
	"time"

	api "github.com/dialohq/roamvm/api/v1alpha1"
	"github.com/dialohq/roamvm/internal/fileio"
)

var (
	ErrNotFound = errors.New("object not found")
	ErrConflict = errors.New("conditional write conflict")
	ErrOwned    = errors.New("VM already owned; fence the previous runtime before recovery")
)

type Object struct {
	Body            io.ReadCloser
	ETag, VersionID string
	Size            int64
}
type Store interface {
	Get(context.Context, string) (Object, error)
	// match="" means create-only; otherwise compare the current ETag.
	Put(context.Context, string, io.ReadSeeker, int64, string) (Object, error)
	List(context.Context, string) ([]string, error)
	Delete(context.Context, string) error
}

type Head struct {
	Schema     int             `json:"schema"`
	VMID       string          `json:"vmID"`
	Base       string          `json:"base"`
	Epoch      int64           `json:"epoch"`
	State      string          `json:"state"`
	Owner      string          `json:"owner,omitempty"`
	Node       string          `json:"node,omitempty"`
	Checkpoint *api.Checkpoint `json:"checkpoint,omitempty"`
	UpdatedAt  time.Time       `json:"updatedAt"`
}
type Session struct {
	Head Head
	ETag string
}
type Manager struct{ Store Store }

func HeadKey(id string) string { return "vm/" + id + "/head.json" }

func (m Manager) Read(ctx context.Context, id string) (Session, error) {
	obj, err := m.Store.Get(ctx, HeadKey(id))
	if err != nil {
		return Session{}, err
	}
	defer obj.Body.Close()
	var h Head
	if err = json.NewDecoder(io.LimitReader(obj.Body, 65536)).Decode(&h); err != nil {
		return Session{}, err
	}
	if h.Schema != 1 || h.VMID != id || obj.ETag == "" {
		return Session{}, errors.New("invalid state head")
	}
	return Session{h, obj.ETag}, nil
}

func (m Manager) write(ctx context.Context, h Head, etag string) (Session, error) {
	h.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(h)
	if err != nil {
		return Session{}, err
	}
	obj, err := m.Store.Put(ctx, HeadKey(h.VMID), bytes.NewReader(b), int64(len(b)), etag)
	if err != nil {
		return Session{}, err
	}
	if obj.ETag == "" {
		return Session{}, errors.New("store returned empty ETag")
	}
	return Session{h, obj.ETag}, nil
}

func (m Manager) Acquire(ctx context.Context, id, base, owner, node string) (Session, error) {
	if id == "" || base == "" || owner == "" || node == "" {
		return Session{}, errors.New("missing identity")
	}
	s, err := m.Read(ctx, id)
	if errors.Is(err, ErrNotFound) {
		s, err = m.write(ctx, Head{Schema: 1, VMID: id, Base: base, State: "Stopped"}, "")
		if errors.Is(err, ErrConflict) {
			s, err = m.Read(ctx, id)
		}
	}
	if err != nil {
		return Session{}, err
	}
	if s.Head.Base != base {
		return Session{}, errors.New("base digest does not match durable state")
	}
	if s.Head.Owner != "" {
		if s.Head.Owner == owner && s.Head.Node == node {
			return s, nil
		}
		return Session{}, ErrOwned
	}
	if s.Head.State != "Stopped" {
		return Session{}, errors.New("state is not durably stopped")
	}
	s.Head.Epoch++
	s.Head.Owner = owner
	s.Head.Node = node
	s.Head.State = "Running"
	return m.write(ctx, s.Head, s.ETag)
}

func (m Manager) Check(ctx context.Context, s Session) error {
	now, err := m.Read(ctx, s.Head.VMID)
	if err != nil {
		return err
	}
	if now.ETag != s.ETag || now.Head.Owner != s.Head.Owner || now.Head.Epoch != s.Head.Epoch {
		return ErrOwned
	}
	return nil
}

// Restore verifies the entire immutable object before exposing it to the VMM.
func (m Manager) Restore(ctx context.Context, s Session, path string) error {
	cp := s.Head.Checkpoint
	if cp == nil {
		return nil
	}
	obj, err := m.Store.Get(ctx, cp.Key)
	if err != nil {
		return err
	}
	defer obj.Body.Close()
	if cp.VersionID != "" && obj.VersionID != cp.VersionID {
		return errors.New("checkpoint object version changed")
	}
	f, err := os.OpenFile(path+".partial", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(path + ".partial")
	h := sha256.New()
	n, err := fileio.CopySparse(f, io.TeeReader(io.LimitReader(obj.Body, cp.Size+1), h))
	if err == nil && (n != cp.Size || hex.EncodeToString(h.Sum(nil)) != cp.SHA256) {
		err = errors.New("checkpoint integrity mismatch")
	}
	if err = fileio.SyncClose(f, err); err != nil {
		return err
	}
	return os.Rename(path+".partial", path)
}

// Commit is only called after the VMM has exited and the overlay has been
// checked. The immutable object is read back before the pointer is advanced.
// A lost response is safe to retry: the key includes epoch and content hash.
func (m Manager) Commit(ctx context.Context, s Session, path string) (Session, error) {
	if err := m.Check(ctx, s); err != nil {
		return Session{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Session{}, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return Session{}, err
	}
	hash := hex.EncodeToString(h.Sum(nil))
	generation := int64(1)
	if s.Head.Checkpoint != nil {
		generation = s.Head.Checkpoint.Generation + 1
	}
	key := fmt.Sprintf("vm/%s/overlay/%020d-%020d-%s.qcow2", s.Head.VMID, generation, s.Head.Epoch, hash)
	if _, err = f.Seek(0, 0); err != nil {
		return Session{}, err
	}
	_, err = m.Store.Put(ctx, key, f, size, "")
	if err != nil && !errors.Is(err, ErrConflict) {
		return Session{}, err
	}
	obj, err := m.Store.Get(ctx, key)
	if err != nil {
		return Session{}, err
	}
	verify := sha256.New()
	n, readErr := io.Copy(verify, io.LimitReader(obj.Body, size+1))
	obj.Body.Close()
	if readErr != nil {
		return Session{}, readErr
	}
	if n != size || hex.EncodeToString(verify.Sum(nil)) != hash {
		return Session{}, errors.New("uploaded checkpoint failed verification")
	}
	h2 := s.Head
	h2.Checkpoint = &api.Checkpoint{
		Generation: generation,
		Key:        key,
		SHA256:     hash,
		Size:       size,
		VersionID:  obj.VersionID,
	}
	h2.Owner = ""
	h2.Node = ""
	h2.State = "Stopped"
	out, err := m.write(ctx, h2, s.ETag)
	if err != nil {
		// The CAS may have succeeded while the HTTP response was lost.
		current, e := m.Read(ctx, h2.VMID)
		if e == nil && current.Head.Epoch == h2.Epoch && current.Head.State == "Stopped" &&
			current.Head.Checkpoint != nil &&
			current.Head.Checkpoint.Key == key {
			out, err = current, nil
		}
	}
	if err == nil {
		err = m.Prune(ctx, out)
	}
	return out, err
}

// Recover deliberately requires an externally established fencing decision.
// No timeout ever authorizes a second writer. The old epoch cannot commit.
func (m Manager) Recover(ctx context.Context, id, expectedOwner string) (Session, error) {
	s, err := m.Read(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if expectedOwner == "" || s.Head.Owner != expectedOwner {
		return Session{}, ErrConflict
	}
	s.Head.Epoch++
	s.Head.Owner = ""
	s.Head.Node = ""
	s.Head.State = "Stopped"
	return m.write(ctx, s.Head, s.ETag)
}
