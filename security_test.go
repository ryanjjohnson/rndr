package main

// Tests that treat input files as hostile. Each one is an attack that either
// worked against an earlier version or is a classic against this kind of program.
// The fuzz targets at the bottom run the whole load-and-render path on random
// mutations: `go test -fuzz FuzzSTL` (and FuzzOBJ, FuzzModelXML, Fuzz3MFZip, FuzzSniff).

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// limits lowers the global limits for one test and restores them afterwards.
func limits(t testing.TB, tris, instances int, work int64) {
	t.Helper()
	a, b, c := maxTriangles, maxInstances, maxRenderWork
	maxTriangles, maxInstances, maxRenderWork = tris, instances, work
	t.Cleanup(func() { maxTriangles, maxInstances, maxRenderWork = a, b, c })
}

// within fails the test if f takes longer than d: the attacks here are about
// making the program hang or balloon, so "finishes promptly" is the assertion.
func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("still running after %v", d)
	}
}

const allowedOutput = " \n.,:;=+*#%@-|/\\"

// colourCode reads one colour-changing escape sequence from the start of s and
// returns its length, or 0 if s does not start with one that rndr could have
// written: a grey from the 256-colour palette, an r=g=b truecolor grey, or
// "back to the default", each in exactly the spelling rndr uses.
func colourCode(s string) (n int, fg, set bool) {
	for _, p := range []struct {
		lead    string
		fg, set bool
	}{{"\x1b[38;", true, true}, {"\x1b[48;", false, true}} {
		rest, ok := strings.CutPrefix(s, p.lead)
		if !ok {
			continue
		}
		end := strings.IndexByte(rest, 'm')
		if end < 0 || end > 13 {
			return 0, false, false
		}
		f := strings.Split(rest[:end], ";")
		num := func(a string) int { // canonical decimal 0..255, or -1
			v, err := strconv.Atoi(a)
			if err != nil || v < 0 || v > 255 || strconv.Itoa(v) != a {
				return -1
			}
			return v
		}
		switch {
		case len(f) == 2 && f[0] == "5":
			if v := num(f[1]); v == 16 || v >= 231 {
				return len(p.lead) + end + 1, p.fg, true
			}
		case len(f) == 4 && f[0] == "2":
			if v := num(f[1]); v >= 0 && f[2] == f[1] && f[3] == f[1] {
				return len(p.lead) + end + 1, p.fg, true
			}
		}
		return 0, false, false
	}
	if strings.HasPrefix(s, "\x1b[39m") {
		return 5, true, false
	}
	if strings.HasPrefix(s, "\x1b[49m") {
		return 5, false, false
	}
	return 0, false, false
}

// frameProblem checks a printed frame against everything its style is allowed
// to contain, and returns the offset of the first byte that is not, or -1.
//
//	ascii:   the shading and line characters
//	braille: spaces and braille patterns (never the blank one)
//	blocks:  spaces, half and full blocks and grey colour codes, and every
//	         line must end with the colours back at the terminal's own
func frameProblem(text string, style int) int {
	switch style {
	case styleASCII:
		return onlyFrom(text, allowedOutput)
	case styleBraille:
		for i, c := range text {
			if c != ' ' && c != '\n' && !(c > 0x2800 && c <= 0x28ff) {
				return i
			}
		}
		return -1
	}
	fgSet, bgSet := false, false
	for i := 0; i < len(text); {
		c, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case c == '\n':
			if fgSet || bgSet {
				return i // colours left on would leak into whatever is printed next
			}
		case c == ' ' || c == '▀' || c == '▄' || c == '█':
		case strings.HasPrefix(text[i:], "\x1b[0m"):
			fgSet, bgSet, size = false, false, 4
		case c == 0x1b:
			n, fg, set := colourCode(text[i:])
			if n == 0 {
				return i
			}
			if fg {
				fgSet = set
			} else {
				bgSet = set
			}
			size = n
		default:
			return i
		}
		i += size
	}
	if fgSet || bgSet {
		return len(text)
	}
	return -1
}

// anyStyleProblem is frameProblem for a frame whose style is not known.
func anyStyleProblem(text string) int {
	worst := -1
	for style := range styles {
		i := frameProblem(text, style)
		if i < 0 {
			return -1
		}
		worst = max(worst, i)
	}
	return worst
}

