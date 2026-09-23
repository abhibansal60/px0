//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ptySupported is false where px0 has no pseudo-terminal implementation yet
// (Windows needs ConPTY; the BSDs differ in their ptmx ioctls). The terminal
// pane then reports itself unavailable instead of failing on first use.
const ptySupported = false

var errPTYUnsupported = errors.New("the terminal is not supported on this platform yet")

func startInPTY(cmd *exec.Cmd, rows, cols int) (*os.File, error) { return nil, errPTYUnsupported }

func setWinsize(master *os.File, rows, cols int) error { return nil }

func signalGroup(pid int, sig syscall.Signal) {}
