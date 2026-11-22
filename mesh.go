package main

import (
	"encoding/binary"
	"errors"
	"hash/maphash"
	"math"
	"runtime"
)

// Mesh is a welded triangle mesh, centred and scaled to fit a unit sphere.
type Mesh struct {
	nv, nt, ne    int
	vx, vy, vz    []float32 // vertices
	tri           []int32   // 3 vertex indices per triangle
	fnx, fny, fnz []float32 // face normals
	ea, eb        []int32   // unique edges: the two vertices...
	ef1, ef2      []int32   // ...and the faces on either side (ef2 is -1 on a boundary)
	eflag         []uint8   // edgeDir | edgeMismatch | edgeFeature

	dimX, dimY, dimZ float64 // original bounding box size
}

const (
	edgeDir      = 1 // direction the first face walked the edge
	edgeMismatch = 2 // the two faces wind in opposite senses
	edgeFeature  = 4 // crease, boundary or non-manifold edge: always drawn
)

// The hash tables below are keyed with a seed chosen at random on every run.
// With a fixed hash, a file could be built so that every vertex lands in the same
// slot, turning the linear-time weld into a quadratic one (hash flooding).
var hashSeed = maphash.MakeSeed()

func hashVertex(x, y, z float32) uint32 {
	var b [12]byte
	binary.LittleEndian.PutUint32(b[0:], math.Float32bits(x))
	binary.LittleEndian.PutUint32(b[4:], math.Float32bits(y))
	binary.LittleEndian.PutUint32(b[8:], math.Float32bits(z))
	return uint32(maphash.Bytes(hashSeed, b[:]))
}

func hashEdge(key uint64) uint32 {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], key)
	return uint32(maphash.Bytes(hashSeed, b[:]))
}

// buildMesh turns triangle soup (9 floats per triangle) into a Mesh.
// creaseDeg is the angle at which an edge between two faces counts as a feature.
func buildMesh(t []float32, creaseDeg float64) (*Mesh, error) {
	// drop triangles with non-finite coordinates
	n := 0
	for i := 0; i+9 <= len(t); i += 9 {
		ok := true
		for k := 0; k < 9; k++ {
			f := float64(t[i+k])
			if math.IsNaN(f) || math.IsInf(f, 0) {
				ok = false
			}
		}
		if ok {
			if n != i {
				copy(t[n:n+9], t[i:i+9])
			}
			n += 9
		}
	}
	m := &Mesh{nt: n / 9}
	if m.nt == 0 {
		return nil, errors.New("no triangles found")
	}
	if m.nt > maxTriangles {
		return nil, errTooManyTriangles()
	}

	// weld identical vertices
	nc := m.nt * 3
	capacity := 16
	for capacity < nc*2 && capacity < 1<<30 {
		capacity <<= 1
	}
	mask := uint32(capacity - 1)
	tab := make([]int32, capacity)
	for i := range tab {
		tab[i] = -1
	}
	px, py, pz := make([]float32, nc), make([]float32, nc), make([]float32, nc)
	m.tri = make([]int32, nc)
	nv := int32(0)
	for c := 0; c < nc; c++ {
		x, y, z := t[3*c]+0, t[3*c+1]+0, t[3*c+2]+0
		i := hashVertex(x, y, z) & mask
		for {
			v := tab[i]
			if v < 0 {
				tab[i] = nv
				px[nv], py[nv], pz[nv] = x, y, z
				m.tri[c] = nv
				nv++
				break
			}
			if px[v] == x && py[v] == y && pz[v] == z {
				m.tri[c] = v
				break
			}
			i = (i + 1) & mask
		}
	}
	m.nv = int(nv)
	m.vx = append([]float32(nil), px[:nv]...)
	m.vy = append([]float32(nil), py[:nv]...)
	m.vz = append([]float32(nil), pz[:nv]...)

	px, py, pz, tab, t = nil, nil, nil, nil, nil
	runtime.GC() // drop the weld tables before the edge tables are allocated

	// centre on the bounding box and scale to a unit sphere
	x0, y0, z0 := math.MaxFloat64, math.MaxFloat64, math.MaxFloat64
	x1, y1, z1 := -x0, -x0, -x0
	for i := 0; i < m.nv; i++ {
		x0, x1 = math.Min(x0, float64(m.vx[i])), math.Max(x1, float64(m.vx[i]))
		y0, y1 = math.Min(y0, float64(m.vy[i])), math.Max(y1, float64(m.vy[i]))
		z0, z1 = math.Min(z0, float64(m.vz[i])), math.Max(z1, float64(m.vz[i]))
	}
	m.dimX, m.dimY, m.dimZ = x1-x0, y1-y0, z1-z0
	cx, cy, cz := (x0+x1)/2, (y0+y1)/2, (z0+z1)/2
	r2 := 0.0
	for i := 0; i < m.nv; i++ {
		dx, dy, dz := float64(m.vx[i])-cx, float64(m.vy[i])-cy, float64(m.vz[i])-cz
		r2 = math.Max(r2, dx*dx+dy*dy+dz*dz)
	}
	inv := 1.0
	if r2 > 0 {
		inv = 1 / math.Sqrt(r2)
	}
	for i := 0; i < m.nv; i++ {
		m.vx[i] = float32((float64(m.vx[i]) - cx) * inv)
		m.vy[i] = float32((float64(m.vy[i]) - cy) * inv)
		m.vz[i] = float32((float64(m.vz[i]) - cz) * inv)
	}

	// face normals, computed from the vertices (normals stored in files are often junk)
	m.fnx, m.fny, m.fnz = make([]float32, m.nt), make([]float32, m.nt), make([]float32, m.nt)
	for f := 0; f < m.nt; f++ {
		a, b, c := m.tri[3*f], m.tri[3*f+1], m.tri[3*f+2]
		ux, uy, uz := float64(m.vx[b]-m.vx[a]), float64(m.vy[b]-m.vy[a]), float64(m.vz[b]-m.vz[a])
		wx, wy, wz := float64(m.vx[c]-m.vx[a]), float64(m.vy[c]-m.vy[a]), float64(m.vz[c]-m.vz[a])
		nx, ny, nz := uy*wz-uz*wy, uz*wx-ux*wz, ux*wy-uy*wx
		if l := math.Sqrt(nx*nx + ny*ny + nz*nz); l > 0 {
			m.fnx[f], m.fny[f], m.fnz[f] = float32(nx/l), float32(ny/l), float32(nz/l)
		}
	}

	m.buildEdges(creaseDeg)
	return m, nil
}

