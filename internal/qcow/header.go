// Package qcow validates the small part of QCOW2 metadata that can reference
// host files, before any image parser is permitted to follow those references.
package qcow

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

func Validate(path string, standalone bool) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	h := make([]byte, 104)
	if _, e = io.ReadFull(f, h); e != nil {
		return e
	}
	if binary.BigEndian.Uint32(h[:4]) != 0x514649fb {
		return errors.New("invalid QCOW2 magic")
	}
	version := binary.BigEndian.Uint32(h[4:8])
	if version != 2 && version != 3 {
		return errors.New("unsupported QCOW2 version")
	}
	if binary.BigEndian.Uint32(h[32:36]) != 0 {
		return errors.New("encrypted QCOW2 images are unsupported")
	}
	if standalone && (binary.BigEndian.Uint64(h[8:16]) != 0 || binary.BigEndian.Uint32(h[16:20]) != 0) {
		return errors.New("base image must not reference a backing file")
	}
	if version == 3 {
		features := binary.BigEndian.Uint64(h[72:80])
		allowed := uint64(3) // Dirty/corrupt flags are diagnosed by qemu-img check.
		if standalone {
			allowed |= 8 // Alternate compression is supported only for immutable bases.
		}
		if features & ^allowed != 0 {
			return errors.New("unsupported QCOW2 incompatible features (including external data files)")
		}
		if features&8 != 0 {
			compression := make([]byte, 1)
			if binary.BigEndian.Uint32(h[100:104]) < 112 {
				return errors.New("missing QCOW2 compression header")
			}
			if _, e = io.ReadFull(f, compression); e != nil {
				return e
			}
			if compression[0] != 1 {
				return errors.New("unsupported QCOW2 compression type")
			}
		}
	}
	return nil
}
