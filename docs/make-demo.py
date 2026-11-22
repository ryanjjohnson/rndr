#!/usr/bin/env python3
"""Regenerates the README animation: a model spinning while rndr cycles through its modes.

    python3 docs/make-demo.py MODEL [OUT.gif]

Needs ./rndr (sh build.sh), Pillow, and a monospace TrueType font (Menlo or DejaVu Sans Mono).
"""
import os
import subprocess
import sys

from PIL import Image, ImageDraw, ImageFont

COLS, ROWS = 72, 36
TILT, ZOOM = 15, 1.0
STEP = 6                              # degrees per frame
MODES = ["both", "wire", "shade", "mesh"]
FRAMES_PER_MODE = 30                  # 4 modes x 30 frames x 6 degrees = two full turns
FRAME_MS = 80
BG, FG, DIM = (13, 17, 23), (201, 209, 217), (110, 118, 129)
FONTS = ["/System/Library/Fonts/Menlo.ttc",
         "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"]

model = sys.argv[1]
out = sys.argv[2] if len(sys.argv) > 2 else os.path.join(os.path.dirname(__file__), "goose.gif")
rndr = os.path.join(os.path.dirname(__file__), "..", "rndr")
font = ImageFont.truetype(next(f for f in FONTS if os.path.exists(f)), 14)
cw, lh, pad = font.getlength("M"), 17, 12
size = (round(COLS * cw) + 2 * pad, (ROWS + 2) * lh + 2 * pad)

frames = []
for i in range(len(MODES) * FRAMES_PER_MODE):
    mode = MODES[i // FRAMES_PER_MODE]
    spin = (-30 + i * STEP + 180) % 360 - 180
    view = [str(TILT), str(spin), "0", str(ZOOM)]
    text = subprocess.run([rndr, model, *view, "-m", mode, "-s", f"{COLS}x{ROWS}"],
                          check=True, capture_output=True, text=True).stdout
    img = Image.new("RGB", size, BG)
    d = ImageDraw.Draw(img)
    for row, line in enumerate(text.split("\n")):
        d.text((pad, pad + row * lh), line, font=font, fill=FG)
    d.text((pad, pad + (ROWS + 1) * lh), f"$ rndr goose.stl {' '.join(view)} -m {mode}", font=font, fill=DIM)
    frames.append(img)

# one shared palette keeps the file small and stops colours flickering between frames
palette = frames[0].quantize(colors=16, dither=Image.Dither.NONE)
frames = [f.quantize(palette=palette, dither=Image.Dither.NONE) for f in frames]
frames[0].save(out, save_all=True, append_images=frames[1:], duration=FRAME_MS, loop=0, optimize=True)
print(out, f"{os.path.getsize(out) / 1e6:.2f} MB", f"{size[0]}x{size[1]}", f"{len(frames)} frames")
