package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// A 3MF file is a zip. The root model part lists objects (meshes, or assemblies of
// other objects with transforms) and a build: the placed items that make up the
// scene. Slicers such as Bambu/Orca/Prusa keep the meshes in further model parts
// and point at them with a path attribute (the "production" extension).
//
// Nothing is ever written to disk and a path only ever names a part inside the
// zip, so there is no way for a file to reach outside itself. The XML is read with
// the small scanner below rather than encoding/xml: it keeps nothing but the
// current tag, so text, comments and oversized tags cannot be used to exhaust
// memory, and it has no notion of entities or DTDs at all.

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

	xmlLeft   int64           // decompressed bytes still allowed
	nVerts    int             // vertices parsed so far, all parts
	nTris     int             // triangles parsed so far, all parts
	instances int             // objects placed so far
	active    map[string]bool // objects currently being expanded (cycle guard)
}

func partName(p string) string {
	return strings.ToLower(strings.TrimLeft(strings.ReplaceAll(p, "\\", "/"), "/"))
}

func read3MF(d []byte) ([]float32, error) {
	zr, err := zip.NewReader(bytes.NewReader(d), int64(len(d)))
	if err != nil {
		return nil, errors.New("not a 3MF file (not a zip archive)")
	}
	z := &tmfFile{
		parts:   map[string]*zip.File{},
		models:  map[string]*tmfModel{},
		active:  map[string]bool{},
		xmlLeft: maxXMLBytes(),
	}
	for _, f := range zr.File {
		z.parts[partName(f.Name)] = f
	}

	root, err := z.rootPart()
	if err != nil {
		return nil, err
	}
	if root == "" {
		return nil, errors.New("no 3D model found inside the 3MF file")
	}
	m, err := z.model(root)
	if err != nil {
		return nil, err
	}
	if len(m.items) > 0 {
		for _, it := range m.items {
			if err := z.emit(root, it, identity, 0); err != nil {
				return nil, err
			}
		}
	} else {
		for _, id := range m.order { // no build section: show every mesh
			if err := z.emit(root, tmfRef{id: id, xf: identity}, identity, 0); err != nil {
				return nil, err
			}
		}
	}
	if len(z.out) == 0 {
		return nil, errors.New("no triangles found in the 3MF file")
	}
	return z.out, nil
}

// open starts reading one part's XML, charged against the file's byte budget.
func (z *tmfFile) open(part string) (*tagScanner, io.Closer, error) {
	f := z.parts[part]
	if f == nil {
		return nil, nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, nil, errors.New("3MF part cannot be read")
	}
	return &tagScanner{r: bufio.NewReaderSize(&budgetReader{r: rc, left: &z.xmlLeft}, 64<<10)}, rc, nil
}

// rootPart finds the main model: the package relationships say where it is.
func (z *tmfFile) rootPart() (string, error) {
	sc, c, err := z.open("_rels/.rels")
	if err != nil {
		return "", err
	}
	if sc != nil {
		defer c.Close()
		for {
			tag, err := sc.next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			name, closing, rest := splitTag(tag)
			if closing || string(name) != "Relationship" {
				continue
			}
			var typ, target string
			err = eachAttr(rest, func(k, v []byte) {
				switch string(k) {
				case "Type":
					typ = string(v)
				case "Target":
					target = unescape(v)
				}
			})
			if err != nil {
				return "", err
			}
			if strings.HasSuffix(typ, "/3dmodel") && z.parts[partName(target)] != nil {
				return partName(target), nil
			}
		}
	}
	if z.parts["3d/3dmodel.model"] != nil {
		return "3d/3dmodel.model", nil
	}
	best := ""
	for name := range z.parts { // deterministic pick: the first in name order
		if strings.HasSuffix(name, ".model") && (best == "" || name < best) {
			best = name
		}
	}
	return best, nil
}

func parseF32(v []byte) float32 {
	f, err := strconv.ParseFloat(string(bytes.TrimSpace(v)), 32)
	if err != nil {
		return float32(math.NaN()) // the triangles using it get dropped later
	}
	return float32(f)
}

