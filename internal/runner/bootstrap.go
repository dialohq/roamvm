package runner

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"time"
)

type BootstrapConfig struct {
	Token, AMPURL string
	NixBuilders   *NixBuilders `json:",omitempty"`
}

type NixBuilders struct {
	Machines, SSHPrivateKey, KnownHosts string
}

// Bootstrap sends credentials over the runner's private QGA channel. Tokens
// arrive on stdin, not in kubectl/Pod command arguments or Kubernetes Secrets.
func Bootstrap(ctx context.Context, socket string, config BootstrapConfig) error {
	if config.Token == "" {
		return fmt.Errorf("ORB_TOKEN is required")
	}
	// /run is guest tmpfs: keys must not enter disk-only project templates.
	// Refresh after every wake without interrupting unchanged, live builds.
	setup := map[string]any{
		"path": "/bin/sh", "arg": []string{"-c", `set -eu
cd /run/roamvm-builder 2>/dev/null || exit 0
if [ -f nix.conf ] || [ -f restart.pending ]; then
    touch restart.pending
    rm -f nix.conf machines id_ed25519 known_hosts
    systemctl try-restart nix-daemon.service
    rm -f restart.pending
fi`},
	}
	if b := config.NixBuilders; b != nil {
		if b.Machines == "" || b.SSHPrivateKey == "" || b.KnownHosts == "" {
			return fmt.Errorf("remote builders require Machines, SSHPrivateKey and KnownHosts")
		}
		data, err := json.Marshal(b)
		if err != nil {
			return err
		}
		setup = map[string]any{
			"path": "/run/current-system/sw/bin/python3", "arg": []string{"-c", `import json, os, pathlib, subprocess, sys
if "!include /run/roamvm-builder/nix.conf" not in pathlib.Path("/etc/nix/nix.conf").read_text():
    raise RuntimeError("guest lacks the remote-builders NixOS module")
b = json.load(sys.stdin)
os.umask(0o077)
directory = pathlib.Path("/run/roamvm-builder")
directory.mkdir(mode=0o700, exist_ok=True)
directory.chmod(0o755)
pending = directory / "restart.pending"
files = {"id_ed25519": b["SSHPrivateKey"], "known_hosts": b["KnownHosts"], "machines": b["Machines"],
         "nix.conf": "max-jobs = 0\nbuilders = @/run/roamvm-builder/machines\nbuilders-use-substitutes = true\n"}
for name, value in files.items():
    path = directory / name
    if path.exists() and path.read_text() == value + "\n":
        continue
    pending.touch()
    temporary = directory / (name + ".tmp")
    temporary.write_text(value + "\n")
    temporary.chmod(0o600 if name == "id_ed25519" else 0o644)
    temporary.replace(path)
if pending.exists():
    subprocess.run(["systemctl", "try-restart", "nix-daemon.service"], check=True)
    pending.unlink()
`}, "input-data": base64.StdEncoding.EncodeToString(data),
		}
	}
	if err := guestExec(ctx, socket, setup); err != nil {
		return fmt.Errorf("configure remote builders: %w", err)
	}
	env := []string{"ORB_TOKEN=" + config.Token, "AMP_URL=" + config.AMPURL, "PATH=/run/wrappers/bin:/run/current-system/sw/bin:/usr/local/bin:/usr/bin:/bin"}
	return guestExec(ctx, socket, map[string]any{
		"path": "/bin/sh", "arg": []string{"-c", "exec sudo --preserve-env=ORB_TOKEN,AMP_URL -Hu user -- /bin/sh -c 'cd /home/user && nohup amp orb bootstrap </dev/null >.amp-bootstrap.log 2>&1 &'"},
		"env": env, "capture-output": false,
	})
}

func guestExec(ctx context.Context, socket string, args map[string]any) error {
	var process struct {
		PID int `json:"pid"`
	}
	err := guestCall(ctx, socket, "guest-exec", args, &process)
	if err != nil {
		return err
	}
	for {
		var result struct {
			Exited   bool
			Exitcode int
			Signal   int
		}
		if err := guestCall(ctx, socket, "guest-exec-status", map[string]any{"pid": process.PID}, &result); err != nil {
			return err
		}
		if result.Exited {
			if result.Exitcode != 0 || result.Signal != 0 {
				return fmt.Errorf("guest bootstrap launcher failed: exit=%d signal=%d", result.Exitcode, result.Signal)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func guestCall(ctx context.Context, socket, command string, args, output any) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	// A delimited sync discards an interrupted previous caller's partial reply.
	if _, err := conn.Write([]byte{0xff}); err != nil {
		return err
	}
	encoder := json.NewEncoder(conn)
	id := rand.Int64()
	if err := encoder.Encode(map[string]any{"execute": "guest-sync-delimited", "arguments": map[string]any{"id": id}}); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return err
		}
		if b == 0xff {
			break
		}
	}
	decoder := json.NewDecoder(reader)
	var synced struct{ Return int64 }
	if err := decoder.Decode(&synced); err != nil {
		return err
	}
	if synced.Return != id {
		return fmt.Errorf("guest-agent sync identity mismatch")
	}
	if err := encoder.Encode(map[string]any{"execute": command, "arguments": args}); err != nil {
		return err
	}
	var response struct {
		Return json.RawMessage
		Error  *struct{ Class, Desc string }
	}
	if err := decoder.Decode(&response); err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("guest-agent %s failed (%s)", command, response.Error.Class)
	}
	return json.Unmarshal(response.Return, output)
}
