package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dialohq/roamvm/internal/daemon"
	"github.com/dialohq/roamvm/internal/fileio"
	"github.com/dialohq/roamvm/internal/hibernate"
	"github.com/dialohq/roamvm/internal/qmp"
	"github.com/dialohq/roamvm/internal/timing"
)

func privateRAM(p *daemon.Prepared) bool {
	return p.ExternalRAM && (p.Hibernation == nil || p.Hibernation.Layout == hibernate.Private)
}

func wake(ctx context.Context, q qmp.Client, p *daemon.Prepared) error {
	if p.Hibernation == nil && !privateRAM(p) {
		return nil
	}
	if hibernate.HasExternalRAM(p.Hibernation) && !p.Spec.Hibernate {
		return nil // Deliberate disk-only boot; there is no internal snapshot.
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
	if p.Hibernation == nil {
		// Refuse an unpatched hypervisor before executing a new guest, rather
		// than discovering unsupported private checkpoints at its first pause.
		if err := q.OmitRAM(ctx, true); err != nil {
			return err
		}
		return q.Call(ctx, "cont", nil, nil)
	}
	if p.ExternalRAM {
		if err := q.Migrate(ctx, filepath.Join(p.Dir, hibernate.Devices), true, privateRAM(p)); err != nil {
			return err
		}
		return q.Call(ctx, "cont", nil, nil)
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

func nap(q qmp.Client, p *daemon.Prepared, pid int) error {
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
	if p.ExternalRAM {
		// Migration drains and inactivates the block graph, flushing qcow2's
		// metadata before reporting completion. There is no blockdev-flush QMP
		// command; the stopped disk is fsynced again after QEMU exits.
		if err = q.Migrate(ctx, filepath.Join(p.Dir, hibernate.Devices), false, privateRAM(p)); err != nil {
			return err
		}
		paths := []string{filepath.Join(p.Dir, hibernate.Devices)}
		identity.Layout = hibernate.External
		if privateRAM(p) {
			if err = captureRAM(ctx, pid, storagePath(p, hibernate.RAM), filepath.Join(p.Dir, hibernate.RAM)); err != nil {
				return err
			}
			identity.Layout = hibernate.Private
		} else {
			// Sync the actual shared mapping, not just its pager's backing cache.
			paths = append(paths, storagePath(p, hibernate.RAM))
		}
		for _, path := range paths {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			if err = fileio.SyncClose(f, nil); err != nil {
				return err
			}
		}
		return hibernate.Save(p.Dir, p.PodUID, identity)
	}
	if err = q.Snapshot(ctx, "snapshot-save", hibernate.Tag); err != nil {
		return err
	}
	// snapshot-save returns the VM to its previous run state: already stopped.
	// Never execute guest code after this capture, including during publication.
	return hibernate.Save(p.Dir, p.PodUID, identity)
}

// captureRAM reads the stopped child's actual mapping, including private CoW
// pages. The immutable backing file does not contain the guest's modifications.
// QEMU has already drained/inactivated devices; it must never run again afterward.
func captureRAM(ctx context.Context, pid int, source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	root := fmt.Sprintf("/proc/%d/", pid)
	maps, err := os.ReadFile(root + "maps")
	if err != nil {
		return err
	}
	var start int64
	found := false
	for line := range strings.SplitSeq(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 || fields[5] != source {
			continue
		}
		low, high, _ := strings.Cut(fields[0], "-")
		a, ae := strconv.ParseInt(low, 16, 64)
		b, be := strconv.ParseInt(high, 16, 64)
		offset, oe := strconv.ParseInt(fields[2], 16, 64)
		if found || ae != nil || be != nil || oe != nil || a < 0 || b-a != info.Size() || offset != 0 || fields[1] != "rw-p" {
			return fmt.Errorf("unexpected private QEMU RAM mapping")
		}
		start, found = a, true
	}
	if !found {
		return fmt.Errorf("private QEMU RAM mapping not found")
	}
	memory, err := os.Open(root + "mem")
	if err != nil {
		return err
	}
	defer memory.Close()
	stop := context.AfterFunc(ctx, func() { memory.Close() })
	defer stop()
	f, err := os.OpenFile(target+".partial", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(target + ".partial")
	n, err := fileio.CopySparse(f, io.NewSectionReader(memory, start, info.Size()))
	if err == nil && n != info.Size() {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = ctx.Err()
	}
	if err = fileio.SyncClose(f, err); err != nil {
		return err
	}
	if err = os.Rename(target+".partial", target); err != nil {
		return err
	}
	// The new file is complete, not a cache of its old checkpoint ancestry.
	// Leaving lazy metadata would let later materialization overwrite this capture.
	for _, suffix := range []string{".lazy", ".filled"} {
		if err = os.Remove(target + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil // hibernate.Save fsyncs the directory before publishing its marker.
}
