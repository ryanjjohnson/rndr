#!/usr/bin/env python3
"""Regenerates the README animation: a model spinning while rndr cycles through its modes and styles.

    python3 docs/make-demo.py MODEL [OUT.gif]

Needs ./rndr (sh build.sh), Pillow, and a monospace TrueType font (Menlo or DejaVu Sans Mono).
Braille and half blocks are drawn as shapes rather than from the font, the way most terminals do.
"""
import os
import re
import subprocess
import sys

from PIL import Image, ImageDraw, ImageFont

COLS, ROWS = 72, 36
TILT, ZOOM = 15, 1.0
STEP = 6                              # degrees per frame
SCENES = [("both", "ascii"), ("wire", "ascii"), ("wire", "braille"),
          ("both", "braille"), ("shade", "blocks"), ("both", "blocks")]
FRAMES_PER_SCENE = 20                 # 6 scenes x 20 frames x 6 degrees = two full turns
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

TOKEN = re.compile(r"\x1b\[([0-9;]*)m|(.)")
DOTS = [(0, 0, 0x01), (0, 1, 0x02), (0, 2, 0x04), (0, 3, 0x40), (1, 0, 0x08), (1, 1, 0x10), (1, 2, 0x20), (1, 3, 0x80)]


def palette_grey(n):
    """The greys rndr uses from the 256-colour palette."""
    g = {16: 0, 231: 255}.get(n, 8 + 10 * (n - 232))
    return (g, g, g)


def draw_line(d, row, line):
    """Draws one line of rndr output: ASCII, braille, or half blocks with grey colour codes."""
    fg = bg = None
    col = 0
    y0, y1 = pad + row * lh, pad + (row + 1) * lh
    for m in TOKEN.finditer(line):
        if m.group(2) is None:
            p = [int(x) for x in m.group(1).split(";")]
            if p == [0]:
                fg = bg = None
            elif p == [39]:
                fg = None
            elif p == [49]:
                bg = None
            elif p[:2] == [38, 5]:
                fg = palette_grey(p[2])
            elif p[:2] == [48, 5]:
                bg = palette_grey(p[2])
            continue
        ch = m.group(2)
        x0, x1 = round(pad + col * cw), round(pad + (col + 1) * cw)
        ink = fg or FG
        mid = (y0 + y1) // 2
        if bg:
            d.rectangle([x0, y0, x1 - 1, y1 - 1], fill=bg)
        if ch == "▀":
            d.rectangle([x0, y0, x1 - 1, mid - 1], fill=ink)
        elif ch == "▄":
            d.rectangle([x0, mid, x1 - 1, y1 - 1], fill=ink)
        elif ch == "█":
            d.rectangle([x0, y0, x1 - 1, y1 - 1], fill=ink)
        elif 0x2800 <= ord(ch) <= 0x28FF:
            for bx, by, bit in DOTS:
                if (ord(ch) - 0x2800) & bit:
                    cx, cy = x0 + cw * (0.27 + 0.46 * bx), y0 + lh * (0.125 + 0.25 * by)
                    d.ellipse([cx - 1.25, cy - 1.25, cx + 1.25, cy + 1.25], fill=ink)
        elif ch != " ":
            d.text((x0, y0), ch, font=font, fill=ink)
        col += 1


frames = []
for i in range(len(SCENES) * FRAMES_PER_SCENE):
    mode, style = SCENES[i // FRAMES_PER_SCENE]
    spin = (-30 + i * STEP + 180) % 360 - 180
    view = [str(TILT), str(spin), "0", str(ZOOM)]
    env = {k: v for k, v in os.environ.items() if k != "COLORTERM"}  # the 26 greys every terminal has
    text = subprocess.run([rndr, model, *view, "-m", mode, "-c", style, "-s", f"{COLS}x{ROWS}"],
                          check=True, capture_output=True, text=True, env=env).stdout
    img = Image.new("RGB", size, BG)
    d = ImageDraw.Draw(img)
    for row, line in enumerate(text.split("\n")):
        draw_line(d, row, line)
    chars = "" if style == "ascii" else f" -c {style}"
    d.text((pad, pad + (ROWS + 1) * lh), f"$ rndr goose.stl {' '.join(view)} -m {mode}{chars}", font=font, fill=DIM)
    frames.append(img)

# one shared palette keeps the file small and stops colours flickering between frames;
# it is taken from one frame of every scene so the greys of the blocks scenes are in it
sample = Image.new("RGB", (size[0], size[1] * len(SCENES)))
for k in range(len(SCENES)):
    sample.paste(frames[k * FRAMES_PER_SCENE], (0, k * size[1]))
palette = sample.quantize(colors=48, dither=Image.Dither.NONE)
frames = [f.quantize(palette=palette, dither=Image.Dither.NONE) for f in frames]
frames[0].save(out, save_all=True, append_images=frames[1:], duration=FRAME_MS, loop=0, optimize=True)
print(out, f"{os.path.getsize(out) / 1e6:.2f} MB", f"{size[0]}x{size[1]}", f"{len(frames)} frames")
