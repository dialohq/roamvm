package runner

import (
	"context"
	"time"

	"github.com/dialohq/roamvm/internal/daemon"
	"github.com/dialohq/roamvm/internal/hibernate"
	"github.com/dialohq/roamvm/internal/qmp"
	"github.com/dialohq/roamvm/internal/timing"
)

func wake(ctx context.Context, q qmp.Client, p *daemon.Prepared) error {
	if p.Hibernation == nil {
		return nil
	}
	defer timing.Start("startup.wake")()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// QEMU creates its socket after process launch. No guest CPUs execute on
	// this path until snapshot loading and deletion have completed successfully.
	for {
		if err := q.Call(ctx, "query-status", nil, nil); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if p.Spec.Hibernate {
		if err := q.Snapshot(ctx, "snapshot-load", hibernate.Tag); err != nil {
			return err
		}
	}
	// Keep only the active state, not a growing history of saved RAM and CoW
	// disk references. On the next stop a new coherent snapshot replaces it.
	if err := q.Snapshot(ctx, "snapshot-delete", hibernate.Tag); err != nil {
		return err
	}
	if p.Spec.Hibernate {
		return q.Call(ctx, "cont", nil, nil)
	}
	return nil
}

func nap(q qmp.Client, p *daemon.Prepared) error {
	defer timing.Start("checkpoint.capture")()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.Spec.ShutdownTimeoutSeconds)*time.Second)
	defer cancel()
	identity, err := hibernate.Identity(ctx, p.Spec)
	if err != nil {
		return err
	}
	if err = q.Call(ctx, "stop", nil, nil); err != nil {
		return err
	}
	if err = q.Snapshot(ctx, "snapshot-save", hibernate.Tag); err != nil {
		return err
	}
	// snapshot-save returns the VM to its previous run state: already stopped.
	// Never execute guest code after this capture, including during publication.
	return hibernate.Save(p.Dir, p.PodUID, identity)
}
