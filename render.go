package main

import (
	"bytes"
	"errors"
	"math"
	"strings"
)

// Each character cell is sampled subX x subY times; with the usual 1:2 cell
// shape that makes the samples square.
const subX, subY = 2, 4

var modes = []string{"both", "wire", "mesh", "xray", "shade"}

const (
	modeBoth = iota
	modeWire
	modeMesh
	modeXray
	modeShade
)

const ramp = ".,:;=+*#%@"

// What a frame is drawn with. ascii works everywhere. braille gives every
// character cell 2x4 dots and is still plain text. blocks gives every cell two
// grey pixels, using colour codes, so it is for screens rather than text files.
var styles = []string{"ascii", "braille", "blocks"}

const (
	styleASCII = iota
	styleBraille
	styleBlocks
)

// View is everything that decides what a frame looks like.
type View struct {
	rotX, rotY, rotZ float64 // degrees: tilt, spin, roll
	zoom, panX, panY float64
	mode             int
	style            int
}

// Renderer holds the mesh plus buffers reused between frames.
type Renderer struct {
	m         *Mesh
	aspect    float64 // character cell height / width
	invert    bool
	truecolor bool // blocks: 256 grey levels instead of the 26 every terminal has

	X, Y, Z          []float32 // projected vertices, in sample units
	faceNz, faceLum  []float32
	zbuf             []float32
	idbuf            []int32
	edgeZ            []float32
	edgeCh           []byte
	edgeHits         []int32 // samples an edge left in each cell
	mask             []uint8 // per sample: an edge is visible here (braille and blocks only)
	useMask          bool
	bufW, bufH, cols int
	work             int64 // samples visited this frame, against maxRenderWork
}

var errTooExpensive = errors.New("model is too expensive to draw at this size (overdraw limit); try a smaller frame")

// clampInt converts to int without ever leaving [lo, hi], whatever f is (huge, NaN, ...).
func clampInt(f float64, lo, hi int) int {
	if !(f > float64(lo)) {
		return lo
	}
	if f > float64(hi) {
		return hi
	}
	return int(f)
}

var negInf = float32(math.Inf(-1))

func rad(deg float64) float64 { return deg * math.Pi / 180 }
func deg(rad float64) float64 { return rad * 180 / math.Pi }

func mul3(a, b [9]float64) (m [9]float64) {
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			m[3*i+j] = a[3*i]*b[j] + a[3*i+1]*b[3+j] + a[3*i+2]*b[6+j]
		}
	}
	return
}

// euler returns Rx * Ry * Rz.
func euler(rx, ry, rz float64) [9]float64 {
	cx, sx := math.Cos(rad(rx)), math.Sin(rad(rx))
	cy, sy := math.Cos(rad(ry)), math.Sin(rad(ry))
	cz, sz := math.Cos(rad(rz)), math.Sin(rad(rz))
	ax := [9]float64{1, 0, 0, 0, cx, -sx, 0, sx, cx}
	ay := [9]float64{cy, 0, sy, 0, 1, 0, -sy, 0, cy}
	az := [9]float64{cz, -sz, 0, sz, cz, 0, 0, 0, 1}
	return mul3(ax, mul3(ay, az))
}

// Render draws the view into a cols x rows grid of ASCII characters (row-major).
func (r *Renderer) Render(v *View, cols, rows int) ([]byte, error) {
	if err := r.raster(v, cols, rows, false); err != nil {
		return nil, err
	}
	return r.asciiGrid(v, cols, rows), nil
}

