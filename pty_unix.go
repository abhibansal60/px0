//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const ptySupported = true

// ptyIoctl issues an ioctl on f without calling f.Fd(), which would switch the
// descriptor to blocking mode and stop Close from interrupting a pending Read.
func ptyIoctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// startInPTY starts cmd as the leader of a new session whose controlling
// terminal is a fresh pseudo-terminal of the given size, and returns the master
// side. The child's process group is its pid, so signalGroup reaches everything
// it starts in the foreground.
func startInPTY(cmd *exec.Cmd, rows, cols int) (*os.File, error) {
	master, slavePath, err := openPTY()
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, err
	}
	defer slave.Close() // the child holds its own copies once started
	if err := setWinsize(master, rows, cols); err != nil {
		master.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// Ctty indexes the child's descriptors: 0 is the slave, as stdin.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, err
	}
	return master, nil
}

// setWinsize tells the terminal (and, through SIGWINCH, the program in it) its
// size in character cells.
func setWinsize(master *os.File, rows, cols int) error {
	ws := struct{ Row, Col, X, Y uint16 }{Row: clampCells(rows), Col: clampCells(cols)}
	return ptyIoctl(master, syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
}

func clampCells(n int) uint16 {
	if n < 1 {
		return 1
	}
	if n > 1000 {
		return 1000
	}
	return uint16(n)
}

// signalGroup signals the process group a startInPTY child leads.
func signalGroup(pid int, sig syscall.Signal) {
	if pid > 0 {
		_ = syscall.Kill(-pid, sig)
	}
}