// model parses one model part (cached). A part that is not in the zip is nil.
func (z *tmfFile) model(part string) (*tmfModel, error) {
	if m, ok := z.models[part]; ok {
		return m, nil
	}
	sc, c, err := z.open(part)
	if err != nil || sc == nil {
		return nil, err
	}
	defer c.Close()

	m := &tmfModel{objects: map[string]*tmfObject{}}
	z.models[part] = m
	var cur *tmfObject
	ref := func(rest []byte) (tmfRef, error) {
		r := tmfRef{xf: identity}
		err := eachAttr(rest, func(k, v []byte) {
			switch string(k) {
			case "objectid":
				r.id = unescape(v)
			case "transform":
				r.xf = parseXform(string(v))
			case "path": // p:path, from the production extension
				r.path = partName(unescape(v))
			}
		})
		return r, err
	}
	for {
		tag, err := sc.next()
		if err == io.EOF {
			return m, nil
		}
		if err != nil {
			return nil, err
		}
		name, closing, rest := splitTag(tag)
		if closing {
			if string(name) == "object" {
				cur = nil
			}
			continue
		}
		switch string(name) {
		case "object":
			cur = &tmfObject{}
			err = eachAttr(rest, func(k, v []byte) {
				if string(k) == "id" {
					id := unescape(v)
					m.objects[id] = cur
					m.order = append(m.order, id)
				}
			})
			if len(rest) > 0 && rest[len(rest)-1] == '/' { // <object ... /> has no body
				cur = nil
			}
		case "vertex":
			if cur == nil {
				continue
			}
			if z.nVerts++; z.nVerts > 3*maxTriangles {
				return nil, errTooManyTriangles()
			}
			var x, y, w float32
			err = eachAttr(rest, func(k, v []byte) {
				switch string(k) {
				case "x":
					x = parseF32(v)
				case "y":
					y = parseF32(v)
				case "z":
					w = parseF32(v)
				}
			})
			cur.verts = append(cur.verts, x, y, w)
		case "triangle":
			if cur == nil {
				continue
			}
			if z.nTris++; z.nTris > maxTriangles {
				return nil, errTooManyTriangles()
			}
			v := [3]int32{-1, -1, -1}
			err = eachAttr(rest, func(k, val []byte) {
				if len(k) == 2 && k[0] == 'v' && k[1] >= '1' && k[1] <= '3' {
					// 32-bit parse: an oversized index is rejected here rather than wrapping
					if n, err := strconv.ParseInt(string(bytes.TrimSpace(val)), 10, 32); err == nil {
						v[k[1]-'1'] = int32(n)
					}
				}
			})
			cur.tris = append(cur.tris, v[0], v[1], v[2])
		case "component":
			if cur != nil {
				var r tmfRef
				if r, err = ref(rest); err == nil {
					if z.instances++; z.instances > maxInstances {
						return nil, errTooManyInstances()
					}
					cur.comps = append(cur.comps, r)
				}
			}
		case "item":
			var r tmfRef
			if r, err = ref(rest); err == nil {
				if z.instances++; z.instances > maxInstances {
					return nil, errTooManyInstances()
				}
				m.items = append(m.items, r)
			}
		}
		if err != nil {
			return nil, err
		}
	}
}

func errTooManyInstances() error {
	return fmt.Errorf("3MF scene places more than %d objects (the limit)", maxInstances)
}

