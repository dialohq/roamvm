package qcow

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestRejectHostFileReferencesBeforeOpeningBacking(t *testing.T) {
	for _, tt := range []struct {
		name       string
		offset     uint64
		features   uint64
		standalone bool
		valid      bool
	}{
		{"standalone", 0, 0, true, true},
		{"backed base", 4096, 0, true, false},
		{"overlay", 4096, 0, false, true},
		{"external data", 0, 4, false, false},
		{"compressed overlay", 4096, 8, false, false},
		{"compressed external data", 0, 12, true, false},
		{"extended L2", 0, 16, true, false},
		{"unknown feature", 0, 1 << 40, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := make([]byte, 104)
			binary.BigEndian.PutUint32(h[:4], 0x514649fb)
			binary.BigEndian.PutUint32(h[4:8], 3)
			binary.BigEndian.PutUint64(h[8:16], tt.offset)
			binary.BigEndian.PutUint64(h[72:80], tt.features)
			path := filepath.Join(t.TempDir(), "disk.qcow2")
			os.WriteFile(path, h, 0o600)
			if e := Validate(path, tt.standalone); (e == nil) != tt.valid {
				t.Fatalf("unexpected result: %v", e)
			}
		})
	}
}

func TestBaseCompressionHeader(t *testing.T) {
	for _, tt := range []struct {
		name   string
		length uint32
		codec  byte
		valid  bool
	}{
		{"zstd", 112, 1, true},
		{"missing field", 104, 1, false},
		{"deflate with alternate bit", 112, 0, false},
		{"unknown codec", 112, 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := make([]byte, 112)
			binary.BigEndian.PutUint32(h[:4], 0x514649fb)
			binary.BigEndian.PutUint32(h[4:8], 3)
			binary.BigEndian.PutUint64(h[72:80], 8)
			binary.BigEndian.PutUint32(h[100:104], tt.length)
			h[104] = tt.codec
			path := filepath.Join(t.TempDir(), "base.qcow2")
			if err := os.WriteFile(path, h, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := Validate(path, true); (err == nil) != tt.valid {
				t.Fatalf("unexpected result: %v", err)
			}
			if err := os.Truncate(path, 104); err != nil {
				t.Fatal(err)
			}
			if err := Validate(path, true); err == nil {
				t.Fatal("truncated compression header accepted")
			}
		})
	}
}
