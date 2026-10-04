package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/dialohq/roamvm/internal/daemon"
	"github.com/dialohq/roamvm/internal/qmp"
	"golang.org/x/sys/unix"
)

type Client struct {
	HTTP    *http.Client
	Request daemon.Request
}

func unixHTTP(socket string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
		Timeout: 30 * time.Minute,
	}
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

func (c *Client) status(phase, message string) bool {
	c.Request.Phase = phase
	c.Request.Message = message
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Call(ctx, "status")
	if err != nil {
		log.Printf("report VM status: %v", err)
	}
	return err == nil
}

func Run() error {
	started := time.Now()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread() // Keep the Pdeathsig parent thread alive.
	c := &Client{HTTP: unixHTTP("/run/roamvm/runtime.sock")}
	var err error
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
		if (!response.Retry && !errors.As(err, &transportError) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)) ||
			ctx.Err() != nil ||
			(!response.Retry && time.Now().After(deadline)) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
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
	log.Printf("startup stage=prepared elapsed=%s", time.Since(started))
	lock, err := os.OpenFile(filepath.Join(p.Dir, "runner.lock"), os.O_CREATE|os.O_RDWR, 0o600)
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
	log.Printf("startup stage=network elapsed=%s", time.Since(started))
	dnsExited := make(chan error, 1)
	go func() { dnsExited <- dns.Wait() }()
	defer dns.Process.Kill()
	socket := filepath.Join(p.Dir, "qmp.sock")
	_ = os.Remove(socket)
	if len(p.Spec.Devices) > 0 {
		if e := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &unix.Rlimit{Cur: unix.RLIM_INFINITY, Max: unix.RLIM_INFINITY}); e != nil {
			return e
		}
	}
	args, err := qemuArgs(ctx, p, socket)
	if err != nil {
		return err
	}
	hypervisor := exec.Command("qemu-system-x86_64", args...)
	hypervisor.Stdout = os.Stdout
	hypervisor.Stderr = os.Stderr
	// A killed runner must not leave a VMM alive holding the writable disk.
	hypervisor.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err = hypervisor.Start(); err != nil {
		return err
	}
	log.Printf("startup stage=hypervisor elapsed=%s", time.Since(started))
	defer hypervisor.Process.Kill()
	exited := make(chan error, 1)
	go func() { exited <- hypervisor.Wait() }()
	setReady := func(value bool) {
		if value {
			if e := os.WriteFile("/tmp/guest-ready", []byte("ready"), 0o600); e != nil {
				fmt.Fprintln(os.Stderr, "readiness file:", e)
			}
		} else {
			_ = os.Remove("/tmp/guest-ready")
		}
	}
	defer setReady(false)
	q := qmp.Client(socket)
	cleanShutdown := false
	lastContact := time.Now()
	var lastHeartbeat time.Time
	stopRequested := false
	var stopping time.Time
	powerSent := false
	runningReported := false
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case e := <-dnsExited:
			fmt.Fprintln(os.Stderr, "DHCP server exited; shutting down guest:", e)
			cancel()
			dnsExited = nil
		case err = <-exited:
			setReady(false)
			if err == nil && !cleanShutdown {
				err = errors.New("hypervisor exited without guest shutdown")
			}
			if err != nil {
				c.status("Error", "hypervisor failed; working disk retained: "+err.Error())
				return err
			}
			return finish(c)
		case <-tick.C:
			if time.Since(lastHeartbeat) >= time.Second {
				lastHeartbeat = time.Now()
				heartbeatCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
				h, e := c.Call(heartbeatCtx, "heartbeat")
				done()
				if e == nil {
					lastContact = time.Now()
				}
				stopRequested = h.Stop
				if e == nil && !h.Stop && stopping.IsZero() {
					resizeError := h.ResizeError
					size := c.Request.DiskSize
					if size == 0 || h.DiskSize > size {
						var resizeErr error
						size, resizeErr = q.Grow(context.Background(), h.DiskSize)
						if resizeErr != nil {
							resizeError = resizeErr.Error()
						}
					}
					if c.Request.DiskSize != size || c.Request.ResizeError != resizeError {
						previous := c.Request
						c.Request.DiskSize, c.Request.ResizeError = size, resizeError
						if runningReported && !c.status("Running", "") {
							c.Request = previous
						}
					}
				}
			}
			shouldStop := stopRequested || ctx.Err() != nil || time.Since(lastContact) > 30*time.Second
			if shouldStop && stopping.IsZero() {
				stopping = time.Now()
				// Observe shutdown promptly without increasing the steady-state
				// heartbeat rate or waiting a full second to notice QEMU halted.
				tick.Reset(100 * time.Millisecond)
				setReady(false)
				c.status("Stopping", "")
			}
			if !stopping.IsZero() {
				if !powerSent {
					if e := q.Call(context.Background(), "system_powerdown", nil, nil); e == nil {
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
						log.Printf("startup stage=guest-ready elapsed=%s", time.Since(started))
						runningReported = c.status("Running", "")
						tick.Reset(time.Second)
					}
				} else {
					setReady(false)
				}
			}
			var info struct {
				Status string `json:"status"`
			}
			if e := q.Call(context.Background(), "query-status", nil, &info); e == nil && info.Status == "shutdown" {
				cleanShutdown = true
				_ = q.Call(context.Background(), "quit", nil, nil)
			}
		}
	}
}

func finish(c *Client) error {
	c.status("Checkpointing", "")
	for {
		_, err := c.Call(context.Background(), "finish")
		if err == nil {
			return nil
		}
		fmt.Fprintln(os.Stderr, "local stop handoff incomplete:", err)
		c.status("Checkpointing", err.Error())
		// Retain the Pod until the stopped disk is handed off safely. Retained-PVC
		// runners return before upload; legacy ephemeral runners still commit here.
		time.Sleep(3 * time.Second)
	}
}
