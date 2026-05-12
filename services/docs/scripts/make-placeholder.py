#!/usr/bin/env python3
"""Generate a labeled placeholder PNG using only the Python standard library.

Used to scaffold screenshot placeholders for the docs site. Each file is
written with its final filename so a real screenshot can drop in without
touching markdown or sidebars.

Usage:
    python3 make-placeholder.py OUT.png "TITLE LINE" "subtitle line"

Output: a 1200x800 PNG with the title and subtitle drawn in a 5x7 bitmap
font. Background tint is derived from the filename so different files end
up visually distinct.
"""

from __future__ import annotations

import struct
import sys
import zlib
from hashlib import md5
from pathlib import Path

# 5x7 bitmap font — uppercase A-Z, digits, and a few punctuation marks. Each
# glyph is 7 rows of 5 columns (`#` = pixel on, ` ` = off). Enough to render
# filenames and short descriptions for placeholder use.
FONT: dict[str, list[str]] = {
    "A": [" ### ", "#   #", "#   #", "#####", "#   #", "#   #", "#   #"],
    "B": ["#### ", "#   #", "#   #", "#### ", "#   #", "#   #", "#### "],
    "C": [" ####", "#    ", "#    ", "#    ", "#    ", "#    ", " ####"],
    "D": ["#### ", "#   #", "#   #", "#   #", "#   #", "#   #", "#### "],
    "E": ["#####", "#    ", "#    ", "#### ", "#    ", "#    ", "#####"],
    "F": ["#####", "#    ", "#    ", "#### ", "#    ", "#    ", "#    "],
    "G": [" ####", "#    ", "#    ", "#  ##", "#   #", "#   #", " ### "],
    "H": ["#   #", "#   #", "#   #", "#####", "#   #", "#   #", "#   #"],
    "I": [" ### ", "  #  ", "  #  ", "  #  ", "  #  ", "  #  ", " ### "],
    "J": ["  ###", "    #", "    #", "    #", "    #", "#   #", " ### "],
    "K": ["#   #", "#  # ", "# #  ", "##   ", "# #  ", "#  # ", "#   #"],
    "L": ["#    ", "#    ", "#    ", "#    ", "#    ", "#    ", "#####"],
    "M": ["#   #", "## ##", "# # #", "# # #", "#   #", "#   #", "#   #"],
    "N": ["#   #", "##  #", "# # #", "# # #", "# # #", "#  ##", "#   #"],
    "O": [" ### ", "#   #", "#   #", "#   #", "#   #", "#   #", " ### "],
    "P": ["#### ", "#   #", "#   #", "#### ", "#    ", "#    ", "#    "],
    "Q": [" ### ", "#   #", "#   #", "#   #", "# # #", "#  # ", " ## #"],
    "R": ["#### ", "#   #", "#   #", "#### ", "# #  ", "#  # ", "#   #"],
    "S": [" ####", "#    ", "#    ", " ### ", "    #", "    #", "#### "],
    "T": ["#####", "  #  ", "  #  ", "  #  ", "  #  ", "  #  ", "  #  "],
    "U": ["#   #", "#   #", "#   #", "#   #", "#   #", "#   #", " ### "],
    "V": ["#   #", "#   #", "#   #", "#   #", "#   #", " # # ", "  #  "],
    "W": ["#   #", "#   #", "#   #", "# # #", "# # #", "## ##", "#   #"],
    "X": ["#   #", "#   #", " # # ", "  #  ", " # # ", "#   #", "#   #"],
    "Y": ["#   #", "#   #", " # # ", "  #  ", "  #  ", "  #  ", "  #  "],
    "Z": ["#####", "    #", "   # ", "  #  ", " #   ", "#    ", "#####"],
    "0": [" ### ", "#   #", "#  ##", "# # #", "##  #", "#   #", " ### "],
    "1": ["  #  ", " ##  ", "  #  ", "  #  ", "  #  ", "  #  ", " ### "],
    "2": [" ### ", "#   #", "    #", "   # ", "  #  ", " #   ", "#####"],
    "3": [" ### ", "#   #", "    #", "  ## ", "    #", "#   #", " ### "],
    "4": ["   # ", "  ## ", " # # ", "#  # ", "#####", "   # ", "   # "],
    "5": ["#####", "#    ", "#    ", "#### ", "    #", "#   #", " ### "],
    "6": [" ### ", "#    ", "#    ", "#### ", "#   #", "#   #", " ### "],
    "7": ["#####", "    #", "   # ", "  #  ", " #   ", " #   ", " #   "],
    "8": [" ### ", "#   #", "#   #", " ### ", "#   #", "#   #", " ### "],
    "9": [" ### ", "#   #", "#   #", " ####", "    #", "    #", " ### "],
    "-": ["     ", "     ", "     ", "#####", "     ", "     ", "     "],
    "_": ["     ", "     ", "     ", "     ", "     ", "     ", "#####"],
    ".": ["     ", "     ", "     ", "     ", "     ", "  #  ", "  #  "],
    "/": ["    #", "    #", "   # ", "  #  ", " #   ", "#    ", "#    "],
    ",": ["     ", "     ", "     ", "     ", "  #  ", "  #  ", " #   "],
    "(": ["   # ", "  #  ", " #   ", " #   ", " #   ", "  #  ", "   # "],
    ")": [" #   ", "  #  ", "   # ", "   # ", "   # ", "  #  ", " #   "],
    ":": ["     ", "  #  ", "  #  ", "     ", "  #  ", "  #  ", "     "],
    " ": ["     ", "     ", "     ", "     ", "     ", "     ", "     "],
}

