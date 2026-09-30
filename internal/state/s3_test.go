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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Runs with Garage as well as MinIO: Kubernetes owns the metadata CAS.
func TestObjectStoreCheckpointReplacement(t *testing.T) {
	endpoint := os.Getenv("TEST_OBJECT_STORE_ENDPOINT")
	if endpoint == "" {
		t.Skip("set TEST_OBJECT_STORE_ENDPOINT and TEST_OBJECT_STORE_BUCKET")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	objects, err := NewS3(ctx, endpoint, os.Getenv("TEST_OBJECT_STORE_BUCKET"))
	if err != nil {
		t.Fatal(err)
	}
	testStoreReplacement(t, ctx, retentionManager(objects, "kubernetes"), objects)
}

func testStoreReplacement(t *testing.T, ctx context.Context, m Manager, objects *S3) string {
	t.Helper()
	id := "retention-" + time.Now().Format("20060102T150405.000000000")
	path := filepath.Join(t.TempDir(), "overlay")
	var previous string
	for _, data := range []string{"first", "second", "third"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := m.Acquire(ctx, id, "base", data, data)
		if err != nil {
			t.Fatal(err)
		}
		s, err = m.Commit(ctx, s, path)
		if err != nil {
			t.Fatal(err)
		}
		keys, err := objects.List(ctx, "vm/"+id+"/overlay/")
		if err != nil || len(keys) != 1 || keys[0] != s.Head.Checkpoint.Key {
			t.Fatal("retained old stops", keys, err)
		}
		if previous != "" {
			if _, err = objects.Get(ctx, previous); !errors.Is(err, ErrNotFound) {
				t.Fatal("old checkpoint still exists", err)
			}
			if err = objects.Delete(ctx, previous); err != nil {
				t.Fatal("delete retry failed", err)
			}
		}
		checkRestore(t, m, s, data)
		previous = s.Head.Checkpoint.Key
	}
	return id
}

func TestS3VersionedCheckpointReplacement(t *testing.T) {
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set TEST_S3_ENDPOINT; requires permission to create a temporary versioned test bucket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	bucket := "roamvm-retention-" + time.Now().Format("20060102t150405000000000")
	store, err := NewS3(ctx, endpoint, bucket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		versions, e := store.Client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &bucket})
		if e != nil {
			t.Error(e)
			return
		}
		for _, v := range versions.Versions {
			if _, e = store.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: v.Key, VersionId: v.VersionId}); e != nil {
				t.Error(e)
			}
		}
		for _, v := range versions.DeleteMarkers {
			if _, e = store.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: v.Key, VersionId: v.VersionId}); e != nil {
				t.Error(e)
			}
		}
		if _, e = store.Client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket}); e != nil {
			t.Error(e)
		}
	}()
	if _, err = store.Client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &bucket, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatal(err)
	}
	id := testStoreReplacement(t, ctx, Manager{store}, store)
	versions, err := store.Client.ListObjectVersions(
		ctx,
		&s3.ListObjectVersionsInput{Bucket: &bucket, Prefix: aws.String("vm/" + id + "/overlay/")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions.Versions) != 1 || len(versions.DeleteMarkers) != 0 {
		t.Fatal("old disk versions were hidden rather than deleted", versions)
	}
}

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
