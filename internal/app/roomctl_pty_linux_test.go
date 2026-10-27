// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// pty opens a pseudo-terminal: the slave is a terminal Roomctl writes to, the master is read.
func pty(t *testing.T) (slave *os.File, master *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	var n uint32
	zero := 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&zero))); e != 0 { //nolint:gosec // the pty ioctl
		t.Skipf("unlockpt: %v", e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 { //nolint:gosec // the pty ioctl
		t.Skipf("ptsname: %v", e)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty slave: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = m.Close() })
	return s, m
}

// On a terminal the view is the colour one (TextColor), and what an agent wrote is still inert: no
// escape of the agent's reaches the terminal.
func TestRoomctlViewsOnATerminalAreInert(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, args := range [][]string{{"status", "26zfnuxm"}} {
		slave, master := pty(t)
		r, _ := configured(t, newRoomctlBroker(t))
		r.Out = slave
		if err := r.Run(t.Context(), args); err != nil {
			t.Fatal(err)
		}
		chunks := make(chan string, 64)
		go func() { // ends when the cleanup closes the master
			buf := make([]byte, 4096)
			for {
				n, err := master.Read(buf)
				if n > 0 {
					chunks <- string(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}()
		var got string
		for done := false; !done; {
			select {
			case c := <-chunks:
				got += c
			case <-time.After(300 * time.Millisecond): // the output is small: silence means it is all here
				done = true
			}
		}
		if !strings.Contains(got, "hi[2J") { // the agent's text, cleaned, in the view
			t.Fatalf("%v: %q", args, got)
		}
		if strings.Contains(got, "\x1b[2J") {
			t.Fatalf("%v: the agent's escape reached the terminal: %q", args, got)
		}
	}
}
