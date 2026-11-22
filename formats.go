package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
)

type format int

const (
	fmtUnknown format = iota
	fmtSTL            // named .stl: binary or ASCII is decided from the content
	fmtSTLBinary
	fmtSTLASCII
	fmtOBJ
	fmt3MF
)

func formatFromName(name string) format {
	switch strings.ToLower(path.Ext(strings.ReplaceAll(name, "\\", "/"))) {
	case ".stl":
		return fmtSTL
	case ".obj":
		return fmtOBJ
	case ".3mf":
		return fmt3MF
	}
	return fmtUnknown
}

// sniffBytes is how much of a stream is looked at to decide what it is.
const sniffBytes = 64 << 10

// textLike reports whether b could be the start of a text file: no control
// bytes other than whitespace. Binary STL data fails this almost at once.
func textLike(b []byte) bool {
	for _, c := range b {
		if c < '\t' || (c > '\r' && c < ' ') {
			return false
		}
	}
	return true
}

func looksLikeASCIISTL(head []byte) bool {
	h := bytes.ToLower(head[:min(len(head), 4096)])
	return bytes.HasPrefix(bytes.TrimSpace(h), []byte("solid")) && bytes.Contains(h, []byte("facet"))
}

// hasOBJVertex looks for a line of the form "v x y z".
func hasOBJVertex(head []byte) bool {
	for len(head) > 0 {
		line := head
		if nl := bytes.IndexByte(head, '\n'); nl >= 0 {
			line, head = head[:nl], head[nl+1:]
		} else {
			head = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) < 3 || line[0] != 'v' || (line[1] != ' ' && line[1] != '\t') {
			continue
		}
		f := bytes.Fields(line[2:])
		ok := len(f) >= 3
		for k := 0; ok && k < 3; k++ {
			_, err := strconv.ParseFloat(string(f[k]), 32)
			ok = err == nil || errors.Is(err, strconv.ErrRange)
		}
		if ok {
			return true
		}
	}
	return false
}

// sniff decides what a stream is from its first bytes.
func sniff(head []byte) format {
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return fmt3MF
	case textLike(head):
		if looksLikeASCIISTL(head) {
			return fmtSTLASCII
		}
		if hasOBJVertex(head) {
			return fmtOBJ
		}
		return fmtUnknown
	case len(head) >= 84:
		return fmtSTLBinary
	}
	return fmtUnknown
}

// loadModel reads a model from r and returns triangle soup: 9 floats per triangle.
//
// The format comes from the name's extension when it has one; otherwise (stdin,
// or an oddly named file) it is detected from the first bytes as they arrive, so
// something that is not a model is refused after 64 KB rather than after the
// whole stream, and a binary STL is read to exactly the length its header
// declares. size is the stream's length if known, or -1.
func loadModel(r io.Reader, name string, size int64) ([]float32, error) {
	if size > maxInputBytes() {
		return nil, errInputTooBig()
	}
	head := make([]byte, sniffBytes)
	n, err := io.ReadFull(r, head)
	head = head[:n]
	whole := err == io.EOF || err == io.ErrUnexpectedEOF // the stream ended inside the head
	if err != nil && !whole {
		return nil, errors.New("read error")
	}
	if n == 0 {
		return nil, errors.New("empty input")
	}

	f := formatFromName(name)
	if f == fmtUnknown {
		f = sniff(head)
	}
	if f == fmtSTL {
		f = fmtSTLBinary
		exact := whole && isBinarySTL(head) || size >= 84 && len(head) >= 84 && stlLength(head) == uint64(size)
		if !exact && looksLikeASCIISTL(head) {
			f = fmtSTLASCII
		}
	}

	switch f {
	case fmtUnknown:
		return nil, errors.New("not an STL, OBJ or 3MF file")
	case fmtSTLBinary:
		return readBinarySTL(head, whole, r, size)
	}
	if f == fmt3MF { // a zip is read from its end, so this one format needs the whole file
		d, err := readRest(head, whole, r, size, max3MFBytes())
		if err != nil {
			return nil, err
		}
		return read3MF(d)
	}
	var text io.Reader = bytes.NewReader(head)
	if !whole {
		text = io.MultiReader(text, r)
	}
	text = &capReader{r: text, left: maxInputBytes() + 1}
	if f == fmtSTLASCII {
		return readASCIISTL(text)
	}
	return readOBJ(text)
}