// raster projects the model and fills the sample buffers: which face is in
// front at each sample, and where the visible edges run.
func (r *Renderer) raster(v *View, cols, rows int, perSampleEdges bool) error {
	m := r.m
	W, H := cols*subX, rows*subY
	ratio := r.aspect * subX / subY // sample height / sample width
	base := 0.94 * math.Min(float64(W)/2, float64(H)/2*ratio)
	s := base * v.zoom
	ox := float64(W)/2 + v.panX*base
	oy := float64(H)/2 - v.panY*base/ratio

	// view = R * (x, z, -y): model Z is up, model -Y faces the viewer
	R := euler(v.rotX, v.rotY, v.rotZ)
	m00, m01, m02 := R[0], -R[2], R[1]
	m10, m11, m12 := R[3], -R[5], R[4]
	m20, m21, m22 := R[6], -R[8], R[7]

	if len(r.X) < m.nv {
		r.X, r.Y, r.Z = make([]float32, m.nv), make([]float32, m.nv), make([]float32, m.nv)
	}
	sy := s / ratio
	for i := 0; i < m.nv; i++ {
		x, y, z := float64(m.vx[i]), float64(m.vy[i]), float64(m.vz[i])
		r.X[i] = float32(ox + (m00*x+m01*y+m02*z)*s)
		r.Y[i] = float32(oy - (m10*x+m11*y+m12*z)*sy)
		r.Z[i] = float32((m20*x + m21*y + m22*z) * s)
	}

	if r.faceNz == nil {
		r.faceNz, r.faceLum = make([]float32, m.nt), make([]float32, m.nt)
	}
	const lx, ly, lz = -0.45, 0.65, 0.61 // light: up and to the left, in front
	for f := 0; f < m.nt; f++ {
		x, y, z := float64(m.fnx[f]), float64(m.fny[f]), float64(m.fnz[f])
		nx := m00*x + m01*y + m02*z
		ny := m10*x + m11*y + m12*z
		nz := m20*x + m21*y + m22*z
		d := nx*lx + ny*ly + nz*lz
		if nz < 0 {
			d = -d // two-sided: light whichever side we see
		}
		r.faceNz[f] = float32(nz)
		r.faceLum[f] = float32(0.12 + 0.68*math.Max(0, d) + 0.20*math.Abs(nz))
	}

	if len(r.zbuf) < W*H {
		r.zbuf, r.idbuf = make([]float32, W*H), make([]int32, W*H)
	}
	if len(r.edgeZ) < cols*rows {
		r.edgeZ, r.edgeCh, r.edgeHits = make([]float32, cols*rows), make([]byte, cols*rows), make([]int32, cols*rows)
	}
	for i := 0; i < W*H; i++ {
		r.zbuf[i] = negInf
		r.idbuf[i] = -1
	}
	for i := 0; i < cols*rows; i++ {
		r.edgeZ[i] = negInf
		r.edgeCh[i] = 0
		r.edgeHits[i] = 0
	}
	r.bufW, r.bufH, r.cols = W, H, cols
	if r.useMask = perSampleEdges; perSampleEdges {
		if len(r.mask) < W*H {
			r.mask = make([]uint8, W*H)
		}
		clear(r.mask[:W*H])
	}

	r.work = 0
	if v.mode != modeXray {
		if err := r.rasterFaces(); err != nil {
			return err
		}
	}

	if v.mode != modeShade {
		all := v.mode == modeMesh || v.mode == modeXray
		depth := v.mode != modeXray
		slash := deg(math.Atan(r.aspect))
		for e := 0; e < m.ne; e++ {
			if !all && m.eflag[e]&edgeFeature == 0 {
				// not a feature edge: still draw it where it forms the silhouette
				a, b := r.faceNz[m.ef1[e]], r.faceNz[m.ef2[e]]
				if m.eflag[e]&edgeMismatch != 0 {
					b = -b
				}
				if !(a*b < 0) {
					continue
				}
			}
			r.drawEdge(e, depth, ratio, slash)
			if r.work > maxRenderWork {
				return errTooExpensive
			}
		}
	}
	return nil
}

