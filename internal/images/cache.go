package images

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dialohq/roamvm/internal/fileio"
	"github.com/dialohq/roamvm/internal/qcow"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"golang.org/x/sys/unix"
)

type Manifest struct {
	Format  string `json:"format"`
	Cmdline string `json:"cmdline,omitempty"`
}
type Base struct {
	Dir      string
	Manifest Manifest
}

func (b Base) Disk() string { return filepath.Join(b.Dir, "root."+b.Manifest.Format) }

type Cache struct {
	Root             string
	PlainHTTP        bool
	MaxUnpackedBytes int64
}

func (c Cache) Ensure(ctx context.Context, reference string, keys authn.Keychain) (Base, error) {
	options := []name.Option{}
	if c.PlainHTTP {
		options = append(options, name.Insecure)
	}
	ref, err := name.NewDigest(reference, options...)
	if err != nil {
		return Base{}, fmt.Errorf("image must be pinned by digest: %w", err)
	}
	if !strings.HasPrefix(ref.DigestStr(), "sha256:") {
		return Base{}, errors.New("only sha256 image digests are supported")
	}
	dir := filepath.Join(c.Root, strings.TrimPrefix(ref.DigestStr(), "sha256:"))
	if err = os.MkdirAll(c.Root, 0755); err != nil {
		return Base{}, err
	}
	lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return Base{}, err
	}
	defer lock.Close()
	// Nonblocking flock allows cancellation during an unrelated image pull.
	if err = lockContext(ctx, lock); err != nil {
		return Base{}, err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if b, e := readBase(dir); e == nil {
		return b, nil
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(keys))
	if err != nil {
		return Base{}, err
	}
	tmp, err := os.MkdirTemp(c.Root, ".pull-")
	if err != nil {
		return Base{}, err
	}
	defer os.RemoveAll(tmp)
	reader := mutate.Extract(img)
	defer reader.Close()
	limit := c.MaxUnpackedBytes
	if limit == 0 {
		limit = 128 << 30
	}
	if err = extract(reader, tmp, limit); err != nil {
		return Base{}, err
	}
	b, err := readBase(tmp)
	if err != nil {
		return Base{}, err
	}
	if b.Manifest.Format == "qcow2" {
		if err = qcow.Validate(b.Disk(), true); err != nil {
			return Base{}, err
		}
	}
	info, err := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", "-f", b.Manifest.Format, b.Disk()).Output()
	if err != nil {
		return Base{}, fmt.Errorf("inspect base: %w", err)
	}
	var disk struct {
		Backing string `json:"backing-filename"`
		Size    int64  `json:"virtual-size"`
	}
	if err = json.Unmarshal(info, &disk); err != nil {
		return Base{}, err
	}
	if disk.Backing != "" || disk.Size <= 0 {
		return Base{}, errors.New("base must be a standalone disk without a backing file")
	}
	// Publish a complete cache entry atomically, never a half-extracted image.
	if err = os.Rename(tmp, dir); err != nil {
		return Base{}, err
	}
	b.Dir = dir
	return b, nil
}

func readBase(dir string) (Base, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Base{}, err
	}
	var m Manifest
	if err = json.Unmarshal(data, &m); err != nil {
		return Base{}, err
	}
	if m.Format != "raw" && m.Format != "qcow2" {
		return Base{}, errors.New("manifest format must be raw or qcow2")
	}
	if _, err = os.Stat(filepath.Join(dir, "root."+m.Format)); err != nil {
		return Base{}, err
	}
	_, kernel := os.Stat(filepath.Join(dir, "vmlinux"))
	_, firmware := os.Stat(filepath.Join(dir, "firmware"))
	if kernel != nil && firmware != nil {
		return Base{}, errors.New("base needs disk/vmlinux or disk/firmware")
	}
	return Base{dir, m}, nil
}

func extract(r io.Reader, dir string, limit int64) error {
	t := tar.NewReader(r)
	seen := map[string]bool{}
	var total int64
	for {
		h, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := strings.TrimPrefix(strings.TrimPrefix(h.Name, "./"), "/")
		leaf, insideDisk := strings.CutPrefix(clean, "disk/")
		if !insideDisk {
			continue
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		switch leaf {
		case "root.raw", "root.qcow2", "manifest.json", "vmlinux", "initrd", "firmware":
		default:
			return fmt.Errorf("unexpected disk artifact entry %q", h.Name)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return errors.New("disk artifact links/devices are forbidden")
		}
		if seen[leaf] {
			return errors.New("duplicate disk artifact entry")
		}
		seen[leaf] = true
		total += h.Size
		if h.Size < 0 || total > limit {
			return errors.New("image exceeds unpacked size limit")
		}
		f, err := os.OpenFile(filepath.Join(dir, leaf), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0444)
		if err != nil {
			return err
		}
		_, err = fileio.CopySparse(f, t)
		if err = fileio.SyncClose(f, err); err != nil {
			return err
		}
	}
}
