//go:build linux

package main

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// openPTY allocates a pseudo-terminal pair through /dev/ptmx and returns the
// master side with the path of the slave, which becomes the child's controlling
// terminal.
func openPTY() (*os.File, string, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	var unlock int32
	if err := ptyIoctl(m, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		m.Close()
		return nil, "", err
	}
	var n uint32
	if err := ptyIoctl(m, syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		m.Close()
		return nil, "", err
	}
	return m, "/dev/pts/" + strconv.FormatUint(uint64(n), 10), nil
}
