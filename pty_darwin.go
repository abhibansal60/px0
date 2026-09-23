//go:build darwin

package main

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// openPTY allocates a pseudo-terminal pair through /dev/ptmx and returns the
// master side with the path of the slave, which becomes the child's controlling
// terminal. These three ioctls are what grantpt, unlockpt and ptsname do in libc.
func openPTY() (*os.File, string, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	if err := ptyIoctl(m, syscall.TIOCPTYGRANT, nil); err != nil {
		m.Close()
		return nil, "", err
	}
	if err := ptyIoctl(m, syscall.TIOCPTYUNLK, nil); err != nil {
		m.Close()
		return nil, "", err
	}
	var name [128]byte // TIOCPTYGNAME's fixed buffer size
	if err := ptyIoctl(m, syscall.TIOCPTYGNAME, unsafe.Pointer(&name[0])); err != nil {
		m.Close()
		return nil, "", err
	}
	if i := bytes.IndexByte(name[:], 0); i >= 0 {
		return m, string(name[:i]), nil
	}
	m.Close()
	return nil, "", syscall.EINVAL
}