// cellsWide is how many character cells the widest line of a frame takes.
func cellsWide(text string) int {
	widest := 0
	for _, line := range strings.Split(text, "\n") {
		n := 0
		for i := 0; i < len(line); {
			if k, _, _ := colourCode(line[i:]); k > 0 {
				i += k
			} else if strings.HasPrefix(line[i:], "\x1b[0m") {
				i += 4
			} else {
				_, size := utf8.DecodeRuneInString(line[i:])
				i += size
				n++
			}
		}
		widest = max(widest, n)
	}
	return widest
}

// pipeline loads, builds and draws a file in every mode and style, checking that
// whatever comes out is made only of what that style is supposed to print.
func pipeline(t testing.TB, name string, data []byte) error {
	soup, err := loadTriangles(name, data)
	if err != nil {
		return err
	}
	m, err := buildMesh(soup, 30)
	if err != nil {
		return err
	}
	r := &Renderer{m: m, aspect: 2}
	for mode := range modes {
		for style := range styles {
			for _, v := range []View{
				{rotX: 25, rotY: -30, zoom: 1, mode: mode, style: style},
				{rotX: -100, rotY: 170, rotZ: 45, zoom: 40, panX: 3, panY: -2, mode: mode, style: style},
			} {
				r.truecolor = mode%2 == 1
				text, err := r.FrameText(&v, 40, 12)
				if err != nil {
					return err
				}
				if i := frameProblem(text, style); i >= 0 {
					t.Fatalf("%s %s output is wrong at byte %d: %q", modes[mode], styles[style], i, text[max(0, i-8):min(len(text), i+8)])
				}
				if w := cellsWide(text); w > 40 || strings.Count(text, "\n") != 12 {
					t.Fatalf("%s %s frame is %d cells wide, %d lines", modes[mode], styles[style], w, strings.Count(text, "\n"))
				}
			}
		}
	}
	return nil
}

func wrap3MF(model []byte, extra ...string) []byte {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, _ := w.Create("3D/3dmodel.model")
	f.Write(model)
	for i := 0; i+1 < len(extra); i += 2 {
		f, _ := w.Create(extra[i])
		io.WriteString(f, extra[i+1])
	}
	w.Close()
	return b.Bytes()
}

const oneTriangle = `<mesh><vertices><vertex x="0" y="0" z="0"/><vertex x="1" y="0" z="0"/><vertex x="0" y="1" z="0"/></vertices><triangles><triangle v1="0" v2="1" v3="2"/></triangles></mesh>`

func binarySTL(tris [][9]float32) []byte {
	d := make([]byte, 84, 84+50*len(tris))
	binary.LittleEndian.PutUint32(d[80:], uint32(len(tris)))
	for _, t := range tris {
		rec := make([]byte, 50)
		for k, f := range t {
			binary.LittleEndian.PutUint32(rec[12+4*k:], math.Float32bits(f))
		}
		d = append(d, rec...)
	}
	return d
}

func wantError(t *testing.T, err error, containing string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), containing) {
		t.Fatalf("want an error containing %q, got %v", containing, err)
	}
}

// ---- 3MF ---------------------------------------------------------------------

// A chain of objects that each include the next one twice: 30 levels is a
// billion copies from a file of a few hundred bytes.
func componentBomb(leaf string) []byte {
	var sb strings.Builder
	sb.WriteString("<model><resources>")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&sb, `<object id="%d"><components><component objectid="%d"/><component objectid="%d"/></components></object>`, i, i+1, i+1)
	}
	fmt.Fprintf(&sb, `<object id="31">%s</object></resources><build><item objectid="1"/></build></model>`, leaf)
	return wrap3MF([]byte(sb.String()))
}

func TestComponentBombIsRefused(t *testing.T) {
	within(t, 20*time.Second, func() {
		wantError(t, pipeline(t, "x.3mf", componentBomb(oneTriangle)), "places more than")
		// the same shape with no geometry at all is pure CPU rather than memory
		wantError(t, pipeline(t, "x.3mf", componentBomb("")), "places more than")
	})
}

