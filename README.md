# rndr

<p align="center">
  <img src="docs/goose.gif" width="600" alt="A low-poly goose rendered as ASCII art, spinning while rndr cycles through its shaded, wireframe and mesh modes">
</p>

[![release](https://github.com/ryanjjohnson/rndr/actions/workflows/release.yml/badge.svg)](https://github.com/ryanjjohnson/rndr/actions/workflows/release.yml)
[![security](https://github.com/ryanjjohnson/rndr/actions/workflows/security.yml/badge.svg)](https://github.com/ryanjjohnson/rndr/actions/workflows/security.yml)
[![latest release](https://img.shields.io/github/v/release/ryanjjohnson/rndr)](https://github.com/ryanjjohnson/rndr/releases/latest)
[![licence: MIT](https://img.shields.io/github/license/ryanjjohnson/rndr)](LICENSE)
[![vibe coded with Claude](https://img.shields.io/badge/vibe%20coded-with%20Claude-d97757)](#the-prompt)

ASCII previewer for 3D models in the terminal: STL, OBJ and 3MF.

One static binary with no dependencies: no runtime to install, no external programs.
Written in Go using only the standard library.

## The Prompt

*Translated from grunts and sneers*

This program was written by Claude from a single request. Tidied up, it read:

> Build a console application that reads an STL file and renders it as ASCII art in the
> terminal, using as few external tools as possible.
>
> **Rendering**
> - Wireframe is the requirement; shading would be a welcome addition.
>
> **Interactive use:** `rndr [filename]`
> - Rotate with `W` `A` `S` `D`.
> - Pan with `Shift` + `W` `A` `S` `D`.
> - Zoom in and out with `i` and `o`.
> - Save the view exactly as currently framed (whatever the rotation, pan and zoom happen
>   to be) to a text file.
>
> **Non-interactive use:** `rndr somefile.stl 94 0 0 1.0`
> - The arguments are the X, Y and Z rotation followed by the zoom factor.
> - Print the render to stdout.
> - The exact argument format is flexible; do whatever makes sense.
>
> **Purpose:** previewing STL files from the command line while browsing a NAS.

The original asked for zsh. That lasted about four seconds before being amended to
"something very portable", which became Java and then, so that it could be shared with
people whose NAS has no Java, Go. OBJ and 3MF support, width-only sizing, the release
pipeline and this section were follow-up requests in the same conversation.

## Install

Download the binary for your platform from the
[latest release](https://github.com/ryanjjohnson/rndr/releases/latest), put it somewhere on
your PATH and name it `rndr` (on Mac and Linux, also `chmod +x rndr`):

| Machine | File |
|---|---|
| Mac (Apple silicon / Intel) | `rndr-darwin-arm64` / `rndr-darwin-amd64` |
| Linux or NAS, Intel/AMD | `rndr-linux-amd64` |
| Linux or NAS, 64-bit ARM | `rndr-linux-arm64` |
| Linux or NAS, 32-bit ARM | `rndr-linux-armv7` |
| Windows | `rndr-windows-amd64.exe` |

Every release binary carries signed build provenance: proof that it was built by this
repository's GitHub Actions workflow from the tagged commit. To check a download, with the
[GitHub CLI](https://cli.github.com) installed:

```bash
gh attestation verify rndr-linux-amd64 --repo ryanjjohnson/rndr
```

`SHA256SUMS.txt` in each release covers accidental corruption if you would rather not install
anything.

On a Mac, a downloaded binary is quarantined; clear that with
`xattr -d com.apple.quarantine rndr`, or build from source.

## Build

```bash
sh build.sh
```

```bash
sh build.sh release
```

The first builds `./rndr` for this machine; the second cross-compiles every platform into `dist/`.
Needs Go 1.21 or later.

Every push to `main` that changes the program publishes a new release automatically, bumping
the patch version. To choose the version yourself, push a tag such as `v0.2.0`.
The `security` workflow runs `govulncheck`, CodeQL, the hostile-input tests and a fuzzing pass on
every push and once a week.

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

## Security

Model files come from anywhere, so rndr treats every input as hostile.

**What a file cannot do**

- **Corrupt memory or run code.** rndr is Go with no C code and no third-party packages; every
  array access is bounds-checked. The only `unsafe` code talks to the terminal driver and never
  touches file data.
- **Reach outside itself.** rndr opens the one file you name, read-only. A 3MF's internal paths
  only ever select entries inside its own zip; nothing is extracted to disk. There is no network
  access, and no other program is ever run.
- **Abuse XML.** 3MF is read by a small tag scanner that has no entities or DTDs (a file with a
  DTD is refused), so XXE and "billion laughs" do not apply.
- **Take over the terminal.** A render contains only the characters `` .,:;=+*#%@-|/\ ``, spaces
  and newlines. File names and error text are stripped of control characters before printing.
- **Exhaust the machine.** Everything a file can make rndr spend is capped:

| Limit | Default | Stops |
|---|---|---|
| Triangles | 5,000,000 (`--max-triangles`) | huge or lying files; together with the next row, keeps peak memory under about 2 GB |
| Input size | 300 bytes per allowed triangle (1.4 GB) | endless input, devices and pipes passed as files |
| Unpacked 3MF XML | 100 bytes per allowed triangle (500 MB) | zip bombs |
| One XML tag | 64 KB | single enormous allocations |
| Objects placed by a 3MF | 1,000,000 | assemblies that reference each other exponentially |
| Drawing work per frame | 2 billion samples | files where every triangle covers the whole frame |
| Frame size | 2000 x 1000 | absurd `-s` values or terminal sizes |

  Hash tables are keyed with a per-run random seed, so a file cannot be crafted to make loading
  quadratic. On a machine with little memory, lower `--max-triangles`: 1,000,000 keeps rndr under
  about 400 MB.

**How this is checked.** `security_test.go` holds one test per attack (component bomb, zip bomb,
oversized tag, DTD/entities, path escape, lying STL headers, hash flooding, overdraw, extreme and
non-finite numbers, escape sequences) and fuzz targets for every format. CI runs them on every
push, on 64-bit and 32-bit, with the race detector, plus a fuzzing pass, `govulncheck` and CodeQL.

**What is not covered.** A crash from running out of memory is still possible on a machine with
less RAM than the limits allow; it ends the program and nothing else. `-o` and the `p` key write
exactly where you tell them to, with your permissions. Release binaries carry signed
build provenance (see Install), which shows where a binary was built, not that the source is
free of bugs; they are not notarised by Apple or signed for Windows SmartScreen.

Found a problem? Please open an issue, or email the address in the commit log for anything
sensitive.

## Layout

- `main.go`: command line and the interactive loop
- `formats.go`: STL and OBJ readers
- `threemf.go`: 3MF reader and its XML tag scanner
- `limits.go`: the hard limits on what a file may cost
- `security_test.go`: hostile-input tests and fuzz targets
- `mesh.go`: vertex welding, normals, edge detection
- `render.go`: projection, rasteriser, edge drawing, shading
- `term_*.go`: raw keyboard input and terminal size per operating system
- `docs/`: the animation above and `make-demo.py`, the script that regenerates it
- `testdata/`: a small model and its expected render, used by the release smoke test

## Licence

MIT. See [LICENSE](LICENSE).
