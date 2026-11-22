package main

// Tests and fuzz targets for everything around the parsers: the command line,
// stdin detection, the interactive key loop, and view parameters. They run the
// real run() against a fake system, so nothing here touches the terminal or disk.

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- fake system -------------------------------------------------------------

type fakeTerm struct {
	keys       [][]byte // one Read each, then EOF
	cols, rows int
	out        bytes.Buffer
	restored   int
}

func (t *fakeTerm) Read(b []byte) (int, error) {
	if len(t.keys) == 0 {
		return 0, io.EOF
	}
	n := copy(b, t.keys[0])
	if t.keys[0] = t.keys[0][n:]; len(t.keys[0]) == 0 {
		t.keys = t.keys[1:]
	}
	return n, nil
}
func (t *fakeTerm) Write(b []byte) (int, error) { return t.out.Write(b) }
func (t *fakeTerm) Size() (int, int, bool)      { return t.cols, t.rows, t.cols > 0 && t.rows > 0 }
func (t *fakeTerm) Restore()                    { t.restored++ }

type fake struct {
	sys           *system
	out, errOut   bytes.Buffer
	files         map[string][]byte
	written       map[string][]byte // -o
	created       map[string][]byte // p key
	env           map[string]string
	stdoutTTY     bool
	termCols      int
	termRows      int
	term          *fakeTerm // nil: no terminal can be opened
	termOpened    int
	interruptSet  int
	interruptStop int
}

// newFake builds a system whose stdin carries stdin (nil = a keyboard).
func newFake(files map[string][]byte, stdin io.Reader) *fake {
	f := &fake{files: files, written: map[string][]byte{}, created: map[string][]byte{}, env: map[string]string{}}
	f.sys = &system{
		stdin:        stdin,
		stdout:       &f.out,
		stderr:       &f.errOut,
		stdinIsTerm:  func() bool { return stdin == nil },
		stdoutIsTerm: func() bool { return f.stdoutTTY },
		termSize:     func() (int, int, bool) { return f.termCols, f.termRows, f.termCols > 0 && f.termRows > 0 },
		getenv:       func(k string) string { return f.env[k] },
		fileExists:   func(p string) bool { _, ok := f.files[p]; return ok },
		openFile: func(p string) (io.ReadCloser, int64, error) {
			d, ok := f.files[p]
			if !ok {
				return nil, 0, errors.New("no such file")
			}
			return io.NopCloser(bytes.NewReader(d)), int64(len(d)), nil
		},
		writeFile: func(p string, d []byte) error { f.written[p] = append([]byte(nil), d...); return nil },
		createNew: func(p string, d []byte) error {
			if _, ok := f.created[p]; ok {
				return fs.ErrExist
			}
			f.created[p] = append([]byte(nil), d...)
			return nil
		},
		saveDirs: func() []string { return []string{"/cwd", "/tmp"} },
		openTerm: func() (terminal, error) {
			f.termOpened++
			if f.term == nil {
				return nil, errors.New("no terminal")
			}
			return f.term, nil
		},
		onInterrupt: func(func()) func() {
			f.interruptSet++
			return func() { f.interruptStop++ }
		},
	}
	return f
}

// terminalSequences are the only escape sequences rndr ever sends.
var terminalSequences = strings.NewReplacer(
	"\x1b[?1049h", "", "\x1b[?1049l", "", "\x1b[?25l", "", "\x1b[?25h", "",
	"\x1b[H", "", "\x1b[K", "", "\x1b[7m", "", "\x1b[0m", "", "\r\n", "")

func onlyFrom(s, allowed string) int {
	return strings.IndexFunc(s, func(c rune) bool { return !strings.ContainsRune(allowed, c) })
}

func printableASCII(s string) int {
	return strings.IndexFunc(s, func(c rune) bool { return c != '\n' && (c < ' ' || c > '~') })
}

