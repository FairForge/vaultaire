#!/usr/bin/env python3
"""Pixel-art sprite generator for the stored.ge landing page.

Sprites are authored as ASCII grids (or drawn procedurally), then emitted as
SVG <symbol>s: one <path> per colour, each row's horizontal runs merged into
"M x y h w v1 h-w z" rectangles. Output is pasted into landing.html once;
this script is the source of truth if a sprite ever needs editing.
"""
import json
import sys

PAL = {
    '#': 'currentColor',
    'K': '#262626',  # outline / eyes / shoes
    'k': '#4a4a4a',
    'g': '#9e9e9e',
    'W': '#ffffff',
    # wood
    'A': '#6e4127', 'B': '#a8683b', 'C': '#d59352', '9': '#4a2c1a',
    'D': '#f2cf7c', 'E': '#e2b45b',
    # plant
    'G': '#9ccc4a', 'H': '#43ad4b', 'I': '#1f7a45',
    # pot / cool blues
    'J': '#b9e4f7', 'L': '#7fcdf0', 'M': '#4aa8dc',
    # lamp
    'N': '#f7d774', 'O': '#dfb24a', 'P': '#5bc0eb',
    # tape spines
    'Q': '#e8412c', 'R': '#ffd400', 'S': '#27c24c', 'T': '#3b82f6', 'U': '#8e5bd6', 'V': '#ff8a3d',
    # mascot
    'X': '#f6cba4', 'x': '#e0a77c', 'Y': '#6b3f1f', 'y': '#1c3445', 'z': '#2f5c86',
    'p': '#d9a33a', 'q': '#b3832a', 'm': '#c85a54',
    # box
    '1': '#e7bc86', '2': '#cf9356', '3': '#a9713f', '4': '#f3e6c4',
    # heart
    '5': '#ff3b6b', '6': '#c9244f',
    # ground shadow
    'o': '#000000@0.14',
    'h': '#ffffff@0.32',
    'F': '#f7dc98',  # wood highlight
    '8': '#7a4c26',  # cardboard edge
    'a': '#4b5763', 'b': '#6f7f8d', 'c': '#9fb0bd', 'e': '#d5dee5',  # steel
    'f': '#ff6a1a', 'i': '#ffc23a', 'j': '#fff1a8',  # flame
    'r': '#9c4a36', 't': '#b8604a', 'u': '#d9b8a8', 's': '#2b1d18',  # brick, mortar, soot
    'n': '#d8d0c4', 'v': '#b3aa9d',  # stone
    'w': '#f3ead7', '7': '#d6ccba',  # cream, cream shade
    'd': '#000000@0.2',
}


class Grid:
    def __init__(self, w, h):
        self.w, self.h = w, h
        self.px = [['.'] * w for _ in range(h)]

    def rect(self, x, y, w, h, c):
        for yy in range(y, y + h):
            for xx in range(x, x + w):
                if 0 <= xx < self.w and 0 <= yy < self.h:
                    self.px[yy][xx] = c

    def set(self, x, y, c):
        self.rect(x, y, 1, 1, c)

    def blit(self, rows, ox=0, oy=0):
        for dy, row in enumerate(rows):
            for dx, c in enumerate(row):
                if c != '.':
                    self.set(ox + dx, oy + dy, c)


def ascii_grid(rows):
    w = len(rows[0])
    for i, r in enumerate(rows):
        if len(r) != w:
            sys.exit(f'row {i} has width {len(r)}, want {w}: {r!r}')
    g = Grid(w, len(rows))
    g.blit(rows)
    return g


# ---------------------------------------------------------------- mascot
MASCOT = [
    "...YYYYYYYY.....",
    "..YYYYYYYYYY....",
    "..YYYYYYYYYYY...",
    "..YYXXXXXXXYY...",
    "..YXXXXXXXXXY...",
    "..XXKKXXXKKXx...",
    "..XXKKXXXKKXx...",
    "..XXXXXXXXXXx...",
    "..XXXXmmmmXXx...",
    "...XXXXXXXXx....",
    ".....xxxx.......",
    "...########.....",
    "..##########....",
    ".#####hh#####...",
    ".####hWWh####...",
    ".####hWWh####...",
    ".#####hh#####...",
    ".############...",
    ".X##########X...",
    ".x.pppppppp.x...",
    "...pppppppp.....",
    "...pppq.pppq....",
    "...pppq.pppq....",
    "...pppq.pppq....",
    "..KKKK..KKKK....",
    ".ooooooooooooo..",
]