// loadTriangles is loadModel for data already in memory.
func loadTriangles(name string, d []byte) ([]float32, error) {
	return loadModel(bytes.NewReader(d), name, int64(len(d)))
}

func errInputTooBig() error {
	return fmt.Errorf("larger than %d MB, the most rndr will read at this --max-triangles", maxInputBytes()>>20)
}

// readRest reads the remainder of a stream after its head into memory,
// refusing to go past limit.
func readRest(head []byte, whole bool, r io.Reader, size, limit int64) ([]byte, error) {
	tooBig := fmt.Errorf("larger than %d MB, the most rndr will read of this kind of file at this --max-triangles", limit>>20)
	if whole {
		return head, nil
	}
	if size > limit {
		return nil, tooBig
	}
	if size >= int64(len(head)) { // known length: one allocation, no regrowth
		d := make([]byte, size)
		copy(d, head)
		n, err := io.ReadFull(r, d[len(head):])
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return nil, errors.New("read error")
		}
		return d[:len(head)+n], nil
	}
	// unknown length: collect fixed-size pieces, then join them once
	pieces, total := [][]byte{head}, int64(len(head))
	for {
		c := make([]byte, 4<<20)
		n, err := io.ReadFull(r, c)
		if total += int64(n); total > limit {
			return nil, tooBig
		}
		pieces = append(pieces, c[:n])
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, errors.New("read error")
		}
	}
	d := make([]byte, 0, total)
	for _, p := range pieces {
		d = append(d, p...)
	}
	return d, nil
}

// ---- STL -------------------------------------------------------------------

// stlLength is how long a binary STL says it is: header, count, 50 bytes a triangle.
func stlLength(head []byte) uint64 {
	return 84 + 50*uint64(binary.LittleEndian.Uint32(head[80:]))
}

func isBinarySTL(d []byte) bool {
	return len(d) >= 84 && stlLength(d) == uint64(len(d))
}

// readBinarySTL reads exactly as many bytes as the header's triangle count calls
// for, after checking that count against the limit: a lying header cannot make
// rndr allocate or read more than the triangle limit allows.
func readBinarySTL(head []byte, whole bool, r io.Reader, size int64) ([]float32, error) {
	if len(head) < 84 {
		return nil, errors.New("too short to be an STL file")
	}
	n := uint64(binary.LittleEndian.Uint32(head[80:]))
	want := 84 + 50*n
	if n == 0 || (whole && want > uint64(len(head))) || (size >= 0 && want > uint64(size)) {
		return nil, errors.New("not an STL file (or a truncated one)")
	}
	if n > uint64(maxTriangles) {
		return nil, errTooManyTriangles()
	}
	d := head
	if want > uint64(len(head)) {
		d = make([]byte, want)
		copy(d, head)
		if _, err := io.ReadFull(r, d[len(head):]); err != nil {
			return nil, errors.New("not an STL file (or a truncated one)")
		}
	}
	t := make([]float32, n*9)
	for i := 0; i < int(n); i++ {
		rec := d[84+50*i+12:] // skip the stored normal
		for k := 0; k < 9; k++ {
			t[9*i+k] = math.Float32frombits(binary.LittleEndian.Uint32(rec[4*k:]))
		}
	}
	return t, nil
}

func isSpace(b byte) bool { return b <= ' ' }

// The two text formats are parsed as they are read, a token or a line at a
// time, so the input itself is never held in memory: only the triangles are.