// check asserts the invariants that must hold after any run, whatever the input:
// only expected bytes reach stdout, files and the terminal, and the terminal is
// always handed back.
func (f *fake) check(t testing.TB, err error, info bool) {
	t.Helper()
	out := f.out.String()
	if info { // --help / --version
		if i := printableASCII(out); i >= 0 {
			t.Fatalf("stdout has byte %q", out[i])
		}
	} else if i := onlyFrom(out, allowedOutput); i >= 0 {
		t.Fatalf("stdout has unexpected byte %q", out[i])
	}
	if f.errOut.Len() > 0 {
		t.Fatalf("run wrote to stderr itself: %q", f.errOut.String())
	}
	for p, d := range f.written {
		if i := onlyFrom(string(d), allowedOutput); i >= 0 {
			t.Fatalf("-o file %q has unexpected byte %q", p, d[i])
		}
	}
	for p, d := range f.created {
		if i := onlyFrom(string(d), allowedOutput); i >= 0 {
			t.Fatalf("saved file %q has unexpected byte %q", p, d[i])
		}
		dir, base := filepath.Split(p)
		if (dir != "/cwd/" && dir != "/tmp/") || strings.HasPrefix(base, ".") || strings.HasPrefix(base, "-") ||
			!strings.HasSuffix(base, ".txt") || strings.ContainsAny(base, "/\\\x00\n\x1b") {
			t.Fatalf("saved to an unexpected place: %q", p)
		}
	}
	if f.term != nil {
		if f.termOpened > 0 && f.term.restored == 0 {
			t.Fatal("terminal was opened but never restored")
		}
		if f.interruptSet != f.interruptStop {
			t.Fatalf("interrupt handler set %d times, removed %d", f.interruptSet, f.interruptStop)
		}
		screen := terminalSequences.Replace(f.term.out.String())
		if i := printableASCII(screen); i >= 0 || strings.ContainsRune(screen, '\n') {
			t.Fatalf("terminal received an unexpected byte (at %d of %d)", i, len(screen))
		}
	}
	if err != nil {
		if msg := printable(err.Error()); strings.ContainsFunc(msg, func(c rune) bool { return c < ' ' || c == 0x7f }) {
			t.Fatalf("error message has control characters: %q", msg)
		}
	}
}

// ---- models used throughout ----------------------------------------------------

var (
	bracketSTL  = mustRead("testdata/bracket.stl")
	smallBinSTL = binarySTL([][9]float32{{0, 0, 0, 2, 0, 0, 0, 2, 0}, {0, 0, 0, 2, 0, 0, 0, 0, 2}, {0, 0, 0, 0, 2, 0, 0, 0, 2}, {2, 0, 0, 0, 2, 0, 0, 0, 2}})
	smallOBJ    = []byte("# tetrahedron\nv 0 0 0\nv 2 0 0\nv 0 2 0\nv 0 0 2\nf 1 2 3\nf 1 2 4\nf 1 3 4\nf 2 3 4\n")
	small3MF    = wrap3MF([]byte(`<model><resources><object id="1"><mesh><vertices><vertex x="0" y="0" z="0"/><vertex x="2" y="0" z="0"/><vertex x="0" y="2" z="0"/><vertex x="0" y="0" z="2"/></vertices><triangles><triangle v1="0" v2="1" v3="2"/><triangle v1="0" v2="1" v3="3"/><triangle v1="0" v2="2" v3="3"/><triangle v1="1" v2="2" v3="3"/></triangles></mesh></object></resources><build><item objectid="1"/></build></model>`))
)

func mustRead(p string) []byte {
	d, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return d
}

func testFiles() map[string][]byte {
	return map[string][]byte{
		"bracket.stl": bracketSTL, "bin.stl": smallBinSTL, "t.obj": smallOBJ, "t.3mf": small3MF,
		"94": smallOBJ, "-dash.stl": smallBinSTL, "noext": small3MF,
		"evil\x1b]0;PWNED\x07\x1b[2J.stl": smallBinSTL,
		"dir/../.hidden.stl":              smallBinSTL,
	}
}

func runCLI(t testing.TB, stdin []byte, args ...string) (*fake, error) {
	t.Helper()
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	saved := maxTriangles
	defer func() { maxTriangles = saved }()
	f := newFake(testFiles(), in)
	err := run(f.sys, args)
	f.check(t, err, false)
	return f, err
}

// ---- command line --------------------------------------------------------------

func TestStdinGivesTheSameRenderAsAFile(t *testing.T) {
	for name, data := range map[string][]byte{"bracket.stl": bracketSTL, "bin.stl": smallBinSTL, "t.obj": smallOBJ, "t.3mf": small3MF} {
		want, err := runCLI(t, nil, name, "40", "-20", "5", "1.2", "-s", "50x16")
		if err != nil || want.out.Len() == 0 {
			t.Fatalf("%s from file: %v", name, err)
		}
		for _, args := range [][]string{
			{"40", "-20", "5", "1.2", "-s", "50x16"},      // no file at all: stdin is the model
			{"-", "40", "-20", "5", "1.2", "-s", "50x16"}, // explicit
			{"-s", "50x16", "--", "-", "40", "-20", "5", "1.2"},
		} {
			got, err := runCLI(t, data, args...)
			if err != nil || got.out.String() != want.out.String() {
				t.Errorf("%s via stdin %v: err %v, output differs from the file render", name, args, err)
			}
		}
	}
}