// emit appends the triangles of one placed object, following assemblies down.
func (z *tmfFile) emit(part string, r tmfRef, parent xform, depth int) error {
	if depth > 64 {
		return nil
	}
	if z.instances++; z.instances > maxInstances {
		return errTooManyInstances()
	}
	if r.path != "" {
		part = r.path
	}
	m, err := z.model(part)
	if err != nil || m == nil {
		return err
	}
	obj := m.objects[r.id]
	if obj == nil {
		return nil
	}
	key := part + "\x00" + r.id
	if z.active[key] { // an object that (directly or not) contains itself
		return nil
	}
	z.active[key] = true
	defer delete(z.active, key)

	if len(z.out)/9+len(obj.tris)/3 > maxTriangles {
		return errTooManyTriangles()
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
		if err := z.emit(part, c, xf, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// ---- XML tag scanner ---------------------------------------------------------

var (
	errXMLTooBig = errors.New("3MF contents are too large when unpacked (possible zip bomb)")
	errBadXML    = errors.New("malformed XML in 3MF file")
)

// budgetReader stops a read once the shared byte budget is spent.
type budgetReader struct {
	r    io.Reader
	left *int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if *b.left <= 0 {
		return 0, errXMLTooBig
	}
	if int64(len(p)) > *b.left {
		p = p[:*b.left]
	}
	n, err := b.r.Read(p)
	*b.left -= int64(n)
	if err != nil && err != io.EOF {
		err = errBadXML // zip-level damage (bad checksum, truncated stream)
	}
	return n, err
}

// tagScanner yields the tags of an XML document one at a time and remembers
// nothing else. Text between tags, comments, CDATA and processing instructions
// are skipped without being stored. Document type declarations are refused.
type tagScanner struct {
	r   *bufio.Reader
	buf []byte
}

func eofIsDamage(err error) error {
	if err == io.EOF {
		return errBadXML
	}
	return err
}

// next returns what lies between the next < and its closing >. io.EOF means the
// document ended cleanly between tags. The slice is only valid until the next call.
func (s *tagScanner) next() ([]byte, error) {
	for {
		for { // skip text
			_, err := s.r.ReadSlice('<')
			if err == nil {
				break
			}
			if err != bufio.ErrBufferFull {
				return nil, err
			}
		}
		c, err := s.r.ReadByte()
		if err != nil {
			return nil, eofIsDamage(err)
		}
		switch c {
		case '?':
			if err := s.skipPast("?>"); err != nil {
				return nil, err
			}
			continue
		case '!':
			if pk, _ := s.r.Peek(2); string(pk) == "--" {
				s.r.Discard(2)
				if err := s.skipPast("-->"); err != nil {
					return nil, err
				}
				continue
			}
			if pk, _ := s.r.Peek(7); string(pk) == "[CDATA[" {
				if err := s.skipPast("]]>"); err != nil {
					return nil, err
				}
				continue
			}
			return nil, errors.New("3MF contains a document type declaration, which is not allowed")
		}
		if c == '>' { // "<>": nothing to report
			continue
		}
		s.buf = append(s.buf[:0], c)
		quote := byte(0)
		if c == '"' || c == '\'' {
			quote = c
		}
		for {
			chunk, err := s.r.ReadSlice('>')
			if err != nil && err != bufio.ErrBufferFull {
				return nil, eofIsDamage(err)
			}
			if len(s.buf)+len(chunk) > maxTagBytes {
				return nil, errors.New("3MF contains an oversized XML tag")
			}
			for _, b := range chunk {
				if quote != 0 {
					if b == quote {
						quote = 0
					}
				} else if b == '"' || b == '\'' {
					quote = b
				}
			}
			s.buf = append(s.buf, chunk...)
			if err == nil && quote == 0 { // a > inside a quoted value does not end the tag
				return s.buf[:len(s.buf)-1], nil
			}
		}
	}
}

// skipPast consumes input up to and including pat (2 or 3 bytes long).
func (s *tagScanner) skipPast(pat string) error {
	n := 0
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			return eofIsDamage(err)
		}
		switch {
		case b == pat[n]:
			if n++; n == len(pat) {
				return nil
			}
		case b == pat[0]:
			if !(n == 2 && pat[1] == pat[0]) { // "--" followed by "-" is still "--"
				n = 1
			}
		default:
			n = 0
		}
	}
}

// localName drops a namespace prefix: p:path -> path.
func localName(n []byte) []byte {
	if i := bytes.LastIndexByte(n, ':'); i >= 0 {
		return n[i+1:]
	}
	return n
}

// splitTag separates a tag into its (local) name and the attribute text after it.
func splitTag(t []byte) (name []byte, closing bool, rest []byte) {
	if len(t) > 0 && t[0] == '/' {
		closing, t = true, t[1:]
	}
	i := 0
	for i < len(t) && !isSpace(t[i]) && t[i] != '/' {
		i++
	}
	return localName(t[:i]), closing, t[i:]
}

// eachAttr calls f for every name="value" pair in a tag's attribute text.
func eachAttr(rest []byte, f func(name, value []byte)) error {
	i := 0
	for {
		for i < len(rest) && (isSpace(rest[i]) || rest[i] == '/') {
			i++
		}
		if i >= len(rest) {
			return nil
		}
		st := i
		for i < len(rest) && rest[i] != '=' && !isSpace(rest[i]) {
			i++
		}
		name := rest[st:i]
		for i < len(rest) && isSpace(rest[i]) {
			i++
		}
		if i >= len(rest) || rest[i] != '=' {
			return errBadXML
		}
		i++
		for i < len(rest) && isSpace(rest[i]) {
			i++
		}
		if i >= len(rest) || (rest[i] != '"' && rest[i] != '\'') {
			return errBadXML
		}
		q := rest[i]
		i++
		st = i
		for i < len(rest) && rest[i] != q {
			i++
		}
		if i >= len(rest) {
			return errBadXML
		}
		f(localName(name), rest[st:i])
		i++
	}
}

var xmlEntities = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&amp;", "&")

// unescape resolves the five built-in XML entities in an id or path. There are
// no others: custom entities need a DTD, and those are refused.
func unescape(v []byte) string {
	if bytes.IndexByte(v, '&') < 0 {
		return string(v)
	}
	return xmlEntities.Replace(string(v))
}
