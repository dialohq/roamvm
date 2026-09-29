package images

import (
	"archive/tar"
	"bytes"
	"context"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRejectUnsafeArtifactEntries(t *testing.T) {
	tests := []struct {
		name  string
		kind  byte
		body  string
		limit int64
	}{
		{"disk/../../escape", tar.TypeReg, "bad", 100},
		{"disk/root.raw", tar.TypeSymlink, "", 100},
		{"disk/root.raw", tar.TypeLink, "", 100},
		{"disk/root.raw", tar.TypeChar, "", 100},
		{"disk/root.raw", tar.TypeReg, "oversized", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name+string(tt.kind), func(t *testing.T) {
			var b bytes.Buffer
			w := tar.NewWriter(&b)
			h := &tar.Header{Name: tt.name, Typeflag: tt.kind, Mode: 0644, Size: int64(len(tt.body)), Linkname: "/etc/shadow"}
			if tt.kind != tar.TypeReg {
				h.Size = 0
			}
			if e := w.WriteHeader(h); e != nil {
				t.Fatal(e)
			}
			w.Write([]byte(tt.body))
			w.Close()
			if e := extract(&b, t.TempDir(), tt.limit); e == nil {
				t.Fatal("unsafe entry accepted")
			}
		})
	}
}

type fixedKeychain struct{ auth authn.Authenticator }

func (k fixedKeychain) Resolve(authn.Resource) (authn.Authenticator, error) { return k.auth, nil }

func TestPrivateRegistryConcurrentPullAndOfflineCache(t *testing.T) {
	backend := registry.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "test" || p != "only" {
			w.Header().Set("WWW-Authenticate", `Basic realm="local-test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer server.Close()
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	for n, b := range map[string][]byte{"root.raw": make([]byte, 8192), "manifest.json": []byte(`{"format":"raw"}`), "vmlinux": []byte("fixture boot file")} {
		if e := w.WriteHeader(&tar.Header{Name: "disk/" + n, Mode: 0444, Size: int64(len(b))}); e != nil {
			t.Fatal(e)
		}
		if _, e := w.Write(b); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	layer, e := tarball.LayerFromReader(&archive)
	if e != nil {
		t.Fatal(e)
	}
	img, e := mutate.AppendLayers(empty.Image, layer)
	if e != nil {
		t.Fatal(e)
	}
	tag, e := name.NewTag(strings.TrimPrefix(server.URL, "http://")+"/private/base:test", name.Insecure)
	if e != nil {
		t.Fatal(e)
	}
	auth := &authn.Basic{Username: "test", Password: "only"}
	if e = remote.Write(tag, img, remote.WithAuth(auth)); e != nil {
		t.Fatal(e)
	}
	digest, e := img.Digest()
	if e != nil {
		t.Fatal(e)
	}
	ref := tag.Context().Digest(digest.String()).Name()
	cache := Cache{Root: t.TempDir(), PlainHTTP: true}
	if _, e = cache.Ensure(context.Background(), ref, fixedKeychain{authn.Anonymous}); e == nil {
		t.Fatal("private registry allowed anonymous pull")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			base, err := cache.Ensure(context.Background(), ref, fixedKeychain{auth})
			if err != nil {
				t.Error(err)
				return
			}
			data, err := os.ReadFile(base.Disk())
			if err != nil || len(data) != 8192 {
				t.Error("incomplete cache publication", err)
			}
		}()
	}
	wg.Wait()
	server.Close()
	if _, e = cache.Ensure(context.Background(), ref, fixedKeychain{auth}); e != nil {
		t.Fatal("cached base requires live registry", e)
	}
}
func TestExtractOnlyDiskPayload(t *testing.T) {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, n := range []string{"etc/passwd", "disk/root.raw", "disk/manifest.json", "disk/vmlinux"} {
		data := []byte("test")
		w.WriteHeader(&tar.Header{Name: n, Mode: 0644, Size: int64(len(data))})
		w.Write(data)
	}
	w.Close()
	dir := t.TempDir()
	if e := extract(&b, dir, 100); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(dir, "root.raw")); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(dir, "etc")); !os.IsNotExist(e) {
		t.Fatal("OCI rootfs unpacked outside disk payload")
	}
}