func TestWhichArgumentIsTheFile(t *testing.T) {
	// a number that is also an existing file name is the file...
	if f, err := runCLI(t, nil, "94", "-p", "-s", "30x10"); err != nil || f.out.Len() == 0 {
		t.Errorf("file named 94: %v", err)
	}
	// ...even with data waiting on stdin
	if _, err := runCLI(t, []byte("not a model"), "94", "-p", "-s", "30x10"); err != nil {
		t.Errorf("file named 94 with stdin piped: %v", err)
	}
	// a number that is not a file, with nothing on stdin, is a mistake
	if _, err := runCLI(t, nil, "95", "0", "0"); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("want no such file, got %v", err)
	}
	// no arguments and a keyboard on stdin: show usage
	if _, err := runCLI(t, nil); err != errUsage {
		t.Errorf("want usage, got %v", err)
	}
	// a file whose name starts with a dash needs --
	if _, err := runCLI(t, nil, "-dash.stl", "-p"); err == nil {
		t.Error("-dash.stl without -- should be an unknown option")
	}
	if f, err := runCLI(t, nil, "-p", "-s", "30x10", "--", "-dash.stl"); err != nil || f.out.Len() == 0 {
		t.Errorf("-- -dash.stl: %v", err)
	}
	// a file with no extension is identified by content
	if f, err := runCLI(t, nil, "noext", "-p", "-s", "30x10"); err != nil || f.out.Len() == 0 {
		t.Errorf("noext: %v", err)
	}
}

