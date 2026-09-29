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
		// Dirty and corrupt flags are left for qemu-img check to diagnose. External
		// data files, alternate compression and extended L2 are outside the format
		// this runtime creates and must not broaden access to the node filesystem.
		if features & ^uint64(3) != 0 {
			return errors.New("unsupported QCOW2 incompatible features (including external data files)")
		}
	}
	return nil
}
