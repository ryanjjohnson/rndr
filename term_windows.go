package main

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode      = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode      = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBuf = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableProcessedInput   = 0x0001
	enableLineInput        = 0x0002
	enableEchoInput        = 0x0004
	enableVirtualTermInput = 0x0200
	enableVirtualTermOut   = 0x0004
)

type coord struct{ X, Y int16 }
type smallRect struct{ Left, Top, Right, Bottom int16 }
type consoleScreenBufferInfo struct {
	Size       coord
	Cursor     coord
	Attributes uint16
	Window     smallRect
	MaxSize    coord
}

func getMode(h uintptr) (uint32, bool) {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode)))
	return mode, r != 0
}

func setMode(h uintptr, mode uint32) bool {
	r, _, _ := procSetConsoleMode.Call(h, uintptr(mode))
	return r != 0
}

// keysFromTTY: on Windows the keyboard is stdin itself.
const keysFromTTY = false

func isTerminal(f *os.File) bool {
	_, ok := getMode(f.Fd())
	return ok
}

func terminalSize() (cols, rows int, ok bool) {
	var info consoleScreenBufferInfo
	r, _, _ := procGetConsoleScreenBuf.Call(os.Stdout.Fd(), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0, 0, false
	}
	cols = int(info.Window.Right-info.Window.Left) + 1
	rows = int(info.Window.Bottom-info.Window.Top) + 1
	return cols, rows, cols > 0 && rows > 0
}

// rawTerm is the console with line input and echo off and escape sequences on.
type rawTerm struct{ inMode, outMode uint32 }

func openRaw() (*rawTerm, error) {
	in, ok1 := getMode(os.Stdin.Fd())
	out, ok2 := getMode(os.Stdout.Fd())
	if !ok1 || !ok2 {
		return nil, errors.New("not a console")
	}
	raw := in&^(enableLineInput|enableEchoInput|enableProcessedInput) | enableVirtualTermInput
	if !setMode(os.Stdin.Fd(), raw) || !setMode(os.Stdout.Fd(), out|enableVirtualTermOut) {
		setMode(os.Stdin.Fd(), in)
		return nil, errors.New("this console does not support escape sequences")
	}
	return &rawTerm{inMode: in, outMode: out}, nil
}

func (t *rawTerm) Read(b []byte) (int, error)  { return os.Stdin.Read(b) }
func (t *rawTerm) Write(b []byte) (int, error) { return os.Stdout.Write(b) }
func (t *rawTerm) Size() (int, int, bool)      { return terminalSize() }

func (t *rawTerm) Restore() {
	setMode(os.Stdin.Fd(), t.inMode)
	setMode(os.Stdout.Fd(), t.outMode)
}