def mascot(wave):
    g = ascii_grid(MASCOT)
    if wave:
        # right arm raised: drop the hand at the hip, draw a sleeve up to a hand
        g.set(12, 18, '#'); g.set(12, 19, '.')
        g.rect(12, 11, 2, 2, '#')
        g.rect(13, 9, 2, 2, '#')
        g.rect(14, 7, 2, 2, '#')
        g.rect(14, 5, 2, 2, 'X')
    return g


PLANT = [
    ".....HG.....",
    "....HGGH....",
    "...IHGGGH...",
    "..IHHGGGHI..",
    ".IIHHGGHHII.",
    ".IHHHGHHHHI.",
    "..IHHHHHHI..",
    "...IIHHII...",
    ".....II.....",
    ".....II.....",
    "..MLLLLLLJ..",
    "..MLLLLLLJ..",
    "..MLLLLLLJ..",
    "..MLLLLLLJ..",
    "..MLLLLLLJ..",
    "..MLLLLLLJ..",
    "..MMLLLLJJ..",
    ".oooooooooo.",
]

LAMP = [
    "..NNNNNN..",
    ".NNNNNNNO.",
    ".NNNNNNNO.",
    ".NNNNNNNO.",
    ".NNNNNNNO.",
    ".NNNNNNNO.",
    "..OOOOOO..",
    "....PP....",
    "....PP....",
    "....PP....",
    "....PP....",
    "..JJJJJJ..",
    ".JJJJJJJJ.",
]

HEART = [
    ".55...55.",
    "5555.5555",
    "555555555",
    "555555556",
    ".5555556.",
    "..55556..",
    "...556...",
    "....6....",
]


CUP = [
    "...g.g..",
    "..g.g...",
    "..kkkk..",
    "kkkkkkkk",
    ".WWWWWg.",
    ".WWWWWg.",
    ".222223.",
    ".224223.",
    ".222223.",
    "..WWWg..",
    "..WWWg..",
    ".oooooo.",
]

MAT = [
    ".####.",
    "#hh###",
    "#h#h##",
    "##hh#d",
    "#####d",
    "#####d",
    "#h###d",
    "#####d",
    "#####d",
    "#####d",
    "#h###d",
    "#####d",
    "#####d",
    "#####d",
    ".####.",
    "oooooo",
]


def rug():
    g = Grid(32, 5)
    g.rect(3, 0, 26, 1, '#')
    g.rect(1, 1, 30, 3, '#')
    g.rect(3, 4, 26, 1, '#')
    for x in range(4, 28, 3):
        g.set(x, 2, 'h')
    g.rect(0, 2, 1, 1, 'h'); g.rect(31, 2, 1, 1, 'h')   # fringe
    return g


def cuboid(g, x, y, w, h, d, top, front, side):
    """Oblique 3D block: front face w*h, depth d going up-right. Bbox (w+d)x(h+d)."""
    for c in range(d):
        g.rect(x + w + c, y + d - 1 - c, 1, h, side)
    for i in range(d):
        g.rect(x + d - i, y + i, w, 1, top)
    g.rect(x, y + d, w, h, front)


def box():
    """1 TB: a moving box, about knee-high next to the mascot."""
    g = Grid(17, 15)
    cuboid(g, 0, 0, 14, 11, 3, '1', '2', '3')
    g.rect(0, 3, 14, 1, '1')                    # lid lip
    for i in range(3):
        g.rect(8 - i, i, 2, 1, '4')             # tape across the lid
    g.rect(6, 3, 2, 4, '4')                     # ...and down the front
    g.rect(2, 6, 3, 1, '8')                     # hand hole
    g.rect(8, 8, 5, 3, 'W'); g.rect(9, 9, 3, 1, 'g')   # label
    g.rect(0, 13, 14, 1, '3')
    g.rect(0, 14, 17, 1, 'o')
    return g


def lockbox():
    g = Grid(13, 11)
    cuboid(g, 0, 0, 10, 7, 3, 'e', 'b', 'a')
    g.rect(1, 4, 8, 5, 'c')
    g.rect(4, 5, 3, 3, 'e'); g.set(5, 6, 'K')   # dial
    g.rect(3, 1, 4, 1, 'K')                     # carry handle
    g.rect(0, 9, 10, 1, 'a')
    g.rect(0, 10, 13, 1, 'o')
    return g


