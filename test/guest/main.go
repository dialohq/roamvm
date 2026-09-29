// A deliberately small, test-only guest control service. Never use this image
// for real work: its unauthenticated endpoints exist for local lifecycle tests.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

func main() {
	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ready\n")) })
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
			f, e := os.OpenFile("/secondary/marker", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
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
			f, e := os.OpenFile("/persistent", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
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
		json.NewEncoder(w).Encode(map[string]any{"hostname": h, "addresses": a, "sha256": hex.EncodeToString(hash[:]), "pid": os.Getpid()})
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
