// rndr - ASCII previewer for STL, OBJ and 3MF models in the terminal.
//
// Standard library only: one static binary, no runtime, no external programs.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defRX, defRY, defRZ = 25.0, -30.0, 0.0
	rotStep             = 5.0
	panStep             = 0.1
	zoomStep            = 1.15
)

// version is stamped in by build.sh (-ldflags -X main.version=...).
var version = "dev"

const usage = `usage: rndr FILE                          interactive viewer
       rndr FILE RX RY RZ [ZOOM [PANX PANY]]   print one frame to stdout

  FILE       an .stl, .obj or .3mf model; - reads one from stdin (print mode only)
  RX RY RZ   tilt, spin, roll: degrees about the screen axes (X right, Y up, Z toward you),
             applied Z first, then Y, then X. 0 0 0 is the front view with model Z up.
  ZOOM       1.0 fits the whole model in the frame
  PANX PANY  shift, in units of the model's radius

options:
  -p, --print          print one frame of the default view instead of going interactive
  -i, --interactive    go interactive even when a view is given
  -s, --size COLSxROWS frame size (default: the terminal, or 80x24)
             COLS      just a width: as many rows as the model needs, no blank rows
  -m, --mode MODE      both (shaded + edges, default), wire (edges, hidden lines removed),
                       mesh (every triangle), xray (see-through mesh), shade
  -o, --out FILE       write the frame to FILE instead of stdout
      --crease DEG     angle at which an edge counts as a feature edge (default 30)
      --aspect N       height/width of a character cell (default 2.0)
      --invert         reverse the shading ramp (for dark-on-light output)
  -V, --version        print the version

interactive keys:
  w s       tilt            a d   spin           z x   roll   (arrows work too)
  W A S D   pan             i o   zoom in / out
  m         cycle mode      r     reset view
  p         save the frame to FILE.rndr.txt in the current directory
  ?         key help        q     quit
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rndr:", err)
		os.Exit(1)
	}
}

func parseNum(s string) (float64, bool) {
	if s == "" || !strings.ContainsRune("+-.0123456789", rune(s[0])) {
		return 0, false
	}
	d, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(d) || math.IsInf(d, 0) {
		return 0, false
	}
	return d, true
}

func run(argv []string) error {
	var file, out, size string
	var haveFile, forcePrint, forceInteractive, invert bool
	var nums []float64
	aspect, crease := 2.0, 30.0
	view := &View{rotX: defRX, rotY: defRY, rotZ: defRZ, zoom: 1}

	need := func(i int, opt string) (string, error) {
		if i >= len(argv) {
			return "", fmt.Errorf("%s needs a value", opt)
		}
		return argv[i], nil
	}
	needNum := func(i int, opt string) (float64, error) {
		s, err := need(i, opt)
		if err != nil {
			return 0, err
		}
		d, ok := parseNum(s)
		if !ok {
			return 0, fmt.Errorf("%s needs a number", opt)
		}
		return d, nil
	}

	for i := 0; i < len(argv); i++ {
		a := argv[i]
		var err error
		if d, ok := parseNum(a); ok && haveFile {
			nums = append(nums, d)
			continue
		}
		switch {
		case a == "-h" || a == "--help":
			fmt.Print(usage)
			return nil
		case a == "-V" || a == "--version":
			fmt.Println("rndr", version)
			return nil
		case a == "-p" || a == "--print":
			forcePrint = true
		case a == "-i" || a == "--interactive":
			forceInteractive = true
		case a == "--invert":
			invert = true
		case a == "-s" || a == "--size":
			i++
			size, err = need(i, a)
		case a == "-o" || a == "--out":
			i++
			out, err = need(i, a)
		case a == "-m" || a == "--mode":
			i++
			var m string
			if m, err = need(i, a); err == nil {
				view.mode = -1
				for k, name := range modes {
					if name == m {
						view.mode = k
					}
				}
				if view.mode < 0 {
					err = fmt.Errorf("unknown mode '%s' (both, wire, mesh, xray, shade)", m)
				}
			}
		case a == "--crease":
			i++
			crease, err = needNum(i, a)
		case a == "--aspect":
			i++
			if aspect, err = needNum(i, a); err == nil && !(aspect > 0.1 && aspect < 10) {
				err = errors.New("--aspect out of range")
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			err = fmt.Errorf("unknown option %s (try --help)", a)
		case !haveFile:
			file, haveFile = a, true
		default:
			err = fmt.Errorf("expected a number, got '%s' (try --help)", a)
		}
		if err != nil {
			return err
		}
	}
	if !haveFile {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch len(nums) {
	case 0, 3, 4, 6:
	default:
		return errors.New("expected RX RY RZ [ZOOM [PANX PANY]]")
	}
	if len(nums) >= 3 {
		view.rotX, view.rotY, view.rotZ = nums[0], nums[1], nums[2]
	}
	if len(nums) >= 4 {
		view.zoom = nums[3]
	}
	if len(nums) >= 6 {
		view.panX, view.panY = nums[4], nums[5]
	}
	if !(view.zoom > 0) {
		return errors.New("zoom must be greater than 0")
	}

	tty := stdoutIsTerminal()
	interactive := forceInteractive || (len(nums) == 0 && !forcePrint && out == "" && tty && file != "-")
	if interactive && file == "-" {
		return errors.New("reading from stdin only works in print mode")
	}

	var data []byte
	var err error
	name := "stdin"
	if file == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		name = filepath.Base(file)
		if st, serr := os.Stat(file); serr != nil || st.IsDir() {
			return fmt.Errorf("%s: no such file", file)
		}
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return fmt.Errorf("%s: %v", file, err)
	}
	soup, err := loadTriangles(name, data)
	if err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	data = nil
	mesh, err := buildMesh(soup, crease)
	if err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	soup = nil
	r := &Renderer{m: mesh, aspect: aspect, invert: invert}

	if interactive {
		return runInteractive(r, view, name)
	}

	var cols, rows int
	cropRows := false
	if size != "" {
		// COLSxROWS, or just COLS (or xROWS): the other side is sized to fit the model
		p := strings.Split(strings.ToLower(size), "x")
		bad := len(p) > 2
		side := func(k int) int {
			if k >= len(p) || strings.TrimSpace(p[k]) == "" {
				return 0
			}
			n, err := strconv.Atoi(strings.TrimSpace(p[k]))
			if err != nil || n < 0 {
				bad = true
			}
			return n
		}
		cols, rows = side(0), side(1)
		if bad || (cols == 0 && rows == 0) {
			return errors.New("--size wants COLSxROWS or just COLS, e.g. 100x40 or 100")
		}
		// a visually square frame is exactly what zoom 1.0 fills
		if rows == 0 {
			rows = int(math.Ceil(float64(cols) / aspect))
			cropRows = true
		}
		if cols == 0 {
			cols = int(math.Ceil(float64(rows) * aspect))
		}
	} else {
		cols, rows = defaultSize(tty && out == "")
	}
	if cols < 2 || rows < 2 || cols*rows > 4000000 {
		return errors.New("unreasonable frame size")
	}
	frame := r.FrameText(view, cols, rows)
	if cropRows {
		frame = strings.Trim(frame, "\n") + "\n"
	}
	if out != "" {
		if err := os.WriteFile(out, []byte(frame), 0o644); err != nil {
			return err
		}
		return nil
	}
	_, err = os.Stdout.WriteString(frame)
	return err
}

func envInt(name string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	return n
}

// defaultSize picks the print-mode frame: fzf preview window, then the terminal,
// then COLUMNS/LINES, then 80x24.
func defaultSize(leaveRoomForPrompt bool) (cols, rows int) {
	if c, r := envInt("FZF_PREVIEW_COLUMNS"), envInt("FZF_PREVIEW_LINES"); c > 0 && r > 0 {
		return c, r
	}
	if c, r, ok := terminalSize(); ok {
		if leaveRoomForPrompt {
			r--
		}
		return c, max(2, r)
	}
	if c, r := envInt("COLUMNS"), envInt("LINES"); c > 0 && r > 0 {
		return c, r
	}
	return 80, 24
}

func num(d float64, places int) string {
	s := strconv.FormatFloat(d, 'f', places, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		return "0"
	}
	return s
}

// viewArgs is the argument list that reproduces the view in print mode.
func viewArgs(v *View) string {
	s := num(v.rotX, 1) + " " + num(v.rotY, 1) + " " + num(v.rotZ, 1) + " " + num(v.zoom, 3)
	if v.panX != 0 || v.panY != 0 {
		s += " " + num(v.panX, 3) + " " + num(v.panY, 3)
	}
	return s
}

func round3(d float64) float64 { return math.Round(d*1000) / 1000 }

// wrap folds an angle into (-180, 180].
func wrap(d float64) float64 {
	d = math.Remainder(d, 360)
	if d <= -180 {
		d += 360
	}
	return d
}

// ---- interactive mode --------------------------------------------------------

const helpLine = " ws tilt | ad spin | zx roll | WASD pan | i/o zoom | m mode | r reset | p save | ? help | q quit"

func runInteractive(r *Renderer, v *View, name string) error {
	t, err := openRaw()
	if err != nil {
		return fmt.Errorf("interactive mode needs a terminal (%v); use -p to print a frame instead", err)
	}
	cols, rows, ok := t.Size()
	if !ok {
		t.Restore()
		return errors.New("cannot read the terminal size; use -p to print a frame instead")
	}
	t.Write([]byte("\x1b[?1049h\x1b[?25l"))
	leave := func() {
		t.Write([]byte("\x1b[0m\x1b[?25h\x1b[?1049l"))
		t.Restore()
	}
	defer leave()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	keys := make(chan []byte)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := t.Read(buf)
			if n > 0 {
				keys <- append([]byte(nil), buf[:n]...)
			}
			if err != nil || n == 0 {
				close(keys)
				return
			}
		}
	}()
	resize := time.NewTicker(250 * time.Millisecond)
	defer resize.Stop()

	msg, help, dirty := "", false, true
	for {
		if dirty {
			status := msg
			if help {
				status = helpLine
			}
			draw(t, r, v, name, cols, rows, status)
			dirty = false
		}
		select {
		case <-sigs:
			return nil
		case <-resize.C:
			if c, rw, ok := t.Size(); ok && (c != cols || rw != rows) {
				cols, rows, dirty = c, rw, true
			}
		case b, open := <-keys:
			if !open {
				return nil
			}
			msg = ""
			for i := 0; i < len(b); i++ {
				k := b[i]
				if k == 27 {
					if i+2 < len(b) && (b[i+1] == '[' || b[i+1] == 'O') {
						k = 0
						if arrow := strings.IndexByte("ABCD", b[i+2]); arrow >= 0 { // up, down, right, left
							k = "wsda"[arrow]
						}
						i += 2
					} else if len(b) == 1 {
						return nil
					} else {
						continue
					}
				}
				switch k {
				case 'w':
					v.rotX -= rotStep
				case 's':
					v.rotX += rotStep
				case 'a':
					v.rotY -= rotStep
				case 'd':
					v.rotY += rotStep
				case 'z':
					v.rotZ += rotStep
				case 'x':
					v.rotZ -= rotStep
				case 'W':
					v.panY += panStep
				case 'S':
					v.panY -= panStep
				case 'A':
					v.panX -= panStep
				case 'D':
					v.panX += panStep
				case 'i', '+', '=':
					v.zoom = math.Min(500, v.zoom*zoomStep)
				case 'o', '-':
					v.zoom = math.Max(0.02, v.zoom/zoomStep)
				case 'm':
					v.mode = (v.mode + 1) % len(modes)
				case 'r', '0':
					v.rotX, v.rotY, v.rotZ = defRX, defRY, defRZ
					v.zoom, v.panX, v.panY = 1, 0, 0
				case 'p':
					msg = save(r.FrameText(v, cols, max(1, rows-1)), name, v)
				case '?', 'h':
					help = !help
				case 'q', 3, 4:
					return nil
				}
				v.rotX, v.rotY, v.rotZ = wrap(v.rotX), wrap(v.rotY), wrap(v.rotZ)
				// keep the view exactly what the status bar shows, so those numbers reproduce it
				v.zoom, v.panX, v.panY = round3(v.zoom), round3(v.panX), round3(v.panY)
			}
			dirty = true
		}
	}
}

func draw(t *rawTerm, r *Renderer, v *View, name string, cols, rows int, msg string) {
	vr := max(1, rows-1)
	g := r.Render(v, cols, vr)
	var sb bytes.Buffer
	sb.Grow(cols*rows + rows*8 + 64)
	sb.WriteString("\x1b[H")
	for row := 0; row < vr; row++ {
		sb.Write(bytes.TrimRight(g[row*cols:(row+1)*cols], " "))
		sb.WriteString("\x1b[K\r\n")
	}
	status := msg
	if status == "" {
		m := r.m
		status = fmt.Sprintf(" %s | %d tris | %s x %s x %s | %s | view %s | ? help",
			name, m.nt, num(m.dimX, 1), num(m.dimY, 1), num(m.dimZ, 1), modes[v.mode], viewArgs(v))
	}
	line := make([]byte, 0, cols)
	for i := 0; i < len(status) && len(line) < cols-1; i++ {
		ch := status[i]
		if ch < ' ' || ch >= 127 {
			ch = '?'
		}
		line = append(line, ch)
	}
	for len(line) < cols-1 {
		line = append(line, ' ')
	}
	sb.WriteString("\x1b[7m")
	sb.Write(line)
	sb.WriteString("\x1b[0m\x1b[K")
	t.Write(sb.Bytes())
}

// save writes the frame next to where you are (or to the temp dir if that is read-only).
func save(text, name string, v *View) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	cwd, _ := os.Getwd()
	var lastErr error
	for _, dir := range []string{cwd, os.TempDir()} {
		for n := 1; n < 1000; n++ {
			suffix := ""
			if n > 1 {
				suffix = "-" + strconv.Itoa(n)
			}
			p := filepath.Join(dir, base+".rndr"+suffix+".txt")
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if os.IsExist(err) {
				continue
			}
			if err != nil {
				lastErr = err
				break
			}
			_, err = f.WriteString(text)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				lastErr = err
				break
			}
			return " saved " + p + "   (rndr " + name + " " + viewArgs(v) + ")"
		}
	}
	return fmt.Sprintf(" save failed: %v", lastErr)
}