def safe():
    g = Grid(21, 25)
    cuboid(g, 0, 0, 18, 20, 3, 'e', 'b', 'a')
    g.rect(1, 4, 16, 18, 'a')                   # door frame
    g.rect(2, 5, 14, 16, 'c')                   # door
    g.rect(2, 5, 14, 1, 'e')
    g.blit(["..e..", ".eee.", "eeKee", ".eee.", "..e.."], 5, 10)   # dial
    g.set(7, 10, 'K')
    g.rect(12, 10, 1, 5, 'K'); g.rect(11, 12, 3, 1, 'K')           # handle
    g.rect(1, 7, 1, 2, 'K'); g.rect(1, 17, 1, 2, 'K')              # hinges
    g.rect(1, 23, 3, 1, 'K'); g.rect(14, 23, 3, 1, 'K')            # feet
    g.rect(0, 24, 21, 1, 'o')
    return g


def dresser():
    """5 TB: chest of drawers, about chest-high next to the mascot."""
    g = Grid(31, 23)
    cuboid(g, 0, 0, 28, 18, 3, 'F', 'C', 'B')
    for y in (4, 9, 14):
        g.rect(1, y - 1, 26, 1, 'B')            # gap above each drawer
        g.rect(2, y, 24, 4, 'D')
        g.rect(2, y, 24, 1, 'F')
        g.rect(2, y + 3, 24, 1, 'E')
        for kx in (8, 18):
            g.rect(kx, y + 1, 2, 2, 'O'); g.set(kx + 1, y + 2, 'A')
    g.rect(0, 18, 28, 3, 'B'); g.rect(0, 18, 28, 1, 'C'); g.rect(0, 20, 28, 1, 'A')
    g.rect(1, 21, 3, 1, 'A'); g.rect(24, 21, 3, 1, 'A')
    g.rect(0, 22, 31, 1, 'o')
    return g


def bookcase():
    """10 TB: a wide bookcase, taller than the mascot; the biggest piece."""
    g = Grid(31, 44)
    cuboid(g, 0, 0, 28, 40, 3, 'F', 'B', 'A')
    g.rect(2, 5, 24, 35, '9')
    colours = 'QTRSUVLYQMTRUSV'
    k = 0
    for row, plank in enumerate((12, 21, 30, 39)):
        g.rect(1, plank, 26, 1, 'C')
        bottom, x = plank - 1, 2
        if row == 1:                                    # a stack lying flat
            for j, c in enumerate('TQS'):
                g.rect(2, bottom - j, 7 - j, 1, c)
            x = 10
        while x < 26:
            c = colours[k % len(colours)]; k += 1
            bw = 1 if k % 3 == 0 else 2
            bh = 5 + (k * 7 + row) % 3
            if x + bw > 26:
                break
            if (k + row) % 13 == 3:                     # gap / small object
                if row == 0 and x < 22:
                    g.rect(x, bottom - 1, 2, 2, 'M'); g.rect(x, bottom - 3, 2, 2, 'H')
                x += 3; continue
            g.rect(x, bottom - bh + 1, bw, bh, c)
            g.set(x, bottom - bh + 3, 'w')              # spine label
            x += bw
    g.rect(0, 41, 28, 1, 'A')
    g.rect(1, 42, 3, 1, 'A'); g.rect(24, 42, 3, 1, 'A')
    g.rect(0, 43, 31, 1, 'o')
    return g


