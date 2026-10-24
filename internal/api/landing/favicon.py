#!/usr/bin/env python3
"""favicon.ico for the web host (Stage 1a Prompt 40 Part 0, RP56-3).

The landing's icon is a data: SVG in landing.src.html (a 22 x 22 grid: a
filled background rect and white 1-unit rects, crispEdges). Browsers read it
from the page; crawlers and link unfurlers ask for /favicon.ico, which used
to be the S3 API's 403 XML. This renders that same SVG — read from
landing.src.html, never retyped — at whole-pixel scales (22, 44, 88 px; the
grid is pixel art, so only integer scales stay crisp) and writes an ICO of
PNG images to internal/api/favicon.ico. Stdlib only. Run:

    python3 internal/api/landing/favicon.py

favicon_test.go decodes the file and re-derives every pixel from the SVG:
an edit of the icon without a re-run fails the test.
"""
import os
import re
import struct
import sys
import urllib.parse
import zlib

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, "landing.src.html")
OUT = os.path.join(HERE, "..", "favicon.ico")
SCALES = (1, 2, 4)


def icon_svg(html):
    m = re.search(r'<link rel="icon" href="data:image/svg\+xml,([^"]*)"', html)
    if not m:
        sys.exit("favicon.py: no <link rel=\"icon\" href=\"data:image/svg+xml,…\"> in landing.src.html")
    return urllib.parse.unquote(m.group(1))


def grid(svg):
    """The SVG as rows of RGBA tuples: (size, pixels)."""
    vb = re.search(r'viewBox="(-?\d+) (-?\d+) (\d+) (\d+)"', svg)
    ox, oy, w, h = (int(v) for v in vb.groups())
    if w != h:
        sys.exit("favicon.py: the icon is not square")
    px = [[(0, 0, 0, 0)] * w for _ in range(h)]

    def rgb(c):
        c = c.lstrip("#")
        if len(c) == 3:
            c = "".join(ch * 2 for ch in c)
        return tuple(int(c[i:i + 2], 16) for i in (0, 2, 4)) + (255,)

    def fill(x, y, rw, rh, col):
        for yy in range(y - oy, y - oy + rh):
            for xx in range(x - ox, x - ox + rw):
                px[yy][xx] = col

    for x, y, rw, rh, col in re.findall(r'<rect x="(-?\d+)" y="(-?\d+)" width="(\d+)" height="(\d+)" fill="(#[0-9a-fA-F]+)"', svg):
        fill(int(x), int(y), int(rw), int(rh), rgb(col))
    for col, d in re.findall(r'<path fill="(#[0-9a-fA-F]+)" d="([^"]*)"', svg):
        # the sprite generator's shape: M x y h w v1 h-w z, one run per rect
        for x, y, rw, rh in re.findall(r'M(-?\d+) (-?\d+)h(\d+)v(\d+)h-\d+z', d):
            fill(int(x), int(y), int(rw), int(rh), rgb(col))
    return w, px


def png(size, scale, px):
    n = size * scale
    raw = bytearray()
    for y in range(n):
        raw.append(0)  # filter: none
        for x in range(n):
            raw.extend(px[y // scale][x // scale])

    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)

    ihdr = struct.pack(">IIBBBBB", n, n, 8, 6, 0, 0, 0)  # 8-bit RGBA
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) + chunk(b"IDAT", zlib.compress(bytes(raw), 9)) + chunk(b"IEND", b"")


def ico(images):
    head = struct.pack("<HHH", 0, 1, len(images))
    off = 6 + 16 * len(images)
    dirs, body = b"", b""
    for n, data in images:
        dirs += struct.pack("<BBBBHHII", n % 256, n % 256, 0, 0, 1, 32, len(data), off + len(body))
        body += data
    return head + dirs + body


def main():
    with open(SRC, encoding="utf-8") as f:
        size, px = grid(icon_svg(f.read()))
    out = ico([(size * s, png(size, s, px)) for s in SCALES])
    with open(OUT, "wb") as f:
        f.write(out)
    print("favicon.py: wrote %s (%d bytes; %s px)" % (os.path.relpath(OUT), len(out), ", ".join(str(size * s) for s in SCALES)))


if __name__ == "__main__":
    main()
