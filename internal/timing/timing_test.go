package timing_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dialohq/roamvm/internal/timing"
)

func TestTraceOptInAndNestedSpans(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = file
	t.Cleanup(func() { os.Stderr = previous; file.Close() })
	t.Setenv("ORBVM_TRACE", "")
	timing.Start("disabled")()
	t.Setenv("ORBVM_TRACE", "1")
	t.Setenv("POD_UID", "pod-1")
	t.Setenv("ORB_TOKEN", "must-never-appear")
	outer := timing.Start("outer")
	timing.Start("inner")()
	outer()
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "disabled") || strings.Contains(string(data), "must-never-appear") {
		t.Fatalf("unexpected trace payload: %s", data)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected two complete spans: %s", data)
	}
	type event struct {
		Event     string `json:"event"`
		Stage     string `json:"stage"`
		Span      uint64 `json:"span"`
		ProcessID string `json:"process_id"`
		PodUID    string `json:"pod_uid"`
		Time      string `json:"time"`
		ElapsedNS int64  `json:"elapsed_ns"`
	}
	events := make([]event, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &events[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := time.Parse(time.RFC3339Nano, events[i].Time); err != nil {
			t.Fatal(err)
		}
		if events[i].PodUID != "pod-1" || events[i].ProcessID == "" || events[i].ProcessID != events[0].ProcessID {
			t.Fatalf("missing correlation: %s", line)
		}
	}
	for _, pair := range [][2]int{{0, 3}, {1, 2}} {
		begin, end := events[pair[0]], events[pair[1]]
		if begin.Event != "span_begin" || end.Event != "span_end" || begin.Span != end.Span ||
			begin.Stage != end.Stage || end.ElapsedNS < begin.ElapsedNS {
			t.Fatalf("invalid span: %+v %+v", begin, end)
		}
	}
	if events[0].Span == events[1].Span {
		t.Fatal("nested spans must have distinct identities")
	}
}

func BenchmarkDisabled(b *testing.B) {
	b.Setenv("ORBVM_TRACE", "")
	b.ReportAllocs()
	for b.Loop() {
		timing.Start("checkpoint.hash")()
	}
}
