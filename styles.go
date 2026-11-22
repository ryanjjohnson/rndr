package main

import (
	"math"
	"strconv"
	"unicode/utf8"
)

// Braille and half-block output. Both are drawn from the same sample buffers
// as ASCII, just shown at a finer grain. Everything written here comes from
// fixed tables and bounded integers; nothing from the model reaches the
// terminal except which dots and which grey levels are on.

// A braille cell is 2x4 dots, which is exactly one dot per sample.
var brailleBit = [subY][subX]rune{{0x01, 0x08}, {0x02, 0x10}, {0x04, 0x20}, {0x40, 0x80}}

// Ordered dither: a sample is lit if its face is brighter than the pattern is there.
var bayer = [4][4]float32{{0, 8, 2, 10}, {12, 4, 14, 6}, {3, 11, 1, 9}, {15, 7, 13, 5}}

func (r *Renderer) brailleLines(v *View, cols, rows int) [][]byte {
	W := r.bufW
	shade := v.mode == modeBoth || v.mode == modeShade
	halo := v.mode == modeBoth // keep shading dots off the edges, or the lines drown in them
	lines := make([][]byte, rows)
	for row := range lines {
		line := make([]byte, 0, cols*utf8.UTFMax)
		blanks := 0
		for c := 0; c < cols; c++ {
			var dots rune
			for j := 0; j < subY; j++ {
				y := row*subY + j
				for k := 0; k < subX; k++ {
					x := c*subX + k
					at := y*W + x
					switch {
					case r.mask[at] != 0:
						dots |= brailleBit[j][k]
					case shade && r.idbuf[at] >= 0 && !(halo && r.nearEdge(x, y)):
						lum := r.faceLum[r.idbuf[at]]
						if halo {
							lum *= 0.7
						}
						if r.invert {
							lum = 1 - lum
						}
						if lum*16 > bayer[y&3][x&3]+0.5 {
							dots |= brailleBit[j][k]
						}
					}
				}
			}
			if dots == 0 {
				blanks++
				continue
			}
			for ; blanks > 0; blanks-- {
				line = append(line, ' ')
			}
			line = utf8.AppendRune(line, 0x2800+dots)
		}
		lines[row] = line
	}
	return lines
}

// nearEdge reports whether a sample touches an edge sample.
func (r *Renderer) nearEdge(px, py int) bool {
	xa, xb := max(0, px-1), min(r.bufW-1, px+1)
	ya, yb := max(0, py-1), min(r.bufH-1, py+1)
	for y := ya; y <= yb; y++ {
		row := y * r.bufW
		for x := xa; x <= xb; x++ {
			if r.mask[row+x] != 0 {
				return true
			}
		}
	}
	return false
}

// A half-block pixel is one of these, or a colour (zero or more).
const (
	pxEmpty = -1 // the terminal's own background
	pxInk   = -2 // the terminal's own text colour
)

func (r *Renderer) blockLines(v *View, cols, rows int) [][]byte {
	shade := v.mode == modeBoth || v.mode == modeShade
	lines := make([][]byte, rows)
	for row := range lines {
		w := blockWriter{line: make([]byte, 0, cols*4), fg: pxInk, bg: pxEmpty, truecolor: r.truecolor}
		for c := 0; c < cols; c++ {
			w.cell(r.pixel(c*subX, row*subY, shade), r.pixel(c*subX, row*subY+subY/2, shade))
		}
		lines[row] = w.finish()
	}
	return lines
}

// pixel is the half-cell whose top-left sample is (x0, y0): 2x2 samples.
// Without shading, edges are drawn in the terminal's own text colour, so line
// drawings need no colour codes at all. With shading, faces are greys and an
// edge darkens the face it crosses.
func (r *Renderer) pixel(x0, y0 int, shade bool) int {
	edge, n := false, 0
	sum := float32(0)
	for y := y0; y < y0+subY/2; y++ {
		at := y*r.bufW + x0
		for k := 0; k < subX; k++ {
			if r.mask[at+k] != 0 {
				edge = true
			}
			if id := r.idbuf[at+k]; id >= 0 {
				n++
				sum += r.faceLum[id]
			}
		}
	}
	switch {
	case !shade && edge:
		return pxInk
	case shade && n >= 2:
		// a little above linear: terminal greys are darker to the eye than their numbers
		lum := math.Pow(float64(sum)/float64(n), 0.8)
		if edge {
			lum *= 0.5
		}
		return r.grey(lum)
	}
	return pxEmpty
}

// grey turns a brightness in [0, 1] into a colour: a grey level 0..255 with
// truecolor, otherwise the nearest of the 26 greys in the 256-colour palette.
// (--invert does not apply: a grey is the same grey on a dark or a light screen.)
func (r *Renderer) grey(lum float64) int {
	g := clampInt(math.Round(lum*255), 0, 255)
	if r.truecolor {
		return g
	}
	switch {
	case g < 4:
		return 16 // black
	case g > 246:
		return 231 // white
	}
	return 232 + clampInt(math.Round(float64(g-8)/10), 0, 23) // the grey ramp: 8, 18, ... 238
}

// blockWriter builds one line of half-block cells, changing colour only when
// it has to and leaving trailing blank cells out.
type blockWriter struct {
	line      []byte
	blanks    int
	fg, bg    int
	truecolor bool
}

func (w *blockWriter) colour(c int, fg bool) {
	cur := &w.bg
	code, def := "\x1b[48;", "\x1b[49m"
	if fg {
		cur, code, def = &w.fg, "\x1b[38;", "\x1b[39m"
	}
	if *cur == c {
		return
	}
	*cur = c
	switch {
	case c < 0:
		w.line = append(w.line, def...)
	case w.truecolor:
		g := strconv.Itoa(c)
		w.line = append(w.line, code+"2;"+g+";"+g+";"+g+"m"...)
	default:
		w.line = append(w.line, code+"5;"+strconv.Itoa(c)+"m"...)
	}
}

func (w *blockWriter) cell(top, bottom int) {
	if top == pxEmpty && bottom == pxEmpty {
		w.blanks++
		return
	}
	if w.blanks > 0 {
		w.colour(pxEmpty, false)
		for ; w.blanks > 0; w.blanks-- {
			w.line = append(w.line, ' ')
		}
	}
	switch {
	case top == bottom && top == pxInk:
		w.colour(pxEmpty, false)
		w.colour(pxInk, true)
		w.line = append(w.line, "█"...)
	case top == bottom: // a background fills its cell with no seams, whatever the font
		w.colour(top, false)
		w.line = append(w.line, ' ')
	case bottom == pxEmpty:
		w.colour(pxEmpty, false)
		w.colour(top, true)
		w.line = append(w.line, "▀"...)
	case top == pxEmpty:
		w.colour(pxEmpty, false)
		w.colour(bottom, true)
		w.line = append(w.line, "▄"...)
	case w.fg == bottom || w.bg == top: // the other way up costs fewer colour changes
		w.colour(bottom, true)
		w.colour(top, false)
		w.line = append(w.line, "▄"...)
	default:
		w.colour(top, true)
		w.colour(bottom, false)
		w.line = append(w.line, "▀"...)
	}
}

func (w *blockWriter) finish() []byte {
	if w.fg != pxInk || w.bg != pxEmpty {
		w.line = append(w.line, "\x1b[0m"...)
	}
	return w.line
}
