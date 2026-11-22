package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
)

// loadTriangles reads a model file and returns triangle soup: 9 floats per triangle.
// The format comes from the file name, or from the content when the name does not say.
func loadTriangles(name string, d []byte) ([]float32, error) {
	switch strings.ToLower(path.Ext(strings.ReplaceAll(name, "\\", "/"))) {
	case ".stl":
		return readSTL(d)
	case ".obj":
		return readOBJ(d)
	case ".3mf":
		return read3MF(d)
	}
	switch {
	case bytes.HasPrefix(d, []byte("PK\x03\x04")):
		return read3MF(d)
	case isBinarySTL(d) || bytes.HasPrefix(bytes.ToLower(bytes.TrimSpace(d[:min(len(d), 4096)])), []byte("solid")):
		return readSTL(d)
	case bytes.HasPrefix(d, []byte("v ")) || bytes.Contains(d[:min(len(d), 1<<16)], []byte("\nv ")):
		return readOBJ(d)
	}
	return nil, errors.New("not an STL, OBJ or 3MF file")
}

// ---- STL -------------------------------------------------------------------

func isBinarySTL(d []byte) bool {
	return len(d) >= 84 && 84+50*uint64(binary.LittleEndian.Uint32(d[80:])) == uint64(len(d))
}

func readSTL(d []byte) ([]float32, error) {
	if !isBinarySTL(d) {
		head := strings.ToLower(strings.TrimSpace(string(d[:min(len(d), 4096)])))
		if strings.HasPrefix(head, "solid") && (strings.Contains(head, "facet") || len(d) < 84) {
			return readASCIISTL(d)
		}
	}
	if len(d) < 84 {
		return nil, errors.New("too short to be an STL file")
	}
	n := uint64(binary.LittleEndian.Uint32(d[80:]))
	if n == 0 || 84+50*n > uint64(len(d)) {
		return nil, errors.New("not an STL file (or a truncated one)")
	}
	if n > uint64(maxTriangles) {
		return nil, errTooManyTriangles()
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

func readASCIISTL(d []byte) ([]float32, error) {
	var t []float32
	i, n := 0, len(d)
	token := func() []byte {
		for i < n && isSpace(d[i]) {
			i++
		}
		st := i
		for i < n && !isSpace(d[i]) {
			i++
		}
		return d[st:i]
	}
	for i < n {
		if !bytes.EqualFold(token(), []byte("vertex")) {
			continue
		}
		for k := 0; k < 3; k++ {
			at := i
			f, err := strconv.ParseFloat(string(token()), 32)
			if err != nil {
				return nil, fmt.Errorf("bad vertex in ASCII STL near byte %d", at)
			}
			t = append(t, float32(f))
		}
		if len(t)/9 > maxTriangles {
			return nil, errTooManyTriangles()
		}
	}
	return t[:len(t)-len(t)%9], nil
}

// ---- OBJ -------------------------------------------------------------------

func readOBJ(d []byte) ([]float32, error) {
	var verts, out []float32
	var idx []int
	for len(d) > 0 {
		line := d
		if nl := bytes.IndexByte(d, '\n'); nl >= 0 {
			line, d = d[:nl], d[nl+1:]
		} else {
			d = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) < 3 || (line[1] != ' ' && line[1] != '\t') {
			continue
		}
		switch line[0] {
		case 'v':
			f := bytes.Fields(line[2:])
			if len(f) < 3 {
				continue
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
				k, err := strconv.Atoi(string(f))
				if err != nil {
					continue
				}
				if k < 0 {
					k += nv
				} else {
					k--
				}
				if k >= 0 && k < nv {
					idx = append(idx, k)
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
	if len(out) == 0 {
		return nil, errors.New("no faces found in OBJ file")
	}
	return out, nil
}
