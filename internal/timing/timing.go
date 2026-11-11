// Package timing emits opt-in lifecycle spans without payloads or credentials.
package timing

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var (
	sequence  atomic.Uint64
	processID = sync.OnceValue(rand.Text)
)

// Start returns a completion function. Spans may overlap and must not be summed.
// ORBVM_TRACE=1 writes JSON Lines to stderr alongside ordinary runtime logs.
func Start(stage string) func() {
	if os.Getenv("ORBVM_TRACE") != "1" {
		return func() {}
	}
	started := time.Now()
	id := sequence.Add(1)
	emit := func(event string) {
		data, _ := json.Marshal(struct {
			Event     string `json:"event"`
			Stage     string `json:"stage"`
			PID       int    `json:"pid"`
			ProcessID string `json:"process_id"`
			Span      uint64 `json:"span"`
			PodUID    string `json:"pod_uid,omitempty"`
			VMUID     string `json:"vm_uid,omitempty"`
			Time      string `json:"time"`
			ElapsedNS int64  `json:"elapsed_ns"`
		}{event, stage, os.Getpid(), processID(), id, os.Getenv("POD_UID"), os.Getenv("VM_UID"), time.Now().UTC().Format(time.RFC3339Nano), time.Since(started).Nanoseconds()})
		_, _ = os.Stderr.Write(append(data, '\n'))
	}
	emit("span_begin")
	return func() { emit("span_end") }
}
