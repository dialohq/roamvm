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
