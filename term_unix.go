//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// ioctlGetTermios / ioctlSetTermios are defined per OS in term_linux.go and term_bsd.go.

type winsize struct{ Row, Col, Xpixel, Ypixel uint16 }

func getTermios(fd uintptr) (syscall.Termios, error) {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlGetTermios, uintptr(unsafe.Pointer(&t))); e != 0 {
		return t, e
	}
	return t, nil
}

func sizeOf(fd uintptr) (cols, rows int, ok bool) {
	var ws winsize
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), ws.Col > 0 && ws.Row > 0
}

func stdoutIsTerminal() bool {
	_, err := getTermios(os.Stdout.Fd())
	return err == nil
}

// terminalSize reports the size of stdout's terminal, or of the controlling
// terminal when stdout is a pipe (so `rndr x.stl -p | less` still sizes sensibly).
func terminalSize() (cols, rows int, ok bool) {
	if c, r, ok := sizeOf(os.Stdout.Fd()); ok {
		return c, r, true
	}
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return 0, 0, false
	}
	defer tty.Close()
	return sizeOf(tty.Fd())
}

// rawTerm is the controlling terminal with line buffering and echo switched off.
type rawTerm struct {
	tty   *os.File
	saved syscall.Termios
}

func openRaw() (*rawTerm, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	saved, err := getTermios(tty.Fd())
	if err != nil {
		tty.Close()
		return nil, err
	}
	raw := saved
	raw.Lflag &^= syscall.ICANON | syscall.ECHO // keys arrive one at a time, unechoed; Ctrl-C still signals
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), ioctlSetTermios, uintptr(unsafe.Pointer(&raw))); e != 0 {
		tty.Close()
		return nil, e
	}
	return &rawTerm{tty: tty, saved: saved}, nil
}

func (t *rawTerm) Read(b []byte) (int, error)  { return t.tty.Read(b) }
func (t *rawTerm) Write(b []byte) (int, error) { return t.tty.Write(b) }
func (t *rawTerm) Size() (int, int, bool)      { return sizeOf(t.tty.Fd()) }

func (t *rawTerm) Restore() {
	syscall.Syscall(syscall.SYS_IOCTL, t.tty.Fd(), ioctlSetTermios, uintptr(unsafe.Pointer(&t.saved)))
}
