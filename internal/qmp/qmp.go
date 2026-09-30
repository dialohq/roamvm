// Package qmp implements the small command subset used by the runner. Each call
// owns its connection so reconnecting never replays an interrupted command.
package qmp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

type Client string

func (c Client) Call(ctx context.Context, command string, arguments any, result any) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", string(c))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
	var greeting struct{ QMP json.RawMessage }
	if err = decoder.Decode(&greeting); err != nil {
		return err
	}
	if len(greeting.QMP) == 0 {
		return fmt.Errorf("missing QMP greeting")
	}
	for id, cmd := range []string{"qmp_capabilities", command} {
		request := map[string]any{"execute": cmd, "id": id}
		if id == 1 && arguments != nil {
			request["arguments"] = arguments
		}
		if err = encoder.Encode(request); err != nil {
			return err
		}
		for {
			var response struct {
				ID     *int                          `json:"id"`
				Return json.RawMessage               `json:"return"`
				Error  *struct{ Class, Desc string } `json:"error"`
				Event  string                        `json:"event"`
			}
			if err = decoder.Decode(&response); err != nil {
				return err
			}
			if response.Event != "" {
				continue
			}
			if response.ID == nil || *response.ID != id {
				return fmt.Errorf("unexpected QMP response ID")
			}
			if response.Error != nil {
				return fmt.Errorf("QMP %s: %s: %s", cmd, response.Error.Class, response.Error.Desc)
			}
			if response.Return == nil {
				return fmt.Errorf("QMP %s: missing return", cmd)
			}
			if id == 1 && result != nil {
				return json.Unmarshal(response.Return, result)
			}
			break
		}
	}
	return nil
}

// Grow is idempotent and never shrinks, including after a lost command response.
func (c Client) Grow(ctx context.Context, size int64) (int64, error) {
	var blocks []struct {
		Inserted struct {
			NodeName string `json:"node-name"`
			Image    struct {
				VirtualSize int64 `json:"virtual-size"`
			} `json:"image"`
		} `json:"inserted"`
	}
	if err := c.Call(ctx, "query-block", nil, &blocks); err != nil {
		return 0, err
	}
	for _, b := range blocks {
		if b.Inserted.NodeName != "root" {
			continue
		}
		current := b.Inserted.Image.VirtualSize
		if current >= size {
			return current, nil
		}
		err := c.Call(ctx, "block_resize", map[string]any{"node-name": "root", "size": size}, nil)
		if err != nil {
			return current, err
		}
		return size, nil
	}
	return 0, fmt.Errorf("QMP root block node not found")
}
