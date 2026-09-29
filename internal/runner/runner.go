package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dialohq/roamvm/internal/daemon"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/api/resource"
)

type Client struct {
	HTTP    *http.Client
	Request daemon.Request
}

func unixHTTP(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}, Timeout: 30 * time.Minute}
}
func (c *Client) Call(ctx context.Context, action string) (daemon.Response, error) {
	b, err := json.Marshal(c.Request)
	if err != nil {
		return daemon.Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://runtime/"+action, bytes.NewReader(b))
	if err != nil {
		return daemon.Response{}, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return daemon.Response{}, err
	}
	defer res.Body.Close()
	var out daemon.Response
	if err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return out, err
	}
	if res.StatusCode != 200 {
		return out, fmt.Errorf("%s: %s", action, out.Error)
	}
	return out, nil
}
func (c *Client) status(phase, message string) {
	c.Request.Phase = phase
	c.Request.Message = message
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = c.Call(ctx, "status")
}

func Run() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread() // Keep the Pdeathsig parent thread alive.
	token, err := os.ReadFile("/run/roamvm-auth/token")
	if err != nil {
		return err
	}
	c := &Client{unixHTTP("/run/roamvm/runtime.sock"), daemon.Request{Namespace: os.Getenv("POD_NAMESPACE"), Pod: os.Getenv("POD_NAME"), UID: os.Getenv("POD_UID"), Token: string(token)}}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	var response daemon.Response
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, err = c.Call(ctx, "prepare")
		if err == nil {
			break
		}
		var transportError *url.Error
		if (!errors.As(err, &transportError) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)) || ctx.Err() != nil || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		c.status("Error", err.Error())
		return err
	}
	if response.Prepared == nil {
		return errors.New("daemon returned no prepared VM")
	}
	p := response.Prepared
	lock, err := os.OpenFile(filepath.Join(p.Dir, "runner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("another local runner holds this VM")
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	dns, err := network(ctx)
	if err != nil {
		c.status("Error", err.Error())
		return err
	}
	dnsExited := make(chan error, 1)
	go func() { dnsExited <- dns.Wait() }()
	defer dns.Process.Kill()
	select {
	case e := <-dnsExited:
		return fmt.Errorf("DHCP server failed: %w", e)
	case <-time.After(100 * time.Millisecond):
	}
	socket := filepath.Join(p.Dir, "ch.sock")
	_ = os.Remove(socket)
	memory, err := resource.ParseQuantity(p.Spec.Memory)
	if err != nil {
		return err
	}
	memoryArg := "size=" + strconv.FormatInt(memory.Value(), 10)
	if p.Spec.Hugepages != "" {
		q, e := resource.ParseQuantity(p.Spec.Hugepages)
		if e != nil {
			return e
		}
		memoryArg += ",hugepages=on,hugepage_size=" + strconv.FormatInt(q.Value(), 10)
	}
	if len(p.Spec.Devices) > 0 {
		if e := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &unix.Rlimit{Cur: unix.RLIM_INFINITY, Max: unix.RLIM_INFINITY}); e != nil {
			return e
		}
	}
	args := []string{"--api-socket", socket, "--cpus", "boot=" + strconv.Itoa(int(p.Spec.CPUs)), "--memory", memoryArg, "--disk", "path=" + filepath.Join(p.Dir, "overlay.qcow2") + ",image_type=qcow2,backing_files=on,direct=on", "--net", "tap=vm-tap,mac=02:00:00:00:00:02", "--console", "off", "--serial", "tty"}
	if _, e := os.Stat(filepath.Join(p.Base.Dir, "vmlinux")); e == nil {
		cmdline := p.Base.Manifest.Cmdline
		if p.Spec.Hostname != "" {
			cmdline += " systemd.hostname=" + p.Spec.Hostname
		}
		args = append(args, "--kernel", filepath.Join(p.Base.Dir, "vmlinux"), "--cmdline", cmdline)
		if _, e = os.Stat(filepath.Join(p.Base.Dir, "initrd")); e == nil {
			args = append(args, "--initramfs", filepath.Join(p.Base.Dir, "initrd"))
		}
	} else {
		args = append(args, "--firmware", filepath.Join(p.Base.Dir, "firmware"))
	}
	for _, d := range p.Spec.Disks {
		path := "/dev/disks/" + d.Name
		if d.VolumeMode == "Filesystem" {
			path = "/disks/" + d.Name + "/disk.img"
		}
		readonly := "off"
		if d.ReadOnly {
			readonly = "on"
		}
		args = append(args, "--disk", "path="+path+",image_type=raw,readonly="+readonly)
	}
	for _, device := range p.Spec.Devices {
		args = append(args, "--device", "path=/sys/bus/pci/devices/"+device.PCIAddress)
	}
	if p.Spec.Config != nil {
		configDisk := "/tmp/config.iso"
		if err = command(ctx, "genisoimage", "-quiet", "-follow-links", "-rock", "-joliet", "-V", "ROAMVM_CONFIG", "-o", configDisk, "/config"); err != nil {
			return err
		}
		args = append(args, "--disk", "path="+configDisk+",image_type=raw,readonly=on")
	}
	for _, disk := range p.Spec.ConfigDisks {
		path := "/tmp/config-" + disk.Name + ".iso"
		if err = command(ctx, "genisoimage", "-quiet", "-follow-links", "-rock", "-joliet", "-V", disk.Label, "-o", path, "/config-disks/"+disk.Name); err != nil {
			return err
		}
		args = append(args, "--disk", "path="+path+",image_type=raw,readonly=on")
	}
	hypervisor := exec.Command("cloud-hypervisor", args...)
	hypervisor.Stdout = os.Stdout
	hypervisor.Stderr = os.Stderr
	// A killed runner must not leave a VMM alive holding the writable disk.
	hypervisor.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err = hypervisor.Start(); err != nil {
		return err
	}
	defer hypervisor.Process.Kill()
	exited := make(chan error, 1)
	go func() { exited <- hypervisor.Wait() }()
	setReady := func(value bool) {
		if value {
			if e := os.WriteFile("/tmp/guest-ready", []byte("ready"), 0600); e != nil {
				fmt.Fprintln(os.Stderr, "readiness file:", e)
			}
		} else {
			_ = os.Remove("/tmp/guest-ready")
		}
	}
	defer setReady(false)
	ch := unixHTTP(socket)
	ch.Timeout = 3 * time.Second
	lastContact := time.Now()
	var stopping time.Time
	powerSent := false
	runningReported := false
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case e := <-dnsExited:
			fmt.Fprintln(os.Stderr, "DHCP server exited; shutting down guest:", e)
			cancel()
			dnsExited = nil
		case err = <-exited:
			setReady(false)
			if err != nil {
				c.status("Error", "hypervisor failed; working disk retained: "+err.Error())
				return err
			}
			return finish(c)
		case <-tick.C:
			heartbeatCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
			h, e := c.Call(heartbeatCtx, "heartbeat")
			done()
			if e == nil {
				lastContact = time.Now()
			}
			shouldStop := h.Stop || ctx.Err() != nil || time.Since(lastContact) > 30*time.Second
			if shouldStop && stopping.IsZero() {
				stopping = time.Now()
				setReady(false)
				c.status("Stopping", "")
			}
			if !stopping.IsZero() {
				if !powerSent {
					if e = chCall(ch, "PUT", "vm.power-button", nil); e == nil {
						powerSent = true
					}
				}
				if time.Since(stopping) > time.Duration(p.Spec.ShutdownTimeoutSeconds)*time.Second {
					_ = hypervisor.Process.Kill()
					<-exited
					c.status("Error", "guest did not shut down cleanly; working disk retained, no checkpoint committed")
					return errors.New("graceful shutdown timed out")
				}
			} else {
				conn, e := net.DialTimeout("tcp", net.JoinHostPort(GuestIP, strconv.Itoa(int(p.Spec.ReadinessPort))), 300*time.Millisecond)
				if e == nil {
					conn.Close()
					setReady(true)
					if !runningReported {
						c.status("Running", "")
						runningReported = true
					}
				} else {
					setReady(false)
				}
			}
			var info struct {
				State string `json:"state"`
			}
			if e = chCall(ch, "GET", "vm.info", &info); e == nil && strings.EqualFold(info.State, "Shutdown") {
				_ = chCall(ch, "PUT", "vmm.shutdown", nil)
			}
		}
	}
}
func chCall(c *http.Client, method, action string, out any) error {
	req, err := http.NewRequest(method, "http://vmm/api/v1/"+action, nil)
	if err != nil {
		return err
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("%s: %s", action, b)
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}
func finish(c *Client) error {
	c.status("Checkpointing", "")
	for {
		_, err := c.Call(context.Background(), "finish")
		if err == nil {
			return nil
		}
		fmt.Fprintln(os.Stderr, "checkpoint remains uncommitted:", err)
		c.status("Checkpointing", err.Error())
		// Keep the Pod and its local disk until the durable commit succeeds. Kubelet
		// termination/node loss may interrupt us, but must never report Stopped.
		time.Sleep(3 * time.Second)
	}
}
