package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const helpLine = " ws tilt | ad spin | zx roll | WASD pan | i/o zoom | m mode | r reset | p save | ? help | q quit"

func runInteractive(s *system, t terminal, r *Renderer, v *View, name string) error {
	size := func() (int, int, bool) { // never trust the terminal to report something sane
		c, rw, ok := t.Size()
		return min(c, maxCols), min(rw, maxRows), ok && c > 0 && rw > 0
	}
	cols, rows, ok := size()
	if !ok {
		t.Restore()
		return errors.New("cannot read the terminal size; use -p to print a frame instead")
	}
	t.Write([]byte("\x1b[?1049h\x1b[?25l"))
	leave := func() {
		t.Write([]byte("\x1b[0m\x1b[?25h\x1b[?1049l"))
		t.Restore()
	}
	defer leave()
	defer s.onInterrupt(leave)()

	keys := make(chan []byte)
	done := make(chan struct{})
	defer close(done)
	go func() {
		defer close(keys)
		buf := make([]byte, 256)
		for {
			n, err := t.Read(buf)
			if n > 0 {
				select {
				case keys <- append([]byte(nil), buf[:n]...):
				case <-done:
					return
				}
			}
			if err != nil || n == 0 {
				return
			}
		}
	}()
	resize := time.NewTicker(250 * time.Millisecond)
	defer resize.Stop()

	msg, help, dirty := "", false, true
	for {
		if dirty {
			status := msg
			if help {
				status = helpLine
			}
			if err := draw(t, r, v, name, cols, rows, status); err != nil {
				return err
			}
			dirty = false
		}
		select {
		case <-resize.C:
			if c, rw, ok := size(); ok && (c != cols || rw != rows) {
				cols, rows, dirty = c, rw, true
			}
		case b, open := <-keys:
			if !open {
				return nil
			}
			msg = ""
			for i := 0; i < len(b); i++ {
				k := b[i]
				if k == 27 {
					if i+2 < len(b) && (b[i+1] == '[' || b[i+1] == 'O') {
						k = 0
						if arrow := strings.IndexByte("ABCD", b[i+2]); arrow >= 0 { // up, down, right, left
							k = "wsda"[arrow]
						}
						i += 2
					} else if len(b) == 1 {
						return nil
					} else {
						continue
					}
				}
				switch k {
				case 'w':
					v.rotX -= rotStep
				case 's':
					v.rotX += rotStep
				case 'a':
					v.rotY -= rotStep
				case 'd':
					v.rotY += rotStep
				case 'z':
					v.rotZ += rotStep
				case 'x':
					v.rotZ -= rotStep
				case 'W':
					v.panY += panStep
				case 'S':
					v.panY -= panStep
				case 'A':
					v.panX -= panStep
				case 'D':
					v.panX += panStep
				case 'i', '+', '=':
					v.zoom = math.Min(500, v.zoom*zoomStep)
				case 'o', '-':
					v.zoom = math.Max(0.02, v.zoom/zoomStep)
				case 'm':
					v.mode = (v.mode + 1) % len(modes)
				case 'r', '0':
					v.rotX, v.rotY, v.rotZ = defRX, defRY, defRZ
					v.zoom, v.panX, v.panY = 1, 0, 0
				case 'p':
					if text, err := r.FrameText(v, cols, max(1, rows-1)); err == nil {
						msg = save(s, text, name, v)
					}
				case '?', 'h':
					help = !help
				case 'q', 3, 4:
					return nil
				}
				v.rotX, v.rotY, v.rotZ = wrap(v.rotX), wrap(v.rotY), wrap(v.rotZ)
				// keep the view exactly what the status bar shows, so those numbers reproduce it
				v.zoom, v.panX, v.panY = round3(v.zoom), round3(v.panX), round3(v.panY)
				// panning further than this only moves an already invisible model
				v.panX, v.panY = math.Max(-1e4, math.Min(1e4, v.panX)), math.Max(-1e4, math.Min(1e4, v.panY))
			}
			dirty = true
		}
	}
}

func draw(t terminal, r *Renderer, v *View, name string, cols, rows int, msg string) error {
	vr := max(1, rows-1)
	g, err := r.Render(v, cols, vr)
	if err != nil {
		return err
	}
	var sb bytes.Buffer
	sb.Grow(cols*rows + rows*8 + 64)
	sb.WriteString("\x1b[H")
	for row := 0; row < vr; row++ {
		sb.Write(bytes.TrimRight(g[row*cols:(row+1)*cols], " "))
		sb.WriteString("\x1b[K\r\n")
	}
	status := msg
	if status == "" {
		m := r.m
		status = fmt.Sprintf(" %s | %d tris | %s x %s x %s | %s | view %s | ? help",
			name, m.nt, num(m.dimX, 1), num(m.dimY, 1), num(m.dimZ, 1), modes[v.mode], viewArgs(v))
	}
	// The status bar shows a file name, which is not ours: plain ASCII only.
	line := make([]byte, 0, cols)
	for i := 0; i < len(status) && len(line) < cols-1; i++ {
		ch := status[i]
		if ch < ' ' || ch >= 127 {
			ch = '?'
		}
		line = append(line, ch)
	}
	for len(line) < cols-1 {
		line = append(line, ' ')
	}
	sb.WriteString("\x1b[7m")
	sb.Write(line)
	sb.WriteString("\x1b[0m\x1b[K")
	t.Write(sb.Bytes())
	return nil
}

// saveName turns a model's file name into the stem of the file the p key writes.
// The name is someone else's: it must not be able to leave the directory or hide.
func saveName(name string) string {
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	base = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f || r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, base)
	base = strings.TrimLeft(base, ".-") // no hidden files, nothing that reads as an option
	if base == "" {
		base = "model"
	}
	if len(base) > 100 {
		base = base[:100]
	}
	return base
}

// save writes the frame next to where you are (or to the temp dir if that is read-only).
func save(s *system, text, name string, v *View) string {
	base := saveName(name)
	var lastErr error
	for _, dir := range s.saveDirs() {
		for n := 1; n < 1000; n++ {
			suffix := ""
			if n > 1 {
				suffix = "-" + strconv.Itoa(n)
			}
			p := filepath.Join(dir, base+".rndr"+suffix+".txt")
			err := s.createNew(p, []byte(text))
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			if err != nil {
				lastErr = err
				break
			}
			return " saved " + p + "   (rndr " + name + " " + viewArgs(v) + ")"
		}
	}
	return fmt.Sprintf(" save failed: %v", lastErr)
}
