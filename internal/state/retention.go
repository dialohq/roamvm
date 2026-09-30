package state

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

var checkpointKey = regexp.MustCompile(`^vm/([^/]+)/overlay/[0-9]{20}-([0-9]{20})-[0-9a-f]{64}\.qcow2$`)

func checkpointEpoch(key, vmID string) (int64, error) {
	parts := checkpointKey.FindStringSubmatch(key)
	if parts == nil || parts[1] != vmID {
		return 0, errors.New("invalid checkpoint key")
	}
	return strconv.ParseInt(parts[2], 10, 64)
}

// Prune keeps the committed checkpoint and newer uploads. Epochs never recur,
// so cleanup delayed across a subsequent start cannot delete its checkpoint.
// Call only with a session read from the durable head or returned by its CAS.
func (m Manager) Prune(ctx context.Context, committed Session) error {
	cp := committed.Head.Checkpoint
	if cp == nil {
		return nil
	}
	epoch, err := checkpointEpoch(cp.Key, committed.Head.VMID)
	if err != nil {
		return err
	}
	keys, err := m.Store.List(ctx, "vm/"+committed.Head.VMID+"/overlay/")
	if err != nil {
		return fmt.Errorf("list superseded checkpoints: %w", err)
	}
	for _, key := range keys {
		candidate, err := checkpointEpoch(key, committed.Head.VMID)
		if err != nil || candidate > epoch || key == cp.Key {
			continue
		}
		if err = m.Store.Delete(ctx, key); err != nil {
			return fmt.Errorf("delete superseded checkpoint: %w", err)
		}
	}
	return nil
}
