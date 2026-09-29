package fileio

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestSparseCopyAcrossWritebackBoundary(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "disk"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const hole = 33 << 20
	tail := []byte("persistent changes")
	n, err := CopySparse(f, io.MultiReader(io.LimitReader(zeroReader{}, hole), bytes.NewReader(tail), io.LimitReader(zeroReader{}, 4096)))
	if err != nil || n != hole+int64(len(tail))+4096 {
		t.Fatalf("copy: %d %v", n, err)
	}
	info, err := f.Stat()
	if err != nil || info.Size() != n {
		t.Fatalf("size: %v %v", info, err)
	}
	got := make([]byte, len(tail)+4096)
	if _, err = f.ReadAt(got, hole); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:len(tail)], tail) || !bytes.Equal(got[len(tail):], make([]byte, 4096)) {
		t.Fatal("tail changed")
	}
}

func TestSparseCopyPropagatesReadFailure(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "disk"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = CopySparse(f, brokenReader{})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lost source failure: %v", err)
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }
