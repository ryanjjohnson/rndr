package main

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// The largest frame, whatever the terminal or -s claims. (Variables so tests can lower them.)
var maxCols, maxRows = 2000, 1000

const (
	defRX, defRY, defRZ = 25.0, -30.0, 0.0
	rotStep             = 5.0
	panStep             = 0.1
	zoomStep            = 1.15
)

const usage = `usage: rndr FILE                          interactive viewer
       rndr FILE RX RY RZ [ZOOM [PANX PANY]]   print one frame to stdout
       ... | rndr [RX RY RZ ...]          read the model from stdin

  FILE       an .stl, .obj or .3mf model. With no FILE (or -) the model is read from
             stdin and its type is detected from the data.
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
  -c, --chars STYLE    what to draw with: ascii (default, works everywhere),
                       braille (2x4 dots per character: 8x the detail, still plain text),
                       blocks (2 grey pixels per character; needs a colour terminal)
  -o, --out FILE       write the frame to FILE instead of stdout
      --crease DEG     angle at which an edge counts as a feature edge (default 30)
      --aspect N       height/width of a character cell (default 2.0)
      --invert         reverse the shading, for dark-on-light output (ascii and braille;
                       blocks looks the same on any background)
      --max-triangles N  refuse models with more triangles than this (default 5000000);
                       this is what bounds memory: about 300 bytes per triangle
  -V, --version        print the version
  --                   everything after this is a FILE or a number, never an option

interactive keys:
  w s       tilt            a d   spin           z x   roll   (arrows work too)
  W A S D   pan             i o   zoom in / out
  m         cycle mode      c     cycle ascii / braille / blocks
  r         reset view
  p         save the frame to FILE.rndr.txt in the current directory
  ?         key help        q     quit
`

// options is a parsed command line.
type options struct {
	file      string // "" when the model comes from stdin
	useStdin  bool
	out, size string
	sizeGiven bool
	help      bool
	version   bool

	forcePrint, forceInteractive bool
	viewGiven                    bool // a rotation was given, which implies print mode

	view           View
	aspect, crease float64
	invert         bool
	maxTriangles   int
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

// parseArgs turns the command line into options. It reads nothing but its
// arguments: exists says whether a name is a file, stdinPiped whether stdin
// has data rather than a keyboard on it.
func parseArgs(argv []string, exists func(string) bool, stdinPiped bool) (*options, error) {
	o := &options{
		view:         View{rotX: defRX, rotY: defRY, rotZ: defRZ, zoom: 1},
		aspect:       2,
		crease:       30,
		maxTriangles: maxTriangles,
	}
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

	var pos []string // everything that is not an option: the file and the view numbers
	optionsDone := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		var err error
		if _, isNum := parseNum(a); isNum || optionsDone || a == "-" || !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		switch a {
		case "--":
			optionsDone = true
		case "-h", "--help":
			o.help = true
			return o, nil
		case "-V", "--version":
			o.version = true
			return o, nil
		case "-p", "--print":
			o.forcePrint = true
		case "-i", "--interactive":
			o.forceInteractive = true
		case "--invert":
			o.invert = true
		case "-s", "--size":
			i++
			o.size, err = need(i, a)
			o.sizeGiven = true
		case "-o", "--out":
			i++
			if o.out, err = need(i, a); err == nil && o.out == "" {
				err = errors.New("-o needs a file name")
			}
		case "-m", "--mode":
			i++
			var m string
			if m, err = need(i, a); err == nil {
				o.view.mode = -1
				for k, name := range modes {
					if name == m {
						o.view.mode = k
					}
				}
				if o.view.mode < 0 {
					err = fmt.Errorf("unknown mode '%s' (both, wire, mesh, xray, shade)", m)
				}
			}
		case "-c", "--chars":
			i++
			var c string
			if c, err = need(i, a); err == nil {
				o.view.style = -1
				for k, name := range styles {
					if name == c {
						o.view.style = k
					}
				}
				if o.view.style < 0 {
					err = fmt.Errorf("unknown style '%s' (ascii, braille, blocks)", c)
				}
			}
		case "--crease":
			i++
			o.crease, err = needNum(i, a)
		case "--max-triangles":
			i++
			var n float64
			if n, err = needNum(i, a); err == nil {
				if !(n >= 1 && n <= hardMaxTriangles) || n != math.Trunc(n) {
					err = fmt.Errorf("--max-triangles must be a whole number between 1 and %d", hardMaxTriangles)
				} else {
					o.maxTriangles = int(n)
				}
			}
		case "--aspect":
			i++
			if o.aspect, err = needNum(i, a); err == nil && !(o.aspect >= 0.2 && o.aspect <= 10) {
				err = errors.New("--aspect must be between 0.2 and 10")
			}
		default:
			err = fmt.Errorf("unknown option %s (try --help)", a)
		}
		if err != nil {
			return nil, err
		}
	}

	// Which positional, if any, is the file? A leading number is a file only if
	// such a file exists; otherwise, with data on stdin, it is the first angle.
	firstIsNum := false
	if len(pos) > 0 {
		_, firstIsNum = parseNum(pos[0])
	}
	switch {
	case len(pos) == 0 || (firstIsNum && !exists(pos[0])):
		if !stdinPiped {
			if len(pos) == 0 {
				return nil, errUsage
			}
			return nil, fmt.Errorf("%s: no such file", pos[0])
		}
		o.useStdin = true
	case pos[0] == "-":
		if !stdinPiped {
			return nil, errors.New("stdin is a terminal: pipe a model in, or name a file")
		}
		o.useStdin, pos = true, pos[1:]
	default:
		if pos[0] == "" {
			return nil, errors.New("the file name is empty")
		}
		o.file, pos = pos[0], pos[1:]
	}

	nums := make([]float64, len(pos))
	for i, p := range pos {
		d, ok := parseNum(p)
		if !ok {
			return nil, fmt.Errorf("expected a number, got '%s' (try --help)", p)
		}
		nums[i] = d
	}
	switch len(nums) {
	case 0, 3, 4, 6:
	default:
		return nil, errors.New("expected RX RY RZ [ZOOM [PANX PANY]]")
	}
	if len(nums) >= 3 {
		o.viewGiven = true
		o.view.rotX, o.view.rotY, o.view.rotZ = nums[0], nums[1], nums[2]
	}
	if len(nums) >= 4 {
		o.view.zoom = nums[3]
	}
	if len(nums) >= 6 {
		o.view.panX, o.view.panY = nums[4], nums[5]
	}
	for _, a := range nums[:min(len(nums), 3)] {
		if math.Abs(a) > 1e9 {
			return nil, errors.New("angles must be between -1000000000 and 1000000000 degrees")
		}
	}
	if !(o.view.zoom >= 1e-6 && o.view.zoom <= 1e6) {
		return nil, errors.New("zoom must be between 0.000001 and 1000000")
	}
	if math.Abs(o.view.panX) > 1e6 || math.Abs(o.view.panY) > 1e6 {
		return nil, errors.New("pan must be between -1000000 and 1000000")
	}
	if !(o.crease >= 0 && o.crease <= 180) {
		return nil, errors.New("--crease must be between 0 and 180")
	}
	return o, nil
}

// parseSize reads COLSxROWS, or just COLS (or xROWS): the missing side is sized
// to fit the model. crop reports that blank rows should be trimmed.
func parseSize(size string, aspect float64) (cols, rows int, crop bool, err error) {
	p := strings.Split(strings.ToLower(size), "x")
	bad := len(p) > 2
	side := func(k int) int {
		if k >= len(p) || strings.TrimSpace(p[k]) == "" {
			return 0
		}
		n, err := strconv.Atoi(strings.TrimSpace(p[k]))
		if err != nil || n < 0 || n > 1<<20 {
			bad = true
		}
		return n
	}
	cols, rows = side(0), side(1)
	if bad || (cols == 0 && rows == 0) {
		return 0, 0, false, errors.New("--size wants COLSxROWS or just COLS, e.g. 100x40 or 100")
	}
	// a visually square frame is exactly what zoom 1.0 fills
	if rows == 0 {
		rows, crop = int(math.Ceil(float64(cols)/aspect)), true
	}
	if cols == 0 {
		cols = int(math.Ceil(float64(rows) * aspect))
	}
	return cols, rows, crop, nil
}

func run(s *system, argv []string) error {
	o, err := parseArgs(argv, s.fileExists, !s.stdinIsTerm())
	if err != nil {
		return err
	}
	if o.help {
		fmt.Fprint(s.stdout, usage)
		return nil
	}
	if o.version {
		fmt.Fprintln(s.stdout, "rndr", version)
		return nil
	}
	maxTriangles = o.maxTriangles

	tty := s.stdoutIsTerm()
	interactive := o.forceInteractive || (!o.viewGiven && !o.forcePrint && o.out == "" && tty)
	if o.useStdin && !keysFromTTY {
		// here the keyboard is stdin, and stdin is busy carrying the model
		if o.forceInteractive {
			return errors.New("on this system the model cannot come from stdin in interactive mode")
		}
		interactive = false
	}

	var soup []float32
	name := "stdin"
	if o.useStdin {
		soup, err = loadModel(s.stdin, "", -1)
	} else {
		name = filepath.Base(o.file)
		rc, size, oerr := s.openFile(o.file)
		if oerr != nil {
			return fmt.Errorf("%s: %v", o.file, oerr)
		}
		soup, err = loadModel(rc, name, size)
		rc.Close()
	}
	if err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	runtime.GC() // the file's bytes are no longer needed; give them back before the mesh is built
	mesh, err := buildMesh(soup, o.crease)
	if err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	soup = nil
	r := &Renderer{m: mesh, aspect: o.aspect, invert: o.invert}
	if ct := s.getenv("COLORTERM"); ct == "truecolor" || ct == "24bit" {
		r.truecolor = true // the terminal says it can show every grey, not just 26 of them
	}

	if interactive {
		t, err := s.openTerm()
		if err != nil {
			return fmt.Errorf("interactive mode needs a terminal (%v); use -p to print a frame instead", err)
		}
		return runInteractive(s, t, r, &o.view, name)
	}

	var cols, rows int
	crop := false
	if o.sizeGiven {
		if cols, rows, crop, err = parseSize(o.size, o.aspect); err != nil {
			return err
		}
	} else {
		cols, rows = defaultSize(s, tty && o.out == "")
	}
	if cols < 2 || rows < 2 || cols > maxCols || rows > maxRows {
		return fmt.Errorf("frame size must be between 2x2 and %dx%d", maxCols, maxRows)
	}
	frame, err := r.FrameText(&o.view, cols, rows)
	if err != nil {
		return err
	}
	if o.view.style == styleBlocks && o.out == "" && tty {
		defer s.colourOut()()
	}
	if crop {
		frame = strings.Trim(frame, "\n") + "\n"
	}
	if o.out != "" {
		return s.writeFile(o.out, []byte(frame))
	}
	_, err = s.stdout.Write([]byte(frame))
	return err
}

func envInt(s *system, name string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s.getenv(name)))
	return n
}

// defaultSize picks the print-mode frame: fzf preview window, then the terminal,
// then COLUMNS/LINES, then 80x24. Whatever those say is clamped to the largest
// frame rndr will draw.
func defaultSize(s *system, leaveRoomForPrompt bool) (cols, rows int) {
	clamp := func(c, r int) (int, int) { return max(2, min(c, maxCols)), max(2, min(r, maxRows)) }
	if c, r := envInt(s, "FZF_PREVIEW_COLUMNS"), envInt(s, "FZF_PREVIEW_LINES"); c > 0 && r > 0 {
		return clamp(c, r)
	}
	if c, r, ok := s.termSize(); ok {
		if leaveRoomForPrompt {
			r--
		}
		return clamp(c, r)
	}
	if c, r := envInt(s, "COLUMNS"), envInt(s, "LINES"); c > 0 && r > 0 {
		return clamp(c, r)
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