// capReader fails, rather than quietly ending, once more than its allowance has
// been read. It bounds how long a text parser can be kept busy.
type capReader struct {
	r    io.Reader
	left int64 // set to the allowance + 1
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errInputTooBig()
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if err != nil && err != io.EOF {
		err = errors.New("read error")
	}
	return n, err
}

// maxLineBytes caps one OBJ line or STL token; real ones are tens of bytes.
const maxLineBytes = 1 << 20

func scanErr(sc *bufio.Scanner, what string) error {
	if err := sc.Err(); err == bufio.ErrTooLong {
		return fmt.Errorf("%s longer than %d bytes", what, maxLineBytes)
	} else if err != nil {
		return err
	}
	return nil
}

// scanTokens is a bufio.SplitFunc: runs of bytes above the space character.
func scanTokens(data []byte, atEOF bool) (advance int, token []byte, err error) {
	start := 0
	for start < len(data) && isSpace(data[start]) {
		start++
	}
	for i := start; i < len(data); i++ {
		if isSpace(data[i]) {
			return i + 1, data[start:i], nil
		}
	}
	if atEOF && len(data) > start {
		return len(data), data[start:], nil
	}
	return start, nil, nil
}

func readASCIISTL(r io.Reader) ([]float32, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	sc.Split(scanTokens)
	var t []float32
	for sc.Scan() {
		if !bytes.EqualFold(sc.Bytes(), []byte("vertex")) {
			continue
		}
		for k := 0; k < 3; k++ {
			if !sc.Scan() {
				if err := scanErr(sc, "token"); err != nil {
					return nil, err
				}
				return nil, errors.New("ASCII STL ends in the middle of a vertex")
			}
			f, err := strconv.ParseFloat(string(sc.Bytes()), 32)
			if err != nil {
				return nil, fmt.Errorf("bad vertex in ASCII STL (triangle %d)", len(t)/9+1)
			}
			t = append(t, float32(f))
		}
		if len(t)/9 > maxTriangles {
			return nil, errTooManyTriangles()
		}
	}
	if err := scanErr(sc, "token"); err != nil {
		return nil, err
	}
	return t[:len(t)-len(t)%9], nil
}

// ---- OBJ -------------------------------------------------------------------

func readOBJ(r io.Reader) ([]float32, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	var verts, out []float32
	var idx []int
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) < 3 || (line[1] != ' ' && line[1] != '\t') {
			continue
		}
		switch line[0] {
		case 'v':
			f := bytes.Fields(line[2:])
			if len(f) < 3 {
				continue
			}
			if len(verts)/3 >= 3*maxTriangles { // more vertices than the allowed triangles could ever use
				return nil, errTooManyTriangles()
			}
			for k := 0; k < 3; k++ {
				x, err := strconv.ParseFloat(string(f[k]), 32)
				if err != nil {
					x = math.NaN() // the triangles using it get dropped later
				}
				verts = append(verts, float32(x))
			}
		case 'f':
			// each corner is v, v/vt, v//vn or v/vt/vn; indices are 1-based, negative = from the end
			idx = idx[:0]
			nv := len(verts) / 3
			for _, f := range bytes.Fields(line[2:]) {
				if s := bytes.IndexByte(f, '/'); s >= 0 {
					f = f[:s]
				}
				k, err := strconv.ParseInt(string(f), 10, 32)
				if err != nil {
					continue
				}
				if k < 0 {
					k += int64(nv)
				} else {
					k--
				}
				if k >= 0 && k < int64(nv) {
					idx = append(idx, int(k))
				}
			}
			if len(out)/9+len(idx) > maxTriangles { // a polygon of n corners becomes n-2 triangles
				return nil, errTooManyTriangles()
			}
			for k := 2; k < len(idx); k++ { // fan-triangulate polygons
				for _, v := range [3]int{idx[0], idx[k-1], idx[k]} {
					out = append(out, verts[3*v], verts[3*v+1], verts[3*v+2])
				}
			}
		}
	}
	if err := scanErr(sc, "line"); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("no faces found in OBJ file")
	}
	return out, nil
}
