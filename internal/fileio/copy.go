package fileio

import (
	"bytes"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// SyncClose closes a completed write, preserving the first write or sync error.
func SyncClose(f *os.File, err error) error {
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// OCI tar streams contain zero-filled regions of raw disks. Preserve those as
// holes instead of allocating the entire virtual disk on the local filesystem.
func CopySparse(dst *os.File, src io.Reader) (int64, error) {
	buf := make([]byte, 64<<10)
	zero := make([]byte, len(buf))
	var total, synced int64
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			var e error
			if bytes.Equal(buf[:n], zero[:n]) {
				_, e = dst.Seek(int64(n), io.SeekCurrent)
			} else {
				_, e = dst.Write(buf[:n])
			}
			if e != nil {
				return total, e
			}
			total += int64(n)
			// Dirty file pages count against the daemon's cgroup limit too.
			if total-synced >= 32<<20 {
				if e := dst.Sync(); e != nil {
					return total, e
				}
				_ = unix.Fadvise(int(dst.Fd()), synced, total-synced, unix.FADV_DONTNEED)
				synced = total
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return total, dst.Truncate(total)
		}
		if err != nil {
			return total, err
		}
	}
}