// asciiGrid turns the sample buffers into one ASCII character per cell.
func (r *Renderer) asciiGrid(v *View, cols, rows int) []byte {
	W := r.bufW
	out := make([]byte, cols*rows)
	shade := v.mode == modeBoth || v.mode == modeShade
	levels := len(ramp)
	for row := 0; row < rows; row++ {
		for c := 0; c < cols; c++ {
			cell := row*cols + c
			ch := byte(' ')
			if r.edgeHits[cell] >= 2 {
				// (a single stray sample means the line only clipped this cell's corner)
				ch = r.edgeCh[cell]
			} else if shade {
				n := 0
				sum := float32(0)
				for j := 0; j < subY; j++ {
					at := (row*subY+j)*W + c*subX
					for k := 0; k < subX; k++ {
						if id := r.idbuf[at+k]; id >= 0 {
							n++
							sum += r.faceLum[id]
						}
					}
				}
				if n > 0 {
					val := float64(sum) / float64(n) * (0.5 + 0.5*float64(n)/(subX*subY))
					li := max(0, min(levels-1, int(val*float64(levels))))
					if r.invert {
						li = levels - 1 - li
					}
					ch = ramp[li]
				}
			}
			out[cell] = ch
		}
	}
	return out
}

func (r *Renderer) rasterFaces() error {
	m := r.m
	W, H := r.bufW, r.bufH
	for f := 0; f < m.nt; f++ {
		ia, ib, ic := m.tri[3*f], m.tri[3*f+1], m.tri[3*f+2]
		ax, ay := float64(r.X[ia]), float64(r.Y[ia])
		bx, by := float64(r.X[ib]), float64(r.Y[ib])
		cx, cy := float64(r.X[ic]), float64(r.Y[ic])
		x0 := clampInt(math.Ceil(math.Min(ax, math.Min(bx, cx))-0.5), 0, W)
		x1 := clampInt(math.Floor(math.Max(ax, math.Max(bx, cx))-0.5), -1, W-1)
		if x0 > x1 {
			continue
		}
		y0 := clampInt(math.Ceil(math.Min(ay, math.Min(by, cy))-0.5), 0, H)
		y1 := clampInt(math.Floor(math.Max(ay, math.Max(by, cy))-0.5), -1, H-1)
		if y0 > y1 {
			continue
		}
		if r.work += int64(x1-x0+1) * int64(y1-y0+1); r.work > maxRenderWork {
			return errTooExpensive
		}
		area := (bx-ax)*(cy-ay) - (by-ay)*(cx-ax)
		if area == 0 {
			continue
		}
		inv := 1 / area
		az, bz, cz := float64(r.Z[ia]), float64(r.Z[ib]), float64(r.Z[ic])
		for py := y0; py <= y1; py++ {
			sy := float64(py) + 0.5
			row := py * W
			for px := x0; px <= x1; px++ {
				sx := float64(px) + 0.5
				w0 := ((bx-sx)*(cy-sy) - (by-sy)*(cx-sx)) * inv
				w1 := ((cx-sx)*(ay-sy) - (cy-sy)*(ax-sx)) * inv
				w2 := 1 - w0 - w1
				if w0 < -1e-6 || w1 < -1e-6 || w2 < -1e-6 {
					continue
				}
				z := float32(w0*az + w1*bz + w2*cz)
				if z > r.zbuf[row+px] {
					r.zbuf[row+px] = z
					r.idbuf[row+px] = int32(f)
				}
			}
		}
	}
	return nil
}