def fireplace(flame):
    g = Grid(27, 24)
    cuboid(g, 0, 0, 24, 20, 3, 'n', 'n', 'v')
    g.rect(0, 3, 24, 2, 'B'); g.rect(0, 3, 24, 1, 'F')          # mantel
    for c in range(3):
        g.rect(24 + c, 2 - c, 1, 2, 'A')
    g.rect(3, 6, 18, 14, 'r')                                    # brick
    for yy in range(6, 20, 3):
        g.rect(3, yy, 18, 1, 'u')
        for xx in range(4 + (yy // 3) % 2 * 2, 21, 4):
            g.rect(xx, yy + 1, 1, 2, 'u')
    g.rect(6, 10, 12, 10, 's'); g.set(6, 10, 'r'); g.set(17, 10, 'r')   # firebox
    g.blit(flame, 7, 12)
    g.rect(8, 18, 8, 1, 'Y'); g.rect(9, 19, 6, 1, 'A')          # logs
    g.rect(0, 20, 24, 3, 'n'); g.rect(0, 22, 24, 1, 'v')         # hearth
    g.rect(0, 23, 27, 1, 'o')
    return g


def plant():
    g = Grid(14, 23)
    leaves = ((4, 6, 3.3, 2.5), (10, 4, 3.4, 2.6), (7, 2.4, 2.5, 2.2), (3.6, 11, 3.1, 2.2), (10.5, 10, 3.1, 2.3))
    for cx, cy, rx, ry in leaves:                     # stems first
        steps = 12
        for t in range(steps + 1):
            px = round(7 + (cx - 7) * t / steps); py = round(15 + (cy - 15) * t / steps)
            g.set(px, py, 'I')
    for cx, cy, rx, ry in leaves:
        for yy in range(23):
            for xx in range(14):
                dx, dy = (xx - cx) / rx, (yy - cy) / ry
                if dx * dx + dy * dy <= 1:
                    tone = dx + dy
                    g.set(xx, yy, 'G' if tone < -0.45 else ('I' if tone > 0.75 else 'H'))
        g.set(round(cx), round(cy), 'I')
    g.rect(2, 15, 10, 1, 'W'); g.rect(2, 16, 10, 1, '7')           # rim
    g.rect(3, 17, 8, 4, 'W'); g.rect(10, 17, 1, 4, '7'); g.rect(3, 17, 1, 4, 'w')
    g.rect(4, 21, 6, 1, '7')
    g.rect(1, 22, 12, 1, 'o')
    return g


def frame():
    g = Grid(16, 12)
    g.rect(0, 0, 16, 12, 'A'); g.rect(0, 0, 16, 1, 'B')
    g.rect(1, 1, 14, 10, 'w')
    g.rect(2, 2, 12, 8, 'J')
    g.rect(10, 3, 2, 2, 'R')
    for yy in range(4, 10):
        r = yy - 4
        g.rect(max(2, 5 - r), yy, min(13, 5 + r) - max(2, 5 - r) + 1, 1, 'H')
        if yy >= 6:
            r2 = yy - 6
            g.rect(max(2, 10 - r2), yy, min(13, 10 + r2) - max(2, 10 - r2) + 1, 1, 'I')
    g.rect(5, 4, 1, 1, 'W'); g.rect(4, 5, 3, 1, 'W')
    return g


PHONE = [
    ".KKKKKKK.",
    "KKKgggKKK",
    "KJJJJJJJK",
    "KJJJJJWJK",
    "KJJJJWJJK",
    "KJJJWJJJK",
    "KJJWWJJJK",
    "KJJJWJJJK",
    "KJJJJWJJK",
    "KJJJJJJJK",
    "KJQQQQQJK",
    "KJQQQQQJK",
    "KJJJJJJJK",
    "KKKKKKKKK",
    "KKKgKKKKK",
    ".KKKKKKK.",
]

LAPTOP = [
    "..KKKKKKKKKKKKKKKKKK..",
    "..KJJJJJJJJJJJJJJJJK..",
    "..KJJJJJJJJJJJJJJJJK..",
    "..KJggggggggggggggJK..",
    "..KJgWWWWWWWWWWWWgJK..",
    "..KJgQQQQQQQQQQQWgJK..",
    "..KJgWWWWWWWWWWWWgJK..",
    "..KJggggggggggggggJK..",
    "..KJJJJJJJJJJJJJJJJK..",
    "..KKKKKKKKKKKKKKKKKK..",
    "KKKKKKKKKKKKKKKKKKKKKK",
    "kkkkkkkkkkkkkkkkkkkkkk",
    ".oooooooooooooooooooo.",
]


def floorlamp():
    g = Grid(10, 44)
    g.rect(2, 0, 6, 1, 'N')
    g.rect(1, 1, 8, 6, 'N')
    g.rect(8, 1, 1, 6, 'O')
    g.rect(1, 7, 8, 1, 'O')
    g.rect(4, 8, 1, 33, 'g')
    g.rect(5, 8, 1, 33, 'k')
    g.rect(2, 41, 6, 2, 'k')
    g.rect(0, 43, 10, 1, 'o')
    return g


# ------------------------------------------------------------- lettering
FONT = {
    'S': ["####", "#...", "####", "...#", "####"],
    'T': ["####", ".##.", ".##.", ".##.", ".##."],
    'O': ["####", "#..#", "#..#", "#..#", "####"],
    'R': ["####", "#..#", "####", "#.#.", "#..#"],
    'E': ["####", "#...", "###.", "#...", "####"],
    'D': ["###.", "#..#", "#..#", "#..#", "###."],
    'G': ["####", "#...", "#.##", "#..#", "####"],
    '.': [".", ".", ".", ".", "#"],
    '_': ["...", "...", "...", "...", "###"],
}


def text(g, s, x, y):
    for ch in s:
        glyph = FONT[ch]
        g.blit(glyph, x, y)
        x += len(glyph[0]) + 1
    return x - 1  # right edge (exclusive of trailing gap)


def logo():
    # Square frame broken at the lower-left by the wordmark: STORED sits
    # inside, _.GE rides the bottom edge and the frame resumes after it.
    g = Grid(34, 30)
    g.rect(0, 0, 34, 1, '#')
    g.rect(33, 0, 1, 30, '#')
    g.rect(0, 0, 1, 14, '#')
    text(g, 'STORED', 0, 18)
    end = text(g, '_.GE', 0, 25)
    g.rect(end + 2, 29, 34 - end - 2, 1, '#')
    return g


def mark():
    g = Grid(14, 14)
    g.rect(0, 0, 14, 1, '#')
    g.rect(13, 0, 1, 14, '#')
    g.rect(0, 0, 1, 6, '#')
    text(g, 'S', 0, 9)
    g.rect(5, 13, 9, 1, '#')
    return g


# ----------------------------------------------------------------- output
def color_attr(c):
    col = PAL[c]
    if '@' in col:
        hexc, op = col.split('@')
        return f'fill="{hexc}" fill-opacity="{op}"'
    return f'fill="{col}"'


def paths(g):
    by = {}
    for y, row in enumerate(g.px):
        x = 0
        while x < g.w:
            c = row[x]
            if c == '.':
                x += 1; continue
            x0 = x
            while x < g.w and row[x] == c:
                x += 1
            by.setdefault(c, []).append(f'M{x0} {y}h{x - x0}v1h-{x - x0}z')
    order = sorted(by, key=lambda c: (c != 'o', c))  # shadow first
    return ''.join(f'<path {color_attr(c)} d="{"".join(by[c])}"/>' for c in order)


def symbol(name, g):
    return f'<symbol id="s-{name}" viewBox="0 0 {g.w} {g.h}">{paths(g)}</symbol>'


SPRITES = {
    'mascot': mascot(False),
    'mascot-wave': mascot(True),
    'plant': plant(),
    'lamp': ascii_grid(LAMP),
    'heart': ascii_grid(HEART),
    'cup': ascii_grid(CUP),
    'mat': ascii_grid(MAT),
    'rug': rug(),
    'frame': frame(),
    'phone': ascii_grid(PHONE),
    'laptop': ascii_grid(LAPTOP),
    'floorlamp': floorlamp(),
    'box': box(),
    'dresser': dresser(),
    'bookcase': bookcase(),
    'logo': logo(),
    'mark': mark(),
}

if __name__ == '__main__':
    out = sys.argv[1] if len(sys.argv) > 1 else 'sprites'
    defs = ''.join(symbol(n, g) for n, g in SPRITES.items())
    block = ('<svg class="sprite-defs" width="0" height="0" aria-hidden="true" focusable="false">'
             f'<defs>{defs}</defs></svg>')
    open(f'{out}.html', 'w').write(block)
    json.dump({n: [g.w, g.h] for n, g in SPRITES.items()}, open(f'{out}.json', 'w'))
    # favicon: white mark on charcoal
    m = SPRITES['mark']
    fav = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="-4 -4 22 22" shape-rendering="crispEdges">'
           f'<rect x="-4" y="-4" width="22" height="22" fill="#333"/>'
           f'{paths(m).replace("currentColor", "#fff")}</svg>')
    open(f'{out}-favicon.svg', 'w').write(fav)
    # preview sheet for eyeballing
    uses, x = [], 2
    for n, g in SPRITES.items():
        uses.append(f'<svg x="{x}" y="2" width="{g.w}" height="{g.h}" viewBox="0 0 {g.w} {g.h}" color="#333">{paths(g)}</svg>')
        x += g.w + 3
    prev = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {x} 48" width="{x*6}" height="{48*6}" '
            f'shape-rendering="crispEdges"><rect width="100%" height="100%" fill="#f2f2f2"/>{"".join(uses)}</svg>')
    open(f'{out}-preview.svg', 'w').write(prev)
    print(f'{len(block)} bytes of defs;', {n: (g.w, g.h) for n, g in SPRITES.items()})
