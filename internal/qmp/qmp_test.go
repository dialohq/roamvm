package qmp

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProtocol(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "events-and-command-error", true: "wrong-response-id"}[malformed], func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "qmp")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
				enc.Encode(map[string]any{"QMP": map[string]any{}})
				var req map[string]any
				dec.Decode(&req)
				enc.Encode(map[string]any{"event": "RESUME"})
				enc.Encode(map[string]any{"return": map[string]any{}, "id": req["id"]})
				dec.Decode(&req)
				if malformed {
					req["id"] = 99
				}
				enc.Encode(map[string]any{"error": map[string]string{"class": "GenericError", "desc": "resize denied"}, "id": req["id"]})
			}()
			err = Client(socket).Call(t.Context(), "block_resize", nil, nil)
			expected := "resize denied"
			if malformed {
				expected = "response ID"
			}
			if err == nil || !strings.Contains(err.Error(), expected) {
				t.Fatalf("got %v, want %s", err, expected)
			}
		})
	}
}

func TestLiveBlockGrowth(t *testing.T) {
	binary, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		t.Skip("QEMU system binary is not installed")
	}
	dir := t.TempDir()
	base, overlay, socket := filepath.Join(dir, "base.raw"), filepath.Join(dir, "overlay.qcow2"), filepath.Join(dir, "qmp")
	if err = os.WriteFile(base, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", "-F", "raw", "-b", base, overlay).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	logFile, err := os.Create(filepath.Join(dir, "qemu.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(binary, "-machine", "q35,accel=tcg", "-m", "32", "-S", "-nodefaults", "-display", "none", "-qmp", "unix:"+socket+",server=on,wait=off", "-blockdev", `{"driver":"qcow2","node-name":"root","file":{"driver":"file","filename":"`+overlay+`"}}`, "-device", "virtio-blk-pci,drive=root")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	q := Client(socket)
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = q.Call(t.Context(), "query-status", nil, nil)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := os.ReadFile(logFile.Name())
			t.Fatalf("QMP: %v\n%s", err, logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, target := range []int64{2 << 20, 2 << 20, 1 << 20, 4 << 20} {
		size, err := q.Grow(t.Context(), target)
		expected := max(target, 2<<20)
		if err != nil || size != int64(expected) {
			t.Fatalf("grow %d: got %d, %v", target, size, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := q.Call(ctx, "query-status", nil, nil); err == nil {
		t.Fatal("cancelled call succeeded")
	}
	if err = q.Call(t.Context(), "quit", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("qemu-img", "info", "--output=json", overlay).Output()
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Size int64 `json:"virtual-size"`
	}
	json.Unmarshal(out, &info)
	if info.Size != 4<<20 {
		t.Fatalf("growth not durable: %s", out)
	}
}