func (m *Mesh) buildEdges(creaseDeg float64) {
	nc := m.nt * 3
	capacity := 16
	for float64(capacity) < float64(nc)*1.5 && capacity < 1<<30 {
		capacity <<= 1
	}
	mask := uint32(capacity - 1)
	keys := make([]uint64, capacity)
	idx := make([]int32, capacity)
	const empty = ^uint64(0)
	for i := range keys {
		keys[i] = empty
	}
	m.ea, m.eb = make([]int32, nc), make([]int32, nc)
	m.ef1, m.ef2 = make([]int32, nc), make([]int32, nc)
	m.eflag = make([]uint8, nc)
	messy := make([]bool, nc) // shared by more than two faces
	ne := int32(0)
	for f := 0; f < m.nt; f++ {
		for k := 0; k < 3; k++ {
			a, b := m.tri[3*f+k], m.tri[3*f+(k+1)%3]
			if a == b {
				continue
			}
			fwd := a < b
			lo, hi := a, b
			if !fwd {
				lo, hi = b, a
			}
			key := uint64(lo)<<32 | uint64(hi)
			i := hashEdge(key) & mask
			for {
				if keys[i] == empty {
					keys[i] = key
					idx[i] = ne
					m.ea[ne], m.eb[ne], m.ef1[ne], m.ef2[ne] = lo, hi, int32(f), -1
					if fwd {
						m.eflag[ne] = edgeDir
					}
					ne++
					break
				}
				if keys[i] == key {
					e := idx[i]
					if m.ef2[e] < 0 {
						m.ef2[e] = int32(f)
						// a well-formed pair walks the shared edge in opposite directions
						if (m.eflag[e]&edgeDir != 0) == fwd {
							m.eflag[e] |= edgeMismatch
						}
					} else {
						messy[e] = true
					}
					break
				}
				i = (i + 1) & mask
			}
		}
	}
	m.ne = int(ne)
	cosCrease := math.Cos(creaseDeg * math.Pi / 180)
	for e := 0; e < m.ne; e++ {
		feat := true
		if m.ef2[e] >= 0 && !messy[e] {
			f, g := m.ef1[e], m.ef2[e]
			dot := float64(m.fnx[f]*m.fnx[g] + m.fny[f]*m.fny[g] + m.fnz[f]*m.fnz[g])
			if m.eflag[e]&edgeMismatch != 0 {
				dot = -dot
			}
			degenerate := (m.fnx[f] == 0 && m.fny[f] == 0 && m.fnz[f] == 0) ||
				(m.fnx[g] == 0 && m.fny[g] == 0 && m.fnz[g] == 0)
			feat = !degenerate && dot < cosCrease
		}
		if feat {
			m.eflag[e] |= edgeFeature
		}
	}
}