func TestSelfReferencingObjectEnds(t *testing.T) {
	model := `<model><resources><object id="1">` + oneTriangle + `<components><component objectid="1"/><component objectid="2"/></components></object>` +
		`<object id="2"><components><component objectid="1"/></components></object></resources><build><item objectid="1"/></build></model>`
	within(t, 10*time.Second, func() {
		if err := pipeline(t, "x.3mf", wrap3MF([]byte(model))); err != nil {
			t.Fatal(err)
		}
	})
}

func TestZipBombIsRefused(t *testing.T) {
	limits(t, 1000, maxInstances, maxRenderWork) // unpacked XML budget drops to its 64 MB floor
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, _ := w.Create("3D/3dmodel.model")
	io.WriteString(f, `<model><resources><object id="1">`)
	pad := bytes.Repeat([]byte(" "), 1<<20)
	for i := 0; i < 80; i++ { // 80 MB of nothing; a few KB once compressed
		f.Write(pad)
	}
	io.WriteString(f, oneTriangle+`</object></resources></model>`)
	w.Close()
	if b.Len() > 1<<20 {
		t.Fatalf("test file should be tiny, is %d bytes", b.Len())
	}
	within(t, 30*time.Second, func() {
		wantError(t, pipeline(t, "x.3mf", b.Bytes()), "too large when unpacked")
	})
}

func TestOversizedTagIsRefused(t *testing.T) {
	model := `<model><resources><object id="` + strings.Repeat("A", 1<<20) + `">` + oneTriangle + `</object></resources></model>`
	wantError(t, pipeline(t, "x.3mf", wrap3MF([]byte(model))), "oversized XML tag")
	// same thing hidden in a tag that is never closed
	wantError(t, pipeline(t, "x.3mf", wrap3MF([]byte(`<model><object id="1" x="`+strings.Repeat("<>", 1<<19)))), "oversized XML tag")
}

func TestDoctypeAndEntitiesAreRefused(t *testing.T) {
	model := `<?xml version="1.0"?><!DOCTYPE m [<!ENTITY x SYSTEM "file:///etc/passwd"><!ENTITY a "aaaa"><!ENTITY b "&a;&a;&a;&a;">]>` +
		`<model><resources><object id="&x;&b;">` + oneTriangle + `</object></resources></model>`
	wantError(t, pipeline(t, "x.3mf", wrap3MF([]byte(model))), "document type declaration")
}

func TestPathsCannotLeaveTheArchive(t *testing.T) {
	// /etc/hosts exists on every Unix; if any of these were opened from disk it
	// would fail to parse as a model and surface as an error rather than "no triangles".
	model := `<model><resources><object id="1"><components>` +
		`<component p:path="../../../../../../etc/hosts" objectid="1"/>` +
		`<component p:path="/etc/hosts" objectid="1"/>` +
		`<component p:path="file:///etc/hosts" objectid="1"/>` +
		`<component p:path="C:\Windows\win.ini" objectid="1"/>` +
		`<component p:path="//server/share/x.model" objectid="1"/>` +
		`</components></object></resources><build><item objectid="1"/></build></model>`
	wantError(t, pipeline(t, "x.3mf", wrap3MF([]byte(model))), "no triangles found")
}

func TestInstanceAndTriangleLimitsIn3MF(t *testing.T) {
	limits(t, 100, 50, maxRenderWork)
	var items strings.Builder
	for i := 0; i < 60; i++ {
		items.WriteString(`<item objectid="1"/>`)
	}
	model := `<model><resources><object id="1">` + oneTriangle + `</object></resources><build>` + items.String() + `</build></model>`
	wantError(t, pipeline(t, "x.3mf", wrap3MF([]byte(model))), "places more than")

	limits(t, 5, 1000, maxRenderWork)
	wantError(t, pipeline(t, "x.3mf", wrap3MF([]byte(model[:strings.Index(model, "<build>")]+`<build>`+strings.Repeat(`<item objectid="1"/>`, 6)+`</build></model>`))), "triangles")
	many := `<model><resources><object id="1"><mesh><triangles>` + strings.Repeat(`<triangle/>`, 6) + `</triangles></mesh></object></resources></model>`
	wantError(t, pipeline(t, "x.3mf", wrap3MF([]byte(many))), "triangles")
	verts := `<model><resources><object id="1"><mesh><vertices>` + strings.Repeat(`<vertex/>`, 16) + `</vertices></mesh></object></resources></model>`
	wantError(t, pipeline(t, "x.3mf", wrap3MF([]byte(verts))), "triangles")
}

