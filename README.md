# rndr

ASCII previewer for 3D models in the terminal: STL, OBJ and 3MF.

One static binary with no dependencies: no runtime to install, no external programs.
Written in Go using only the standard library.

## Install

Copy the binary for your platform from `dist/` to somewhere on your PATH and name it `rndr`:

| Machine | File |
|---|---|
| Mac (Apple silicon / Intel) | `rndr-darwin-arm64` / `rndr-darwin-amd64` |
| Linux or NAS, Intel/AMD | `rndr-linux-amd64` |
| Linux or NAS, 64-bit ARM | `rndr-linux-arm64` |
| Linux or NAS, 32-bit ARM | `rndr-linux-armv7` |
| Windows | `rndr-windows-amd64.exe` |

## Build

```bash
sh build.sh
```

```bash
sh build.sh release
```

The first builds `./rndr` for this machine; the second cross-compiles every platform into `dist/`.
Needs Go 1.21 or later.

## Use

```
rndr part.stl                     interactive viewer
rndr part.3mf 94 0 0 1.0          print one frame: RX RY RZ [ZOOM [PANX PANY]]
rndr part.obj -p                  print the default view
rndr part.stl 25 -30 0 -s 100x40 -m wire -o part.txt
rndr part.stl -p -s 100           100 columns wide, as many rows as the model needs
cat part.stl | rndr - -p
```

`rndr --help` lists every option. Rotation is tilt (X), spin (Y), roll (Z) in degrees;
`0 0 0` is the front view with model Z up. Zoom 1.0 fits the whole model.

Interactive keys: `w/s` tilt, `a/d` spin, `z/x` roll, `W/A/S/D` pan, `i/o` zoom,
`m` cycle mode, `r` reset, `p` save the frame, `?` help, `q` quit.

`p` writes `NAME.rndr.txt` into the current directory (the temp dir if that is read-only).
The status bar always shows the `view` numbers that reproduce the current frame from the
command line.

Modes: `both` (shading + feature edges), `wire` (feature edges and silhouette, hidden lines
removed), `mesh` (every triangle edge), `xray` (see-through mesh), `shade`.

Print mode sizes the frame from `-s`, then `FZF_PREVIEW_COLUMNS/LINES`, then the terminal,
then 80x24, so it works as a file-manager previewer:

```bash
fzf --preview 'rndr {} -p'
```

## Formats

- **STL**: binary and ASCII.
- **OBJ**: vertices and faces; polygons are triangulated; materials and textures are ignored.
- **3MF**: the whole build plate, including assemblies, per-item transforms and meshes kept
  in separate parts (as Bambu Studio, OrcaSlicer and PrusaSlicer write them). Colours and
  materials are ignored.

## Layout

- `main.go`: command line and the interactive loop
- `formats.go`: STL, OBJ and 3MF readers
- `mesh.go`: vertex welding, normals, edge detection
- `render.go`: projection, rasteriser, edge drawing, shading
- `term_*.go`: raw keyboard input and terminal size per operating system
- `java/`: the original Java version, kept for reference