func (r *Renderer) drawEdge(e int, depth bool, ratio, slash float64) {
	a, b := r.m.ea[e], r.m.eb[e]
	x0, y0, z0 := float64(r.X[a]), float64(r.Y[a]), float64(r.Z[a])
	dx, dy, dz := float64(r.X[b])-x0, float64(r.Y[b])-y0, float64(r.Z[b])-z0

	// clip the segment to the frame (Liang-Barsky)
	t0, t1 := 0.0, 1.0
	p := [4]float64{-dx, dx, -dy, dy}
	q := [4]float64{x0, float64(r.bufW) - 1e-3 - x0, y0, float64(r.bufH) - 1e-3 - y0}
	for i := 0; i < 4; i++ {
		if p[i] == 0 {
			if q[i] < 0 {
				return
			}
			continue
		}
		t := q[i] / p[i]
		if p[i] < 0 {
			if t > t1 {
				return
			}
			t0 = math.Max(t0, t)
		} else {
			if t < t0 {
				return
			}
			t1 = math.Min(t1, t)
		}
	}

	// pick the glyph closest to the on-screen direction of the line
	ang := deg(math.Atan2(math.Abs(dy*ratio), math.Abs(dx)))
	var ch byte
	switch {
	case ang < slash/2:
		ch = '-'
	case ang > (slash+90)/2:
		ch = '|'
	case dx*dy > 0:
		ch = '\\'
	default:
		ch = '/'
	}

	// two samples per sample-width, so a cell the line really crosses collects at least two hits
	// (after clipping the visible part is no longer than the frame, so this is bounded)
	steps := clampInt(math.Ceil(math.Max(math.Abs(dx), math.Abs(dy))*(t1-t0)*2), 0, 2*(r.bufW+r.bufH)) + 1
	r.work += int64(steps)
	for i := 0; i < steps; i++ {
		t := t0 + (t1-t0)*float64(i)/float64(steps)
		px, py := clampInt(x0+dx*t, -1, r.bufW), clampInt(y0+dy*t, -1, r.bufH)
		if px < 0 || py < 0 || px >= r.bufW || py >= r.bufH {
			continue
		}
		z := float32(z0 + dz*t)
		if depth && !r.visible(px, py, z) {
			continue
		}
		if r.useMask {
			r.mask[py*r.bufW+px] = 1
		}
		cell := (py/subY)*r.cols + px/subX
		r.edgeHits[cell]++
		if z > r.edgeZ[cell] {
			r.edgeZ[cell] = z
			r.edgeCh[cell] = ch
		}
	}
}

// visible reports whether an edge sample can be seen. It is hidden only if every
// sample around it holds a nearer surface; looking at the 3x3 neighbourhood keeps
// edges on steep faces and silhouettes from being eaten by their own surface.
func (r *Renderer) visible(px, py int, z float32) bool {
	lim := z + 0.5
	xa, xb := max(0, px-1), min(r.bufW-1, px+1)
	ya, yb := max(0, py-1), min(r.bufH-1, py+1)
	for y := ya; y <= yb; y++ {
		row := y * r.bufW
		for x := xa; x <= xb; x++ {
			if r.zbuf[row+x] <= lim {
				return true
			}
		}
	}
	return false
}

// Lines draws the view as rows of terminal text in the view's style: no line
// endings, trailing blanks removed, never wider than cols cells.
func (r *Renderer) Lines(v *View, cols, rows int) ([][]byte, error) {
	if err := r.raster(v, cols, rows, v.style != styleASCII); err != nil {
		return nil, err
	}
	switch v.style {
	case styleBraille:
		return r.brailleLines(v, cols, rows), nil
	case styleBlocks:
		return r.blockLines(v, cols, rows), nil
	}
	g := r.asciiGrid(v, cols, rows)
	lines := make([][]byte, rows)
	for row := range lines {
		lines[row] = bytes.TrimRight(g[row*cols:(row+1)*cols], " ")
	}
	return lines, nil
}

// FrameText is the frame as text: one line per row, trailing blanks removed.
func (r *Renderer) FrameText(v *View, cols, rows int) (string, error) {
	lines, err := r.Lines(v, cols, rows)
	if err != nil {
		return "", err
	}
	size := len(lines)
	for _, line := range lines {
		size += len(line)
	}
	var sb strings.Builder
	sb.Grow(size)
	for _, line := range lines {
		sb.Write(line)
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}
