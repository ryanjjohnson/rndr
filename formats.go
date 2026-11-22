package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
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

// ---- 3MF -------------------------------------------------------------------
//
// A 3MF file is a zip. The root model part lists objects (meshes, or assemblies of
// other objects with transforms) and a build: the placed items that make up the
// scene. Slicers such as Bambu/Orca/Prusa keep the meshes in further model parts
// and point at them with a path attribute (the "production" extension).

type xform [12]float64 // row-vector affine: p' = p*A + t, stored as A rows then t

var identity = xform{1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0}

// then returns the transform that applies a first, then b.
func (a xform) then(b xform) (c xform) {
	for i := 0; i < 4; i++ {
		for j := 0; j < 3; j++ {
			c[3*i+j] = a[3*i]*b[j] + a[3*i+1]*b[3+j] + a[3*i+2]*b[6+j]
		}
	}
	c[9] += b[9]
	c[10] += b[10]
	c[11] += b[11]
	return
}

func parseXform(s string) xform {
	f := strings.Fields(s)
	if len(f) != 12 {
		return identity
	}
	var x xform
	for i := range x {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return identity
		}
		x[i] = v
	}
	return x
}

type tmfRef struct { // a component or build item: "this object, placed like so"
	path, id string
	xf       xform
}

type tmfObject struct {
	verts []float32
	tris  []int32
	comps []tmfRef
}

type tmfModel struct {
	objects map[string]*tmfObject
	order   []string
	items   []tmfRef
}

type tmfFile struct {
	parts  map[string]*zip.File // by normalised path
	models map[string]*tmfModel
	out    []float32
}

func partName(p string) string {
	return strings.ToLower(strings.TrimLeft(strings.ReplaceAll(p, "\\", "/"), "/"))
}

func read3MF(d []byte) ([]float32, error) {
	zr, err := zip.NewReader(bytes.NewReader(d), int64(len(d)))
	if err != nil {
		return nil, errors.New("not a 3MF file (not a zip archive)")
	}
	z := &tmfFile{parts: map[string]*zip.File{}, models: map[string]*tmfModel{}}
	for _, f := range zr.File {
		z.parts[partName(f.Name)] = f
	}

	root := z.rootPart()
	if root == "" {
		return nil, errors.New("no 3D model found inside the 3MF file")
	}
	m, err := z.model(root)
	if err != nil {
		return nil, err
	}
	if len(m.items) > 0 {
		for _, it := range m.items {
			z.emit(root, it, identity, 0)
		}
	} else {
		for _, id := range m.order { // no build section: show every mesh
			z.emit(root, tmfRef{id: id, xf: identity}, identity, 0)
		}
	}
	if len(z.out) == 0 {
		return nil, errors.New("no triangles found in the 3MF file")
	}
	return z.out, nil
}

// rootPart finds the main model: the package relationships say where it is.
func (z *tmfFile) rootPart() string {
	if f := z.parts["_rels/.rels"]; f != nil {
		if rc, err := f.Open(); err == nil {
			dec := xml.NewDecoder(rc)
			for {
				tok, err := dec.RawToken()
				if err != nil {
					break
				}
				el, ok := tok.(xml.StartElement)
				if !ok || el.Name.Local != "Relationship" {
					continue
				}
				var typ, target string
				for _, a := range el.Attr {
					switch a.Name.Local {
					case "Type":
						typ = a.Value
					case "Target":
						target = a.Value
					}
				}
				if strings.HasSuffix(typ, "/3dmodel") && z.parts[partName(target)] != nil {
					rc.Close()
					return partName(target)
				}
			}
			rc.Close()
		}
	}
	if z.parts["3d/3dmodel.model"] != nil {
		return "3d/3dmodel.model"
	}
	for name := range z.parts {
		if strings.HasSuffix(name, ".model") {
			return name
		}
	}
	return ""
}

// model parses one model part (cached).
func (z *tmfFile) model(part string) (*tmfModel, error) {
	if m, ok := z.models[part]; ok {
		return m, nil
	}
	f := z.parts[part]
	if f == nil {
		return nil, fmt.Errorf("3MF part %s is missing", part)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("3MF part %s: %v", part, err)
	}
	defer rc.Close()

	m := &tmfModel{objects: map[string]*tmfObject{}}
	z.models[part] = m
	var cur *tmfObject
	num := func(s string) float32 {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 32)
		if err != nil {
			return float32(math.NaN())
		}
		return float32(v)
	}
	ref := func(el xml.StartElement) tmfRef {
		r := tmfRef{xf: identity}
		for _, a := range el.Attr {
			switch a.Name.Local {
			case "objectid":
				r.id = a.Value
			case "transform":
				r.xf = parseXform(a.Value)
			case "path": // p:path, from the production extension
				r.path = partName(a.Value)
			}
		}
		return r
	}
	dec := xml.NewDecoder(rc)
	for {
		tok, err := dec.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("3MF part %s: %v", part, err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "object":
				cur = &tmfObject{}
				for _, a := range el.Attr {
					if a.Name.Local == "id" {
						m.objects[a.Value] = cur
						m.order = append(m.order, a.Value)
					}
				}
			case "vertex":
				if cur == nil {
					continue
				}
				var x, y, zz float32
				for _, a := range el.Attr {
					switch a.Name.Local {
					case "x":
						x = num(a.Value)
					case "y":
						y = num(a.Value)
					case "z":
						zz = num(a.Value)
					}
				}
				cur.verts = append(cur.verts, x, y, zz)
			case "triangle":
				if cur == nil {
					continue
				}
				v := [3]int32{-1, -1, -1}
				for _, a := range el.Attr {
					if len(a.Name.Local) == 2 && a.Name.Local[0] == 'v' && a.Name.Local[1] >= '1' && a.Name.Local[1] <= '3' {
						if n, err := strconv.Atoi(strings.TrimSpace(a.Value)); err == nil {
							v[a.Name.Local[1]-'1'] = int32(n)
						}
					}
				}
				cur.tris = append(cur.tris, v[0], v[1], v[2])
			case "component":
				if cur != nil {
					cur.comps = append(cur.comps, ref(el))
				}
			case "item":
				m.items = append(m.items, ref(el))
			}
		case xml.EndElement:
			if el.Name.Local == "object" {
				cur = nil
			}
		}
	}
	return m, nil
}

// emit appends the triangles of one placed object, following assemblies down.
func (z *tmfFile) emit(part string, r tmfRef, parent xform, depth int) {
	if depth > 32 { // a file that references itself
		return
	}
	if r.path != "" {
		part = r.path
	}
	m, err := z.model(part)
	if err != nil {
		return
	}
	obj := m.objects[r.id]
	if obj == nil {
		return
	}
	xf := r.xf.then(parent)
	nv := int32(len(obj.verts) / 3)
	for i := 0; i+3 <= len(obj.tris); i += 3 {
		a, b, c := obj.tris[i], obj.tris[i+1], obj.tris[i+2]
		if a < 0 || b < 0 || c < 0 || a >= nv || b >= nv || c >= nv {
			continue
		}
		for _, v := range [3]int32{a, b, c} {
			x, y, w := float64(obj.verts[3*v]), float64(obj.verts[3*v+1]), float64(obj.verts[3*v+2])
			z.out = append(z.out,
				float32(x*xf[0]+y*xf[3]+w*xf[6]+xf[9]),
				float32(x*xf[1]+y*xf[4]+w*xf[7]+xf[10]),
				float32(x*xf[2]+y*xf[5]+w*xf[8]+xf[11]))
		}
	}
	for _, c := range obj.comps {
		z.emit(part, c, xf, depth+1)
	}
}
