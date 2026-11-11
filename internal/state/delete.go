package state

import (
	"context"
	"errors"
	"strings"
)

// DeleteVM is called only after the deleting CR has no runners or checkpoint
// workers. The tombstone fences delayed acquisitions/publications during retries.
// The caller must retain the CR's finalizer until this returns successfully.
func (m Manager) DeleteVM(ctx context.Context, id string) error {
	if id == "" || strings.Contains(id, "/") {
		return errors.New("invalid VM identity")
	}
	s, err := m.Read(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil && s.Head.Owner != "" {
		return ErrOwned
	}
	if errors.Is(err, ErrNotFound) {
		s.Head = Head{Schema: 1, VMID: id}
	}
	if s.Head.State != "Deleting" {
		s.Head.State = "Deleting"
		s.Head.Epoch++
		if _, err = m.write(ctx, s.Head, s.ETag); err != nil {
			return err
		}
	}
	return purgePrefix(ctx, m.Store, "vm/"+id+"/")
}

func purgePrefix(ctx context.Context, store Store, prefix string) error {
	if p, ok := store.(interface {
		PurgePrefix(context.Context, string) error
	}); ok {
		return p.PurgePrefix(ctx, prefix)
	}
	return deleteListed(ctx, store, prefix)
}

func deleteListed(ctx context.Context, store Store, prefix string) error {
	keys, err := store.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if key != prefix+"head.json" {
			if err = store.Delete(ctx, key); err != nil {
				return err
			}
		}
	}
	return store.Delete(ctx, prefix+"head.json")
}