func TestTagScanner(t *testing.T) {
	doc := `<?xml version="1.0"?><!-- a <fake> tag --> text <a x="1>2" y='q"r'/> more` +
		`<![CDATA[ <b> ]]> ]]><ns:c p:path = "/A&amp;B.model"  >` + "\n" + `</ns:c><!----><d/><>`
	sc := &tagScanner{r: bufioReader(doc)}
	var got []string
	for {
		tag, err := sc.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name, closing, rest := splitTag(tag)
		s := string(name)
		if closing {
			s = "/" + s
		}
		if err := eachAttr(rest, func(k, v []byte) { s += " " + string(k) + "=" + unescape(v) }); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	want := []string{`a x=1>2 y=q"r`, `c path=/A&B.model`, `/c`, `d`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	for _, bad := range []string{`<a x=1>`, `<a x="1`, `<a x>`, `<!-- never closed`, `<?pi`, `<a`, `<![CDATA[ x`} {
		sc := &tagScanner{r: bufioReader(bad)}
		tag, err := sc.next()
		if err == nil {
			_, _, rest := splitTag(tag)
			err = eachAttr(rest, func(k, v []byte) {})
		}
		if err == nil || err == io.EOF {
			t.Errorf("%q: want an error, got %v", bad, err)
		}
	}
}

// ---- STL and OBJ ---------------------------------------------------------------

func TestLyingSTLHeaders(t *testing.T) {
	huge := make([]byte, 84+50)
	binary.LittleEndian.PutUint32(huge[80:], 0xffffffff) // claims 4 billion triangles
	wantError(t, pipeline(t, "x.stl", huge), "truncated")
	short := make([]byte, 84+120)
	binary.LittleEndian.PutUint32(short[80:], 5)
	wantError(t, pipeline(t, "x.stl", short), "truncated")
	for n := 0; n < 90; n++ { // every length around the header boundary
		pipeline(t, "x.stl", make([]byte, n))
		pipeline(t, "stdin", bytes.Repeat([]byte("solid "), n))
	}
}

func TestTriangleLimit(t *testing.T) {
	limits(t, 10, maxInstances, maxRenderWork)
	tris := make([][9]float32, 11)
	for i := range tris {
		tris[i] = [9]float32{float32(i), 0, 0, float32(i) + 1, 0, 0, float32(i), 1, 0}
	}
	wantError(t, pipeline(t, "x.stl", binarySTL(tris)), "triangles")
	wantError(t, pipeline(t, "x.stl", []byte("solid x\nfacet\n"+strings.Repeat("vertex 1 2 3\n", 33))), "triangles")
	// one polygon with many corners fans out into many triangles
	wantError(t, pipeline(t, "x.obj", []byte("v 0 0 0\nv 1 0 0\nv 0 1 0\nf "+strings.Repeat("1 2 3 ", 5)+"\n")), "triangles")
	if err := pipeline(t, "x.stl", binarySTL(tris[:10])); err != nil {
		t.Fatalf("a model exactly at the limit should load: %v", err)
	}
}

func TestExtremeValues(t *testing.T) {
	inf, nan := float32(math.Inf(1)), float32(math.NaN())
	vals := []float32{math.MaxFloat32, -math.MaxFloat32, math.SmallestNonzeroFloat32, inf, -inf, nan, 0, float32(math.Copysign(0, -1)), 1}
	var tris [][9]float32
	for _, a := range vals {
		for _, b := range vals {
			tris = append(tris, [9]float32{a, b, 0, b, a, 1, a, a, b})
		}
	}
	if err := pipeline(t, "x.stl", binarySTL(tris)); err != nil {
		t.Fatal(err)
	}
	pipeline(t, "x.obj", []byte("v 1e999 0 0\nv -1e999 nan 0\nv 3e38 3e38 3e38\nv -3e38 -3e38 -3e38\nv 0 0 0\nf 1 2 3\nf 3 4 5\n"+
		"f 99999999999999999999 1 2\nf -9223372036854775808 0 5\nf 3/ 4// 5/1/1/1/1\nf\nv\nf 3 3 3\nf -1 -2 -99\n"))
	pipeline(t, "x.3mf", wrap3MF([]byte(`<model><resources><object id="1"><mesh><vertices><vertex x="1e38"/><vertex x="1" y="1e30"/><vertex z="nan"/><vertex y="1"/></vertices>`+
		`<triangles><triangle v1="0" v2="1" v3="3"/><triangle v1="4294967296" v2="-1" v3="99999999999999999999"/><triangle v1="0" v2="3"/></triangles></mesh></object></resources>`+
		`<build><item objectid="1" transform="1e38 1e38 1e38 1e38 1e38 1e38 1e38 1e38 1e38 inf nan -inf"/><item objectid="1" transform="1e300 0 0 0 1 0 0 0 1 0 0 0"/><item objectid="nope"/><item/></build></model>`)))
}

// Every vertex below is distinct, but all of them collide under a fixed,
// unkeyed hash of their bits. Against one, welding 600,000 of them takes minutes.
func TestHashFloodingDoesNotSlowTheWeld(t *testing.T) {
	const p1, p2, p3 = 73856093, 19349663, 83492791
	inv := uint32(1) // modular inverse of p1 mod 2^32, by Newton iteration
	for i := 0; i < 5; i++ {
		inv *= 2 - p1*inv
	}
	var verts [][3]uint32
	for by := uint32(1); len(verts) < 600000; by++ {
		bz := (by * 7) & 0x7fffff
		bx := (0x12345678 ^ by*p2 ^ bz*p3) * inv
		if bx>>23&0xff != 0xff { // skip NaN/Inf bit patterns
			verts = append(verts, [3]uint32{bx, by, bz})
		}
	}
	soup := make([]float32, 0, len(verts)*3)
	for _, v := range verts {
		soup = append(soup, math.Float32frombits(v[0]), math.Float32frombits(v[1]), math.Float32frombits(v[2]))
	}
	within(t, 30*time.Second, func() {
		m, err := buildMesh(soup, 30)
		if err != nil || m.nv != len(verts) {
			t.Errorf("weld: %v", err)
		}
	})
}

func TestOverdrawIsRefused(t *testing.T) {
	limits(t, maxTriangles, maxInstances, 2e6)
	tris := make([][9]float32, 5000) // each one covers the whole frame
	for i := range tris {
		y := float32(i) * 1e-4
		tris[i] = [9]float32{-1, y, -1, 1, y, -1, 0, y, 1}
	}
	soup, _ := loadTriangles("x.stl", binarySTL(tris))
	m, err := buildMesh(soup, 30)
	if err != nil {
		t.Fatal(err)
	}
	r := &Renderer{m: m, aspect: 2}
	for mode := range modes {
		if _, err := r.Render(&View{zoom: 1, mode: mode}, 200, 60); err != errTooExpensive {
			t.Errorf("mode %s: want errTooExpensive, got %v", modes[mode], err)
		}
	}
}

// ---- output ------------------------------------------------------------------

func TestMessagesCannotCarryEscapeSequences(t *testing.T) {
	in := "evil\x1b]0;PWNED\x07\x1b[2J\u009b31m\u202e\x00\r\n\xff.stl"
	out := printable(in)
	for _, c := range out {
		if c < ' ' || c == 0x7f || (c >= 0x80 && c < 0xa0) || c == '\u202e' {
			t.Fatalf("control character %U survived in %q", c, out)
		}
	}
	if !strings.HasPrefix(out, "evil?]0;PWNED?") {
		t.Fatalf("unexpected result %q", out)
	}
}

func TestGoldenRender(t *testing.T) {
	data, err := os.ReadFile("testdata/bracket.stl")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile("testdata/bracket.expected.txt")
	soup, _ := loadTriangles("bracket.stl", data)
	m, err := buildMesh(soup, 30)
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&Renderer{m: m, aspect: 2}).FrameText(&View{rotX: 25, rotY: -30, zoom: 1}, 60, 20)
	if err != nil || got != string(want) {
		t.Fatalf("render differs from testdata/bracket.expected.txt (err %v)", err)
	}
}

// ---- fuzzing -----------------------------------------------------------------

const seedModel = `<model><resources><object id="1"><mesh><vertices><vertex x="0" y="0" z="0"/><vertex x="1" y="0" z="0"/><vertex x="0" y="1" z="0"/><vertex x="0" y="0" z="1"/></vertices><triangles><triangle v1="0" v2="1" v3="2"/><triangle v1="0" v2="1" v3="3"/></triangles></mesh></object><object id="2"><components><component objectid="1" transform="1 0 0 0 1 0 0 0 1 5 0 0"/><component p:path="/3D/3dmodel.model" objectid="1"/></components></object></resources><build><item objectid="2" transform="2 0 0 0 2 0 0 0 2 0 0 0"/><item objectid="1"/></build></model>`

// fuzzLimits keeps each execution fast so the fuzzer can try many inputs; the
// limits themselves are exercised by the tests above.
func fuzzLimits(f *testing.F) { limits(f, 20000, 2000, 5e7) }

func FuzzSTL(f *testing.F) {
	fuzzLimits(f)
	seed, _ := os.ReadFile("testdata/bracket.stl")
	f.Add(seed)
	fuzzSeeds(func(d []byte) { f.Add(d) }, ".stl")
	f.Add(binarySTL([][9]float32{{0, 0, 0, 1, 0, 0, 0, 1, 0}, {0, 0, 0, 1, 0, 0, 0, 0, 1}}))
	f.Fuzz(func(t *testing.T, d []byte) { promptly(t, "pipeline", func() { pipeline(t, "x.stl", d) }) })
}

func FuzzOBJ(f *testing.F) {
	fuzzLimits(f)
	f.Add([]byte("v 0 0 0\nv 1 0 0\nv 0 1 0\nv 0 0 1 1.0\nf 1 2 3\nf 1/1/1 2//2 -1\nf -4 -3 -2 -1\n"))
	fuzzSeeds(func(d []byte) { f.Add(d) }, ".obj")
	f.Fuzz(func(t *testing.T, d []byte) { promptly(t, "pipeline", func() { pipeline(t, "x.obj", d) }) })
}

func FuzzModelXML(f *testing.F) {
	fuzzLimits(f)
	f.Add([]byte(seedModel))
	f.Add([]byte(`<?xml version="1.0"?><!-- c --><model><![CDATA[x]]><resources><object id='1'/></resources></model>`))
	fuzzSeeds(func(d []byte) { f.Add(d) }, ".model")
	f.Fuzz(func(t *testing.T, d []byte) { promptly(t, "pipeline", func() { pipeline(t, "x.3mf", wrap3MF(d)) }) })
}

func Fuzz3MFZip(f *testing.F) {
	fuzzLimits(f)
	f.Add(wrap3MF([]byte(seedModel)))
	f.Add(wrap3MF([]byte(`<model><resources><object id="1"><components><component p:path="/3D/o.model" objectid="1"/></components></object></resources><build><item objectid="1"/></build></model>`),
		"3D/o.model", `<model><resources><object id="1">`+oneTriangle+`</object></resources></model>`,
		"_rels/.rels", `<Relationships><Relationship Target="/3D/3dmodel.model" Type="http://x/3dmodel"/></Relationships>`))
	fuzzSeeds(func(d []byte) { f.Add(d) }, ".3mf")
	f.Fuzz(func(t *testing.T, d []byte) { promptly(t, "pipeline", func() { pipeline(t, "x.3mf", d) }) })
}

func FuzzSniff(f *testing.F) {
	fuzzLimits(f)
	seed, _ := os.ReadFile("testdata/bracket.stl")
	f.Add(seed)
	f.Add(wrap3MF([]byte(seedModel)))
	f.Add([]byte("v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"))
	f.Fuzz(func(t *testing.T, d []byte) { promptly(t, "pipeline", func() { pipeline(t, "", d) }) })
}

func bufioReader(s string) *bufio.Reader { return bufio.NewReaderSize(strings.NewReader(s), 16) }
