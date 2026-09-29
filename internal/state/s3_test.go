package state

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestS3ConditionalAndMultipart(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set TEST_S3_ENDPOINT and TEST_S3_BUCKET for the real object-store test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, e := NewS3(ctx, endpoint, os.Getenv("TEST_S3_BUCKET"))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.CheckSemantics(ctx); e != nil {
		t.Fatal(e)
	}
	key := "conformance/" + time.Now().Format("20060102T150405.000000000")
	one, e := store.Put(ctx, key, bytes.NewReader([]byte("one")), 3, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = store.Put(ctx, key, bytes.NewReader([]byte("two")), 3, ""); !errors.Is(e, ErrConflict) {
		t.Fatalf("store ignores create-only writes: %v", e)
	}
	two, e := store.Put(ctx, key, bytes.NewReader([]byte("two")), 3, one.ETag)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = store.Put(ctx, key, bytes.NewReader([]byte("old")), 3, one.ETag); !errors.Is(e, ErrConflict) {
		t.Fatalf("store ignores ETag fencing: %v", e)
	}
	current, e := store.Get(ctx, key)
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(current.Body)
	current.Body.Close()
	if string(body) != "two" || current.ETag != two.ETag {
		t.Fatal("read-after-write inconsistency")
	}
	file, e := os.Create(filepath.Join(t.TempDir(), "multipart"))
	if e != nil {
		t.Fatal(e)
	}
	defer file.Close()
	const size = int64(65 << 20)
	if e = file.Truncate(size); e != nil {
		t.Fatal(e)
	}
	if _, e = store.Put(ctx, key+"-large", file, size, ""); e != nil {
		t.Fatal(e)
	}
	file.Seek(0, 0)
	if _, e = store.Put(ctx, key+"-large", file, size, ""); !errors.Is(e, ErrConflict) {
		t.Fatalf("multipart overwrite accepted: %v", e)
	}
	large, e := store.Get(ctx, key+"-large")
	if e != nil {
		t.Fatal(e)
	}
	defer large.Body.Close()
	n, e := io.Copy(io.Discard, large.Body)
	if e != nil || n != size {
		t.Fatal("multipart data truncated", n, e)
	}
}
