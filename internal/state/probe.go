package state

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// CheckSemantics refuses stores that accept conditional headers but ignore
// them. Ownership relies on these operations being strongly consistent.
func (s *S3) CheckSemantics(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	key := "runtime-probes/" + rand.Text()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = s.Client.DeleteObject(cleanup, &s3.DeleteObjectInput{Bucket: &s.Bucket, Key: &key})
	}()
	first, e := s.Put(ctx, key, strings.NewReader("one"), 3, "")
	if e != nil {
		return e
	}
	if _, e = s.Put(ctx, key, strings.NewReader("bad"), 3, ""); !errors.Is(e, ErrConflict) {
		return fmt.Errorf("object store lacks create-only writes: %v", e)
	}
	if _, e = s.Put(ctx, key, strings.NewReader("two"), 3, first.ETag); e != nil {
		return e
	}
	if _, e = s.Put(ctx, key, strings.NewReader("old"), 3, first.ETag); !errors.Is(e, ErrConflict) {
		return fmt.Errorf("object store lacks ETag compare-and-swap: %v", e)
	}
	return checkObject(ctx, s, key, "two")
}

func checkObject(ctx context.Context, store Store, key, expected string) error {
	obj, e := store.Get(ctx, key)
	if e != nil {
		return e
	}
	defer obj.Body.Close()
	body, e := io.ReadAll(io.LimitReader(obj.Body, int64(len(expected)+1)))
	if e != nil {
		return e
	}
	if string(body) != expected {
		return errors.New("object store failed read-after-write check")
	}
	return nil
}
