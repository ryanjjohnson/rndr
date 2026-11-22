// rndr - ASCII previewer for STL, OBJ and 3MF models in the terminal.
//
// Standard library only: one static binary, no runtime, no external programs.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// version is stamped in by build.sh (-ldflags -X main.version=...).
var version = "dev"

// system is everything rndr touches outside its own memory. The real one is
// built in main; tests and fuzzers substitute fakes, which is what lets the
// command line and the interactive loop be attacked without a real terminal or
// disk.
type system struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	stdinIsTerm    func() bool
	stdoutIsTerm   func() bool
	termSize       func() (cols, rows int, ok bool)
	getenv         func(string) string

	// fileExists reports whether path names something that could be opened.
	fileExists func(path string) bool
	// openFile opens a model for reading and reports its size. It refuses
	// anything that is not a regular file.
	openFile func(path string) (io.ReadCloser, int64, error)
	// writeFile is what -o does.
	writeFile func(path string, data []byte) error
	// createNew writes a file only if nothing by that name exists (the p key).
	createNew func(path string, data []byte) error
	// saveDirs are where the p key tries to save, in order.
	saveDirs func() []string

	// openTerm puts the terminal into key-at-a-time mode.
	openTerm func() (terminal, error)
	// onInterrupt arranges for cleanup to run if the process is interrupted;
	// the returned function cancels that.
	onInterrupt func(cleanup func()) (stop func())
}

// terminal is a keyboard and screen in raw mode.
type terminal interface {
	io.ReadWriter
	Size() (cols, rows int, ok bool)
	Restore()
}

var errUsage = errors.New("usage")

func main() {
	// A bug must not turn into a stack trace full of whatever was being parsed.
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintln(os.Stderr, "rndr: internal error:", printable(fmt.Sprint(p)))
			os.Exit(2)
		}
	}()
	err := run(realSystem(), os.Args[1:])
	if err == errUsage {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rndr:", printable(err.Error()))
		os.Exit(1)
	}
}

// printable makes text safe to show on a terminal. File names, and error text
// that quotes file contents, are attacker-controlled: without this an escape
// sequence in a file name could retitle, clear or reprogram the terminal.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			return '?'
		}
		return r
	}, s)
}

func realSystem() *system {
	return &system{
		stdin:        os.Stdin,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
		stdinIsTerm:  func() bool { return isTerminal(os.Stdin) },
		stdoutIsTerm: func() bool { return isTerminal(os.Stdout) },
		termSize:     terminalSize,
		getenv:       os.Getenv,
		fileExists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		openFile: func(path string) (io.ReadCloser, int64, error) {
			st, err := os.Stat(path)
			if err != nil || st.IsDir() {
				return nil, 0, errors.New("no such file")
			}
			if !st.Mode().IsRegular() { // devices and pipes never end; a model is a plain file
				return nil, 0, errors.New("not a regular file")
			}
			f, err := os.Open(path)
			if err != nil {
				return nil, 0, errors.New("cannot be opened")
			}
			return f, st.Size(), nil
		},
		writeFile: func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) },
		createNew: func(path string, data []byte) error {
			// O_EXCL also refuses to follow a symlink planted at that name
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return err
			}
			_, err = f.Write(data)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			return err
		},
		saveDirs: func() []string {
			cwd, _ := os.Getwd()
			return []string{cwd, os.TempDir()}
		},
		openTerm: func() (terminal, error) { return openRaw() },
		onInterrupt: func(cleanup func()) func() {
			// On its own goroutine so Ctrl-C works even while a frame is being drawn.
			sigs := make(chan os.Signal, 1)
			done := make(chan struct{})
			signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
			go func() {
				select {
				case <-sigs:
					cleanup()
					os.Exit(130)
				case <-done:
				}
			}()
			return func() {
				signal.Stop(sigs)
				close(done)
			}
		},
	}
}
