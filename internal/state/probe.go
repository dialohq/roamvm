package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// CheckSemantics refuses stores that accept conditional headers but ignore
// them. Ownership relies on these operations being strongly consistent.
func (s *S3) CheckSemantics(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	id := make([]byte, 16)
	if _, e := rand.Read(id); e != nil {
		return e
	}
	key := "runtime-probes/" + hex.EncodeToString(id)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.Client.DeleteObject(cleanup, &s3.DeleteObjectInput{Bucket: &s.Bucket, Key: &key})
	}()
	first, e := s.Put(ctx, key, bytes.NewReader([]byte("one")), 3, "")
	if e != nil {
		return e
	}
	if _, e = s.Put(ctx, key, bytes.NewReader([]byte("bad")), 3, ""); !errors.Is(e, ErrConflict) {
		return fmt.Errorf("object store lacks create-only writes: %v", e)
	}
	if _, e = s.Put(ctx, key, bytes.NewReader([]byte("two")), 3, first.ETag); e != nil {
		return e
	}
	if _, e = s.Put(ctx, key, bytes.NewReader([]byte("old")), 3, first.ETag); !errors.Is(e, ErrConflict) {
		return fmt.Errorf("object store lacks ETag compare-and-swap: %v", e)
	}
	obj, e := s.Get(ctx, key)
	if e != nil {
		return e
	}
	defer obj.Body.Close()
	body, e := io.ReadAll(io.LimitReader(obj.Body, 4))
	if e != nil {
		return e
	}
	if string(body) != "two" {
		return errors.New("object store failed read-after-write check")
	}
	return nil
}
