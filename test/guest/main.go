// A deliberately small, test-only guest control service. Never use this image
// for real work: its unauthenticated endpoints exist for local lifecycle tests.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func main() {
	http.HandleFunc("/cpu", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		seconds, err := strconv.Atoi(r.URL.Query().Get("seconds"))
		if err != nil || seconds < 1 || seconds > 15 {
			http.Error(w, "seconds must be between 1 and 15", http.StatusBadRequest)
			return
		}
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		counts := make([]uint64, runtime.NumCPU())
		var workers sync.WaitGroup
		for i := range counts {
			workers.Go(func() {
				hash := [32]byte{byte(i)}
				for time.Now().Before(deadline) && r.Context().Err() == nil {
					for range 1024 {
						hash = sha256.Sum256(hash[:])
					}
					counts[i] += 1024
				}
			})
		}
		workers.Wait()
		json.NewEncoder(w).Encode(map[string]any{"cpus": runtime.NumCPU(), "iterations": counts})
	})
	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ready\n")) })
	http.HandleFunc("/config-disks", func(w http.ResponseWriter, r *http.Request) {
		values := map[string]string{}
		for label, path := range map[string]string{"agent": "/agent-config/credential", "tool": "/tool-config/setting"} {
			data, err := os.ReadFile(path)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if err = os.WriteFile(path, []byte("must not be writable"), 0o600); !errors.Is(err, syscall.EROFS) {
				http.Error(w, "configuration disk is not read-only", 500)
				return
			}
			values[label] = string(data)
		}
		json.NewEncoder(w).Encode(values)
	})
	http.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		values := map[string]string{}
		for _, name := range []string{"setting", "credential"} {
			b, e := os.ReadFile("/config/" + name)
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			values[name] = string(b)
		}
		json.NewEncoder(w).Encode(values)
	})
	http.HandleFunc("/secondary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			b, e := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			f, e := os.OpenFile("/secondary/marker", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			_, e = f.Write(b)
			if e == nil {
				e = f.Sync()
			}
			f.Close()
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
		}
		b, e := os.ReadFile("/secondary/marker")
		if e != nil {
			http.Error(w, e.Error(), 404)
			return
		}
		w.Write(b)
	})
	http.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			b, e := io.ReadAll(io.LimitReader(r.Body, 16<<20))
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			f, e := os.OpenFile("/persistent", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			_, e = f.Write(b)
			if e == nil {
				e = f.Sync()
			}
			f.Close()
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
		}
		b, e := os.ReadFile("/persistent")
		if e != nil {
			http.Error(w, e.Error(), 404)
			return
		}
		w.Write(b)
	})
	http.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		h, _ := os.Hostname()
		a, _ := net.InterfaceAddrs()
		data, _ := os.ReadFile("/persistent")
		hash := sha256.Sum256(data)
		json.NewEncoder(w).
			Encode(map[string]any{"hostname": h, "addresses": a, "sha256": hex.EncodeToString(hash[:]), "pid": os.Getpid()})
	})
	http.HandleFunc("/dns", func(w http.ResponseWriter, r *http.Request) {
		ips, e := net.LookupHost(r.URL.Query().Get("name"))
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		json.NewEncoder(w).Encode(ips)
	})
	http.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		w.WriteHeader(202)
		go func() { time.Sleep(100 * time.Millisecond); syscall.Kill(1, syscall.SIGUSR2) }()
	})
	if e := http.ListenAndServe(":8080", nil); e != nil {
		panic(e)
	}
}
