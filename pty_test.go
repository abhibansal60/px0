//go:build linux || darwin

package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readUntil reads the PTY master until want appears or the deadline passes.
func readUntil(t *testing.T, f *os.File, out *bytes.Buffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 4096)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q; got %q", want, out.String())
		}
		_ = f.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, _ := f.Read(buf)
		out.Write(buf[:n])
	}
}

func TestPTYSizeInputAndResize(t *testing.T) {
	// stty size prints "rows cols"; SIGWINCH makes it print again after a resize.
	script := `trap 'stty size' WINCH; stty size; echo ready; read line; echo "got:$line"; while :; do sleep 0.05; done`
	cmd := exec.Command("/bin/sh", "-c", script)
	master, err := startInPTY(cmd, 30, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer signalGroup(cmd.Process.Pid, syscall.SIGKILL)

	var out bytes.Buffer
	readUntil(t, master, &out, "ready")
	if !strings.Contains(out.String(), "30 100") {
		t.Fatalf("initial size: want 30 100, got %q", out.String())
	}

	if _, err := master.Write([]byte("hello pty\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, master, &out, "got:hello pty")

	if err := setWinsize(master, 40, 120); err != nil {
		t.Fatal(err)
	}
	readUntil(t, master, &out, "40 120")
}

func TestPTYChildIsSessionLeaderWithControllingTerminal(t *testing.T) {
	// The child leads its own session and process group, and has a terminal.
	cmd := exec.Command("/bin/sh", "-c", `ps -o pid= -o pgid= -o sid= -p $$; tty; sleep 30`)
	master, err := startInPTY(cmd, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()

	var out bytes.Buffer
	readUntil(t, master, &out, "/dev/")
	fields := strings.Fields(out.String())
	if len(fields) < 3 {
		t.Fatalf("unexpected ps output %q", out.String())
	}
	if fields[0] != fields[1] || fields[0] != fields[2] {
		t.Errorf("want pid == pgid == sid, got %v", fields[:3])
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	signalGroup(cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process group did not die on SIGKILL")
	}
}

func TestPTYCloseUnblocksRead(t *testing.T) {
	// A reader parked on the master must return when the master is closed,
	// or closing a session would leak its reader goroutine.
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	master, err := startInPTY(cmd, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer signalGroup(cmd.Process.Pid, syscall.SIGKILL)

	returned := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := master.Read(buf); err != nil {
				close(returned)
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	master.Close()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not return after Close")
	}
}
