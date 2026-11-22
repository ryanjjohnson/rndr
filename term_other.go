//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly || windows)

package main

import (
	"errors"
	"os"
)

// Platforms without terminal support here: print mode only.

type rawTerm struct{}

const keysFromTTY = false

func isTerminal(f *os.File) bool               { return false }
func terminalSize() (cols, rows int, ok bool)  { return 0, 0, false }
func openRaw() (*rawTerm, error)               { return nil, errors.New("not supported on this platform") }
func (t *rawTerm) Read(b []byte) (int, error)  { return 0, errors.New("unsupported") }
func (t *rawTerm) Write(b []byte) (int, error) { return len(b), nil }
func (t *rawTerm) Size() (int, int, bool)      { return 0, 0, false }
func (t *rawTerm) Restore()                    {}