func TestBadArgumentsAreRefused(t *testing.T) {
	for _, args := range [][]string{
		{"bin.stl", "1"}, {"bin.stl", "1", "2"}, {"bin.stl", "1", "2", "3", "4", "5"}, {"bin.stl", "1", "2", "3", "4", "5", "6", "7"},
		{"bin.stl", "0", "0", "0", "0"}, {"bin.stl", "0", "0", "0", "-1"}, {"bin.stl", "0", "0", "0", "1e300"}, {"bin.stl", "0", "0", "0", "1e-300"},
		{"bin.stl", "0", "0", "0", "1", "1e30", "0"}, {"bin.stl", "1e300", "0", "0"},
		{"bin.stl", "0", "0", "nan"}, {"bin.stl", "0", "0", "inf"}, {"bin.stl", "0", "0", "0x10"}, {"bin.stl", "0", "zero", "0"},
		{"bin.stl", "-s"}, {"bin.stl", "-s", ""}, {"bin.stl", "-s", "x"}, {"bin.stl", "-s", "0x0"}, {"bin.stl", "-s", "1x1"},
		{"bin.stl", "-s", "-5x5"}, {"bin.stl", "-s", "5x5x5"}, {"bin.stl", "-s", "99999999999999999999x2"}, {"bin.stl", "-s", "3000x10"},
		{"bin.stl", "-s", "10x2000"}, {"bin.stl", "-s", "1e2x10"}, {"bin.stl", "-s", "10 x 10 x"},
		{"bin.stl", "-m"}, {"bin.stl", "-m", "nope"}, {"bin.stl", "-m", ""},
		{"bin.stl", "-o"}, {"bin.stl", "-o", ""},
		{"bin.stl", "--crease"}, {"bin.stl", "--crease", "x"}, {"bin.stl", "--crease", "-1"}, {"bin.stl", "--crease", "1e9"},
		{"bin.stl", "--aspect", "0"}, {"bin.stl", "--aspect", "-2"}, {"bin.stl", "--aspect", "1e9"}, {"bin.stl", "--aspect", "nan"},
		{"bin.stl", "--max-triangles", "0"}, {"bin.stl", "--max-triangles", "1.5"}, {"bin.stl", "--max-triangles", "1e18"}, {"bin.stl", "--max-triangles", "-3"},
		{"bin.stl", "--max-triangles", "3"}, // the model has 4
		{"bin.stl", "--nope"}, {"bin.stl", "-x"}, {"bin.stl", "-pp"}, {"bin.stl", "extra.stl"},
		{"missing.stl"}, {""}, {"", "-p"}, {"-i", "-", "-p"},
	} {
		f, err := runCLI(t, nil, append(args, "-s", "30x10")[:len(args)+2*btoi(!contains(args, "-s"))]...)
		if err == nil {
			t.Errorf("%q: accepted, wanted an error", args)
		}
		if f.out.Len() > 0 || len(f.written) > 0 {
			t.Errorf("%q: produced output despite the error", args)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}

func TestOptionsDoWhatTheySay(t *testing.T) {
	f, err := runCLI(t, nil, "bracket.stl", "25", "-30", "0", "1", "-s", "60x20")
	if err != nil || f.out.String() != string(mustRead("testdata/bracket.expected.txt")) {
		t.Fatalf("golden render through the CLI: %v", err)
	}
	f, err = runCLI(t, nil, "bracket.stl", "-o", "out.txt", "-s", "40x12")
	if err != nil || f.out.Len() != 0 || strings.Count(string(f.written["out.txt"]), "\n") != 12 {
		t.Errorf("-o: err %v, stdout %d bytes, file %q", err, f.out.Len(), f.written)
	}
	f, _ = runCLI(t, nil, "bracket.stl", "-p", "-s", "40")
	if n := strings.Count(f.out.String(), "\n"); n < 5 || n > 20 || strings.HasPrefix(f.out.String(), "\n") {
		t.Errorf("width-only size gave %d rows", n)
	}
	for _, m := range modes {
		if f, err := runCLI(t, nil, "bracket.stl", "-p", "-s", "40x12", "-m", m); err != nil || strings.TrimSpace(f.out.String()) == "" {
			t.Errorf("mode %s: %v", m, err)
		}
	}
	for _, flag := range []string{"-h", "--help", "-V", "--version"} {
		fk := newFake(nil, nil)
		err := run(fk.sys, []string{flag, "ignored", "--nope"})
		fk.check(t, err, true)
		if err != nil || fk.out.Len() == 0 {
			t.Errorf("%s: %v", flag, err)
		}
	}
}

func TestFrameSizeFromTheEnvironmentIsClamped(t *testing.T) {
	saved := maxTriangles
	defer func() { maxTriangles = saved }()
	f := newFake(testFiles(), nil)
	f.env["FZF_PREVIEW_COLUMNS"], f.env["FZF_PREVIEW_LINES"] = "99999999", "99999999"
	err := run(f.sys, []string{"bin.stl", "-p"})
	f.check(t, err, false)
	lines := strings.Split(strings.TrimRight(f.out.String(), "\n"), "\n")
	if err != nil || len(lines) > maxRows {
		t.Fatalf("err %v, %d rows", err, len(lines))
	}
	f = newFake(testFiles(), nil)
	f.termCols, f.termRows, f.stdoutTTY = 65535, 65535, true
	err = run(f.sys, []string{"bin.stl", "-p"})
	f.check(t, err, false)
	if err != nil || strings.Count(f.out.String(), "\n") > maxRows {
		t.Fatalf("terminal-sized print: %v", err)
	}
}

// ---- stdin detection -------------------------------------------------------------

// countingReader records how much was asked of it, and can go on forever.
type countingReader struct {
	head   []byte
	fill   byte
	read   int
	finite bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.finite {
		if c.read >= len(c.head) {
			return 0, io.EOF
		}
		p = p[:min(len(p), len(c.head)-c.read)]
	}
	for i := range p {
		if c.read+i < len(c.head) {
			p[i] = c.head[c.read+i]
		} else {
			p[i] = c.fill
		}
	}
	c.read += len(p)
	return len(p), nil
}

func TestStdinIsJudgedFromItsFirstBytes(t *testing.T) {
	limits(t, 1000, maxInstances, maxRenderWork)

	// endless garbage is refused after the sniff window, not read to the limit
	junk := &countingReader{head: []byte("GIF89a this is not a model at all\n"), fill: 'A'}
	_, err := loadModel(junk, "", -1)
	wantError(t, err, "not an STL, OBJ or 3MF")
	if junk.read > sniffBytes {
		t.Errorf("read %d bytes of garbage before refusing", junk.read)
	}

	// a binary STL is read to exactly the length its header declares
	stl := &countingReader{head: binarySTL(make([][9]float32, 900)), fill: 0xAA}
	if _, err := loadModel(stl, "", -1); err != nil {
		t.Fatal(err)
	}
	if want := max(84+50*900, sniffBytes); stl.read != want {
		t.Errorf("read %d bytes of a %d-byte STL", stl.read, want)
	}

	// a header promising more triangles than allowed is refused straight away
	big := &countingReader{head: binarySTL(make([][9]float32, 1)), fill: 0}
	big.head[80], big.head[81], big.head[82] = 0xff, 0xff, 0xff
	_, err = loadModel(big, "", -1)
	wantError(t, err, "triangles")
	if big.read > sniffBytes {
		t.Errorf("read %d bytes before refusing an oversized STL", big.read)
	}

	// endless text that looks like a model stops at the input limit
	obj := &countingReader{head: []byte("v 0 0 0\nv 1 0 0\nv 0 1 0\n"), fill: '\n'}
	_, err = loadModel(obj, "", -1)
	wantError(t, err, "larger than")
	if int64(obj.read) > maxInputBytes()+sniffBytes {
		t.Errorf("read %d bytes, limit is %d", obj.read, maxInputBytes())
	}

	// a truncated binary STL says so
	short := &countingReader{head: binarySTL(make([][9]float32, 900))[:20000], finite: true}
	_, err = loadModel(short, "", -1)
	wantError(t, err, "truncated")
}

func TestSniff(t *testing.T) {
	for _, c := range []struct {
		data []byte
		want format
	}{
		{bracketSTL, fmtSTLASCII}, {smallBinSTL, fmtSTLBinary}, {smallOBJ, fmtOBJ}, {small3MF, fmt3MF},
		{[]byte("   \n SOLID x\n FACET normal 0 0 0\n"), fmtSTLASCII},
		{[]byte("solid but really just prose, long enough to pass any length check you care to make, honest"), fmtUnknown},
		{[]byte("# only comments\nvn 0 0 1\n"), fmtUnknown},
		{[]byte("v one two three\n"), fmtUnknown},
		{[]byte("<html><body>v 1 2 3</body></html>"), fmtUnknown},
		{[]byte("#!/bin/sh\nrm -rf /\n"), fmtUnknown},
		{nil, fmtUnknown}, {[]byte{0}, fmtUnknown}, {make([]byte, 83), fmtUnknown}, {make([]byte, 84), fmtSTLBinary},
		{[]byte("PK\x03\x04"), fmt3MF}, {[]byte("PK\x05\x06"), fmtUnknown},
	} {
		if got := sniff(c.data); got != c.want {
			t.Errorf("sniff(%.30q) = %d, want %d", c.data, got, c.want)
		}
	}
}

// ---- interactive mode ------------------------------------------------------------

func interactiveRun(t testing.TB, name string, cols, rows int, keys ...string) (*fake, error) {
	t.Helper()
	saved := maxTriangles
	defer func() { maxTriangles = saved }()
	f := newFake(testFiles(), nil)
	f.stdoutTTY = true
	f.term = &fakeTerm{cols: cols, rows: rows}
	for _, k := range keys {
		f.term.keys = append(f.term.keys, []byte(k))
	}
	err := run(f.sys, []string{"--", name})
	f.check(t, err, false)
	return f, err
}

func TestInteractiveKeysAndSave(t *testing.T) {
	f, err := interactiveRun(t, "bracket.stl", 100, 30, "ddww", "ii", "DW", "m", "p", "p", "\x1b[A\x1b[D", "?", "q", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 2 || f.created["/cwd/bracket.rndr.txt"] == nil || f.created["/cwd/bracket.rndr-2.txt"] == nil {
		t.Fatalf("saved files: %v", keysOf(f.created))
	}
	screen := f.term.out.String()
	for _, want := range []string{"view 15 -20 0 1.322 0.1 0.1", "| wire |", "ws tilt"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen never showed %q", want)
		}
	}
	// the saved frame is reproducible from the numbers the status bar showed
	again, err := runCLI(t, nil, "bracket.stl", "15", "-20", "0", "1.322", "0.1", "0.1", "-m", "wire", "-s", "100x29")
	if err != nil || again.out.String() != string(f.created["/cwd/bracket.rndr.txt"]) {
		t.Errorf("CLI does not reproduce the saved frame (err %v)", err)
	}
}

func keysOf(m map[string][]byte) (k []string) {
	for s := range m {
		k = append(k, s)
	}
	return
}

func TestInteractiveSurvivesOddTerminals(t *testing.T) {
	for _, sz := range [][2]int{{1, 1}, {2, 1}, {1, 2}, {2, 2}, {3, 3}, {65535, 65535}, {100000000, 3}, {5, 100000000}} {
		if _, err := interactiveRun(t, "bin.stl", sz[0], sz[1], "wasdWASDiozxmr?p", "q"); err != nil {
			t.Errorf("%dx%d: %v", sz[0], sz[1], err)
		}
	}
	for _, sz := range [][2]int{{0, 0}, {-5, 10}, {10, -5}} {
		f, err := interactiveRun(t, "bin.stl", sz[0], sz[1], "q")
		if err == nil || f.term.restored == 0 {
			t.Errorf("%dx%d: want an error and a restored terminal, got %v", sz[0], sz[1], err)
		}
	}
	// keys end without a quit (terminal closed)
	if _, err := interactiveRun(t, "bin.stl", 80, 24); err != nil {
		t.Error(err)
	}
	// model on stdin, keys from the terminal
	saved := maxTriangles
	defer func() { maxTriangles = saved }()
	f := newFake(nil, bytes.NewReader(smallOBJ))
	f.stdoutTTY, f.term = true, &fakeTerm{cols: 80, rows: 24, keys: [][]byte{[]byte("dq")}}
	err := run(f.sys, nil)
	f.check(t, err, false)
	if keysFromTTY && (err != nil || f.termOpened != 1) {
		t.Errorf("stdin model, interactive: %v", err)
	}
}

func TestHostileFileNameInInteractiveMode(t *testing.T) {
	name := "evil\x1b]0;PWNED\x07\x1b[2J.stl"
	f, err := interactiveRun(t, name, 120, 30, "p", "q")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.term.out.String(), "\x1b]0;") || strings.Contains(f.term.out.String(), "\x07") {
		t.Fatal("escape sequence from the file name reached the terminal")
	}
	if len(f.created) != 1 {
		t.Fatalf("saved: %q", keysOf(f.created))
	}
}

func TestSaveNameCannotEscapeOrHide(t *testing.T) {
	for in, want := range map[string]string{
		"model.stl": "model", "../../etc/passwd": "passwd", "/etc/shadow.stl": "shadow", `C:\Windows\win.ini`: "win",
		".bashrc": "model", ".config.stl": "config", "..": "model", ".": "model", "": "model", "-rf.stl": "rf", "--help": "help", "...---x.obj": "x",
		"a\x00b\nc\x1b[2J.stl": "a_b_c_[2J", "con:aux.stl": "con_aux", strings.Repeat("n", 500) + ".stl": strings.Repeat("n", 100),
		"stdin": "stdin",
	} {
		if got := saveName(in); got != want {
			t.Errorf("saveName(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- fuzzing ---------------------------------------------------------------------

// fuzzSeeds adds real model files as seeds when RNDR_FUZZ_SEEDS names a
// directory of them (not part of the repository; used for long local runs).
func fuzzSeeds(add func([]byte), exts ...string) {
	dir := os.Getenv("RNDR_FUZZ_SEEDS")
	if dir == "" {
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		for _, ext := range exts {
			if strings.HasSuffix(strings.ToLower(e.Name()), ext) {
				if d, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && len(d) <= 256<<10 {
					add(d)
				}
			}
		}
	}
}

// promptly fails a fuzz input that takes far longer than the lowered limits
// should allow: a slow input is a denial-of-service finding, not just a crash.
func promptly(t *testing.T, what string, f func()) {
	start := time.Now()
	f()
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("%s took %v", what, d)
	}
}

func fuzzFrameLimits(f *testing.F) {
	fuzzLimits(f)
	c, r := maxCols, maxRows
	maxCols, maxRows = 240, 100
	f.Cleanup(func() { maxCols, maxRows = c, r })
}

// FuzzArgs feeds arbitrary argument lists (and arbitrary stdin) to the program.
func FuzzArgs(f *testing.F) {
	fuzzFrameLimits(f)
	for _, s := range []string{
		"bracket.stl\x00-p", "bin.stl\x0025\x00-30\x000\x001.5\x000.1\x00-0.2\x00-s\x0040x12\x00-m\x00wire",
		"t.obj\x00-s\x0060\x00-o\x00out.txt\x00--invert", "t.3mf\x00-p\x00--crease\x0045\x00--aspect\x001.8\x00--max-triangles\x00100",
		"-\x0010\x0020\x0030", "10\x0020\x0030\x002", "-p", "", "-i\x00bin.stl", "--\x00-dash.stl\x00-p", "94\x00-p", "noext\x00-p\x00-s\x00x20",
		"-h", "--version", "bin.stl\x00-s\x00240x100\x00-m\x00mesh\x000\x000\x000\x001e6", "bin.stl\x00-m", "bin.stl\x00--nope\x00-s",
		"evil\x1b]0;PWNED\x07\x1b[2J.stl\x00-p", "missing\x1b[2J.stl", "bin.stl\x00-o\x00../../etc/passwd\x00-p",
	} {
		f.Add([]byte(s), uint8(0), []byte(nil))
		f.Add([]byte(s), uint8(7), smallOBJ)
		f.Add([]byte(s), uint8(2), smallBinSTL)
	}
	f.Fuzz(func(t *testing.T, raw []byte, flags uint8, stdin []byte) {
		args := strings.Split(string(raw), "\x00")
		if len(raw) == 0 {
			args = nil
		}
		if len(args) > 24 {
			args = args[:24]
		}
		var in io.Reader
		if flags&1 != 0 {
			in = bytes.NewReader(stdin)
		}
		saved := maxTriangles
		defer func() { maxTriangles = saved }()
		fk := newFake(testFiles(), in)
		fk.stdoutTTY = flags&2 != 0
		if flags&4 != 0 {
			fk.term = &fakeTerm{cols: 20 + int(flags>>3), rows: 6 + int(flags>>4), keys: [][]byte{[]byte("dwip"), []byte("m?q")}}
			fk.termCols, fk.termRows = fk.term.cols, fk.term.rows
		}
		var err error
		promptly(t, "run", func() { err = run(fk.sys, args) })
		info := err == nil && fk.out.Len() > 0 && (strings.HasPrefix(fk.out.String(), "usage:") || strings.HasPrefix(fk.out.String(), "rndr "))
		fk.check(t, err, info)
		if err != nil && (fk.out.Len() > 0 || len(fk.written) > 0) {
			t.Fatalf("failed with %v but still produced output", err)
		}
	})
}

// FuzzKeys types arbitrary bytes into the interactive viewer on arbitrary terminals.
func FuzzKeys(f *testing.F) {
	fuzzFrameLimits(f)
	for _, k := range []string{"wasdWASDiozxmr0?hp", "\x1b[A\x1b[B\x1b[C\x1b[D\x1bOA", "\x1b", "\x1b\x1b[", "pppppppp", "iiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiiii",
		"oooooooooooooooooooooooooooooooooooooooooooooooooo", "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDmmmmm", "\x03", "\x04", "q", "", "\x00\xff\x1b[Z\x7f"} {
		f.Add([]byte(k), uint16(80), uint16(24), uint8(3), uint8(0))
		f.Add([]byte(k), uint16(1), uint16(1), uint8(1), uint8(1))
		f.Add([]byte(k), uint16(65535), uint16(65535), uint8(200), uint8(2))
	}
	models := []string{"bracket.stl", "bin.stl", "t.obj", "t.3mf", "evil\x1b]0;PWNED\x07\x1b[2J.stl", "dir/../.hidden.stl"}
	f.Fuzz(func(t *testing.T, keys []byte, cols, rows uint16, chunk, model uint8) {
		if len(keys) > 400 {
			keys = keys[:400]
		}
		saved := maxTriangles
		defer func() { maxTriangles = saved }()
		fk := newFake(testFiles(), nil)
		fk.stdoutTTY = true
		fk.term = &fakeTerm{cols: int(cols), rows: int(rows)}
		for n := max(1, int(chunk)); len(keys) > 0; {
			n = min(n, len(keys))
			fk.term.keys, keys = append(fk.term.keys, keys[:n]), keys[n:]
		}
		var err error
		promptly(t, "interactive session", func() { err = run(fk.sys, []string{"--", models[int(model)%len(models)]}) })
		fk.check(t, err, false)
		if (cols == 0 || rows == 0) != (err != nil) {
			t.Fatalf("%dx%d terminal: err = %v", cols, rows, err)
		}
	})
}

// FuzzView draws fixed models with arbitrary view numbers, as if typed on the command line.
func FuzzView(f *testing.F) {
	fuzzFrameLimits(f)
	f.Add(25.0, -30.0, 0.0, 1.0, 0.0, 0.0, 2.0, 30.0, uint16(80), uint16(24), uint8(0))
	f.Add(-1e9, 1e9, 359.999, 1e6, 1e6, -1e6, 0.2, 0.0, uint16(240), uint16(100), uint8(7))
	f.Add(90.0, 90.0, 90.0, 1e-6, -1e6, 1e6, 10.0, 180.0, uint16(2), uint16(2), uint8(11))
	f.Add(math.SmallestNonzeroFloat64, -0.0, 1e-300, 0.999999, 0.5, 0.5, 1.0, 29.9999, uint16(3), uint16(99), uint8(19))
	meshes := map[uint64]*Mesh{}
	datas := [][]byte{bracketSTL, smallBinSTL, smallOBJ, small3MF}
	f.Fuzz(func(t *testing.T, rx, ry, rz, zoom, px, py, aspect, crease float64, cols, rows uint16, sel uint8) {
		finite := func(v ...float64) bool {
			for _, x := range v {
				if math.IsNaN(x) || math.IsInf(x, 0) {
					return false
				}
			}
			return true
		}
		// the ranges parseArgs accepts; anything else never reaches the renderer
		if !finite(rx, ry, rz, zoom, px, py, aspect, crease) || math.Abs(rx) > 1e9 || math.Abs(ry) > 1e9 || math.Abs(rz) > 1e9 ||
			zoom < 1e-6 || zoom > 1e6 || math.Abs(px) > 1e6 || math.Abs(py) > 1e6 || aspect < 0.2 || aspect > 10 || crease < 0 || crease > 180 ||
			cols < 2 || rows < 2 || int(cols) > maxCols || int(rows) > maxRows {
			return
		}
		which := int(sel) % len(datas)
		key := uint64(which)<<32 | uint64(math.Float32bits(float32(crease)))
		m := meshes[key]
		if m == nil {
			soup, err := loadTriangles([]string{"a.stl", "b.stl", "c.obj", "d.3mf"}[which], datas[which])
			if err != nil {
				t.Fatal(err)
			}
			if m, err = buildMesh(soup, crease); err != nil {
				t.Fatal(err)
			}
			if len(meshes) < 64 {
				meshes[key] = m
			}
		}
		r := &Renderer{m: m, aspect: aspect, invert: sel&0x80 != 0}
		v := &View{rotX: rx, rotY: ry, rotZ: rz, zoom: zoom, panX: px, panY: py, mode: int(sel>>2) % len(modes)}
		promptly(t, "render", func() {
			text, err := r.FrameText(v, int(cols), int(rows))
			if err != nil && err != errTooExpensive {
				t.Fatal(err)
			}
			if i := onlyFrom(text, allowedOutput); i >= 0 {
				t.Fatalf("output has unexpected byte %q", text[i])
			}
			if err == nil && strings.Count(text, "\n") != int(rows) {
				t.Fatalf("asked for %d rows, got %d", rows, strings.Count(text, "\n"))
			}
		})
	})
}

// chunkReader hands out data a few bytes at a time, as a pipe might.
type chunkReader struct {
	d []byte
	n int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.d) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), c.n)], c.d)
	c.d = c.d[n:]
	return n, nil
}

