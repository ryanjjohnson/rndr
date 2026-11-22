package main

import (
	"fmt"
	"math"
	"strconv"
)

// Hard limits on what one input file may cost. Model files come from untrusted
// places; nothing in a file can make rndr use more than these allow.
var (
	// maxTriangles caps the mesh, and through it memory and load time: the mesh
	// needs up to about 300 bytes per triangle. Set with --max-triangles.
	maxTriangles = 5_000_000

	// maxInstances caps how many placed objects a 3MF scene may expand to.
	maxInstances = 1_000_000

	// maxRenderWork caps one frame's rasterising, in samples visited. It stops
	// files built to make every triangle cover the whole frame.
	maxRenderWork = int64(2e9)
)

// maxInputBytes caps how much of a file (or stdin) is read. The whole file is
// held in memory while it is parsed. 300 bytes per triangle is more than even
// the wordiest format (ASCII STL) needs.
func maxInputBytes() int64 {
	return min(math.MaxInt32, max(64<<20, int64(maxTriangles)*300))
}

// hardMaxTriangles is the most --max-triangles accepts: beyond it the index
// arithmetic would no longer fit the platform's int.
const hardMaxTriangles = 100_000_000*(strconv.IntSize/64) + 10_000_000*(1-strconv.IntSize/64) // 100M on 64-bit, 10M on 32-bit

// maxXMLBytes caps the decompressed XML read from one 3MF (the zip-bomb guard).
func maxXMLBytes() int64 { return max(64<<20, int64(maxTriangles)*100) }

// maxTagBytes caps a single XML tag; real ones are a few hundred bytes.
const maxTagBytes = 64 << 10

func errTooManyTriangles() error {
	return fmt.Errorf("more than %d triangles (the limit; raise it with --max-triangles)", maxTriangles)
}