GLYPH_W = 5
GLYPH_H = 7


def derive_bg(filename: str) -> tuple[int, int, int]:
    """Pick a deterministic dark background color from the filename."""
    digest = md5(filename.encode("utf-8")).digest()
    # Bias toward the dark end so text stays legible.
    r = 12 + (digest[0] % 32)
    g = 14 + (digest[1] % 32)
    b = 24 + (digest[2] % 48)
    return r, g, b


def text_width(text: str, scale: int) -> int:
    return len(text) * (GLYPH_W + 1) * scale - scale


def draw_text(
    pixels: list[list[tuple[int, int, int]]],
    text: str,
    x: int,
    y: int,
    color: tuple[int, int, int],
    scale: int,
) -> None:
    text = text.upper()
    cx = x
    for ch in text:
        glyph = FONT.get(ch, FONT[" "])
        for gy, row in enumerate(glyph):
            for gx, cell in enumerate(row):
                if cell != "#":
                    continue
                for dy in range(scale):
                    for dx in range(scale):
                        px = cx + gx * scale + dx
                        py = y + gy * scale + dy
                        if 0 <= py < len(pixels) and 0 <= px < len(pixels[0]):
                            pixels[py][px] = color
        cx += (GLYPH_W + 1) * scale


def encode_png(width: int, height: int, pixels: list[list[tuple[int, int, int]]]) -> bytes:
    raw = bytearray()
    for row in pixels:
        raw.append(0)  # filter: None
        for r, g, b in row:
            raw += bytes((r, g, b))

    def chunk(tag: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data))
            + tag
            + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    sig = b"\x89PNG\r\n\x1a\n"
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    idat = zlib.compress(bytes(raw), 9)
    return sig + chunk(b"IHDR", ihdr) + chunk(b"IDAT", idat) + chunk(b"IEND", b"")


def make_placeholder(out_path: Path, title: str, subtitle: str) -> None:
    width, height = 1200, 800
    bg = derive_bg(out_path.name)
    fg = (243, 245, 250)  # near-white text
    accent = (75, 157, 248)  # brand blue (--ifm-color-primary)

    pixels = [[bg for _ in range(width)] for _ in range(height)]

    # Outer brand-color border so the placeholder reads as "deliberate", not broken.
    border = 8
    for y in range(height):
        for x in range(width):
            if y < border or y >= height - border or x < border or x >= width - border:
                pixels[y][x] = accent

    # Centered title
    title_scale = 8
    title_w = text_width(title.upper(), title_scale)
    tx = (width - title_w) // 2
    ty = height // 2 - GLYPH_H * title_scale - 20
    draw_text(pixels, title, tx, ty, fg, title_scale)

    # Centered subtitle
    sub_scale = 4
    sub_w = text_width(subtitle.upper(), sub_scale)
    sx = (width - sub_w) // 2
    sy = height // 2 + 40
    draw_text(pixels, subtitle, sx, sy, accent, sub_scale)

    # Footer hint
    hint = "PLACEHOLDER  REPLACE WITH  " + out_path.name.upper()
    hint_scale = 3
    hint_w = text_width(hint, hint_scale)
    hx = (width - hint_w) // 2
    hy = height - 80
    draw_text(pixels, hint, hx, hy, fg, hint_scale)

    out_path.write_bytes(encode_png(width, height, pixels))


def main() -> int:
    if len(sys.argv) != 4:
        print(__doc__)
        return 2
    out_path = Path(sys.argv[1])
    title = sys.argv[2]
    subtitle = sys.argv[3]
    out_path.parent.mkdir(parents=True, exist_ok=True)
    make_placeholder(out_path, title, subtitle)
    print(f"wrote {out_path} ({out_path.stat().st_size} bytes)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