// FuzzStdin pipes arbitrary bytes in, in arbitrary-sized pieces, with no file name to go on.
func FuzzStdin(f *testing.F) {
	fuzzFrameLimits(f)
	for _, d := range [][]byte{bracketSTL, smallBinSTL, smallOBJ, small3MF, wrap3MF([]byte(seedModel)), []byte("solid x\nfacet\n"), []byte("PK\x03\x04"), make([]byte, 84), nil} {
		f.Add(d, uint16(1))
		f.Add(d, uint16(4096))
	}
	fuzzSeeds(func(d []byte) { f.Add(d, uint16(7)); f.Add(d, uint16(65535)) }, ".stl", ".obj", ".3mf")
	f.Fuzz(func(t *testing.T, d []byte, chunk uint16) {
		saved := maxTriangles
		defer func() { maxTriangles = saved }()
		fk := newFake(nil, &chunkReader{d: d, n: max(1, int(chunk))})
		var err error
		promptly(t, "run", func() { err = run(fk.sys, []string{"20", "30", "40", "-s", "40x12"}) })
		fk.check(t, err, false)
		// whatever the pieces, the answer must match reading it all at once
		whole := newFake(nil, bytes.NewReader(d))
		err2 := run(whole.sys, []string{"20", "30", "40", "-s", "40x12"})
		if (err == nil) != (err2 == nil) || fk.out.String() != whole.out.String() {
			t.Fatalf("chunked read disagrees with whole read: %v vs %v", err, err2)
		}
	})
}

// FuzzSTLBinary builds a well-formed binary STL around arbitrary triangle bytes,
// so mutations reach the mesh builder and renderer instead of dying at the header.
func FuzzSTLBinary(f *testing.F) {
	fuzzLimits(f)
	f.Add(smallBinSTL[84:])
	f.Add(bytes.Repeat([]byte{0xff}, 500))
	f.Fuzz(func(t *testing.T, body []byte) {
		n := len(body) / 50
		d := make([]byte, 84, 84+n*50)
		d[80], d[81], d[82], d[83] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
		promptly(t, "pipeline", func() { pipeline(t, "x.stl", append(d, body[:n*50]...)) })
	})
}
