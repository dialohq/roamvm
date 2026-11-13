// Package pager exposes checkpoint-backed files to QEMU through ordinary file
// I/O and mmap. Immutable chunks are verified before entering the local cache;
// writable mappings persist into that cache, not into shared checkpoint blobs.
package pager

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"github.com/dialohq/roamvm/internal/state"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type fileNode struct {
	fs.Inode
	file *state.LazyFile
}

func (n *fileNode) Getattr(ctx context.Context, handle fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	info, err := n.file.Stat()
	if err != nil {
		return fs.ToErrno(err)
	}
	out.Size = uint64(info.Size())
	out.Mode = syscall.S_IFREG | 0o600
	out.Nlink = 1
	return 0
}

func (n *fileNode) Open(context.Context, uint32) (fs.FileHandle, uint32, syscall.Errno) {
	return &fileHandle{n.file}, fuse.FOPEN_KEEP_CACHE, 0
}

func (n *fileNode) Setattr(ctx context.Context, handle fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if size, ok := in.GetSize(); ok {
		if size > 1<<63-1 {
			return syscall.EFBIG
		}
		if err := n.file.Truncate(int64(size)); err != nil {
			return fs.ToErrno(err)
		}
	}
	return n.Getattr(ctx, handle, out)
}

type fileHandle struct{ file *state.LazyFile }

func (f *fileHandle) Read(ctx context.Context, dest []byte, offset int64) (fuse.ReadResult, syscall.Errno) {
	n, err := f.file.ReadAt(ctx, dest, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		log.Printf("pager read offset=%d: %v", offset, err)
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (f *fileHandle) Write(ctx context.Context, data []byte, offset int64) (uint32, syscall.Errno) {
	n, err := f.file.WriteAt(ctx, data, offset)
	if err != nil {
		log.Printf("pager write offset=%d: %v", offset, err)
		return uint32(n), syscall.EIO
	}
	return uint32(n), 0
}

func (f *fileHandle) Fsync(context.Context, uint32) syscall.Errno {
	return fs.ToErrno(f.file.Sync())
}

// Mount returns the opened caches for download accounting and an unmount
// function. QEMU must exit and all writable mmap data must be synced first.
// Files without lazy metadata remain ordinary local files and are not mounted.
func Mount(ctx context.Context, store state.Store, dir, mountpoint string, names []string) (map[string]*state.LazyFile, func() error, error) {
	files := map[string]*state.LazyFile{}
	closeFiles := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name) + ".lazy"); os.IsNotExist(err) {
			continue
		}
		f, err := state.OpenLazyFile(store, filepath.Join(dir, name))
		if err != nil {
			closeFiles()
			return nil, nil, err
		}
		files[name] = f
	}
	if len(files) == 0 {
		return files, func() error { return nil }, nil
	}
	if err := os.MkdirAll(mountpoint, 0o700); err != nil {
		closeFiles()
		return nil, nil, err
	}
	root := &fs.Inode{}
	server, err := fs.Mount(mountpoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{DirectMountStrict: true, MaxWrite: 1 << 20, MaxReadAhead: 128 << 10, FsName: "roamvm-checkpoint"},
		OnAdd: func(ctx context.Context) {
			for name, f := range files {
				root.AddChild(name, root.NewPersistentInode(ctx, &fileNode{file: f}, fs.StableAttr{Mode: syscall.S_IFREG}), false)
			}
		},
	})
	if err != nil {
		closeFiles()
		return nil, nil, err
	}
	return files, func() error {
		if err := server.Unmount(); err != nil {
			return err
		}
		server.Wait()
		closeFiles()
		return nil
	}, nil
}
