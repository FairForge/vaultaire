#!/usr/bin/env python3
"""Build internal/api/landing.html from the sources in this directory.

    make landing            (or: python3 internal/api/landing/build.py)

Inputs (all in this directory):
  landing.src.html   page markup with __CSS__ / __SPRITES__ / __BUILDER__ /
                     __SCRIPT__ tokens and __PRICE_*__ price tokens
  landing.css        stylesheet; @font-face blobs are __MONT__/__SILK4__/__SILK7__,
                     lines marked /*NIGHT*/ are expanded into the system-dark and
                     data-theme="dark" selector forms
  sprites.py         pixel-art generator (one <symbol> per sprite)
  builder.html       the house builder section (tray / swatches / sprites are
                     generated here from PIECES and prices.json)
  page.js            the page's own interactions (waitlist, tabs, sliders)
  builder.js         the house builder, why strip and light/dark toggle
  prices.json        every stored.ge price the page shows, in one place
  fonts/*.woff2      Montserrat (variable) + Silkscreen 400/700, OFL, latin subset

The output carries a sha256 of these sources in its first comment;
internal/api/landing_build_test.go fails when they drift, so the generated
file can never be edited by hand without the test noticing.

The page is parsed by Go's html/template afterwards, so the script must not
use template literals and nothing here may emit "{{" or "}}".
"""
import base64
import hashlib
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(os.path.dirname(HERE), 'landing.html')
# the dashboard's house (Phase 2): sprite defs, room background and CSS as
# html/template definitions, stamped with the same sources hash
DASH_OUT = os.path.join(os.path.dirname(os.path.dirname(HERE)), 'dashboard', 'templates', 'generated', 'house.html')
sys.path.insert(0, HERE)
import sprites  # noqa: E402  (module import generates the sprite table)


def read(name):
    with open(os.path.join(HERE, name), encoding='utf-8') as f:
        return f.read()


def b64(name):
    with open(os.path.join(HERE, 'fonts', name), 'rb') as f:
        return base64.b64encode(f.read()).decode()


def sub(text, old, new, count=1):
    n = text.count(old)
    if n != count:
        sys.exit(f'build.py: expected {count} match(es), found {n}: {old[:80]!r}')
    return text.replace(old, new)


def money(n):
    return f'${n:,.2f}'


# ---- prices: one file, every token -------------------------------------
P = json.loads(read('prices.json'))
PRICE_TOKENS = {
    '__PRICE_STD__': f"{P['standard']['annual']:.2f}",        # 4.49
    '__PRICE_STD_MONTHLY__': f"{P['standard']['monthly']:.2f}",
    '__PRICE_VAULT__': f"{P['vault']['annual']:g}",           # 2
    '__PRICE_VAULT_2DP__': f"{P['vault']['annual']:.2f}",     # 2.00
    '__PRICE_VAULT_MONTHLY__': f"{P['vault']['monthly']:.2f}",
    '__PRICE_PIN_HOT__': f"{P['pin_hot']:g}",                 # 3
    '__PRICE_PERF__': f"{P['performance']:.2f}",
    '__PRICE_AWS__': f"{P['competitors']['aws_s3']:.2f}",
    '__PRICE_B2__': f"{P['competitors']['backblaze_b2']:.2f}",
    '__PRICE_WASABI__': f"{P['competitors']['wasabi']:.2f}",
    '__PRICE_R2__': f"{P['competitors']['cloudflare_r2']:.2f}",
    # derived figures the copy quotes
    '__PRICE_VAULT_5TB__': f"{5 * P['vault']['annual']:g}",
    '__PRICE_VAULT_MIN__': f"{P['vault']['monthly_minimum']:.2f}",
    '__PRICE_FOUNDERS__': f"{P['founders']:.2f}",
    '__PRICE_STD_20TB__': money(20 * P['standard']['annual']),
    '__PRICE_B2_20TB__': money(20 * P['competitors']['backblaze_b2']),
    '__PRICE_AWS_20TB__': money(20 * P['competitors']['aws_s3']),
    '__PRICE_AWS_SAVED_20TB__': f"${round((P['competitors']['aws_s3'] - P['standard']['annual']) * 20 * 12):,}",
    '__PRICE_AWS_MULT__': f"{P['competitors']['aws_s3'] / P['standard']['annual']:.0f}",
    # Egress allowances are data, not a digit borrowed from the AWS multiple (R14-09).
    '__EGRESS_STD__': f"{P['egress']['standard_free_ratio']:g}",
    '__EGRESS_VAULT__': f"{P['egress']['vault_restore_free_ratio']:g}",
    '__PRICES_ASOF__': P['competitors']['_asof'],
}


def prices(text):
    for k, v in PRICE_TOKENS.items():
        text = text.replace(k, v)
    if '__PRICE_' in text:
        sys.exit('build.py: unknown price token: ' + re.search(r'__PRICE_[A-Z0-9_]+__', text).group(0))
    return text


# ---- sprites --------------------------------------------------------------
def spr(name, w, h, u=None, cls='px'):
    style = f'--w:{w};--h:{h}' + (f';--u:{u}px' if u else '')
    return (f'<svg class="{cls}" style="{style}" viewBox="0 0 {w} {h}" aria-hidden="true" focusable="false">'
            f'<use href="#s-{name}"/></svg>')


NIGHT_DEFS = (
    '<radialGradient id="lampglow"><stop offset="0" stop-color="#ffe7a3" stop-opacity="0.8"/>'
    '<stop offset="0.45" stop-color="#ffc861" stop-opacity="0.3"/><stop offset="1" stop-color="#ffc861" stop-opacity="0"/></radialGradient>'
    # the ambient pool: wide, soft, warm; drawn with screen blending so it lights furniture
    '<radialGradient id="lampambient"><stop offset="0" stop-color="#ffd9a0" stop-opacity="0.7"/>'
    '<stop offset="0.35" stop-color="#ffc072" stop-opacity="0.4"/><stop offset="0.7" stop-color="#ffa64a" stop-opacity="0.14"/>'
    '<stop offset="1" stop-color="#ff9e3d" stop-opacity="0"/></radialGradient>'
    # the room fill: very wide and faint, so the whole floor the lamp is on feels warm
    '<radialGradient id="lampfill"><stop offset="0" stop-color="#ffc98a" stop-opacity="0.22"/>'
    '<stop offset="1" stop-color="#ffb060" stop-opacity="0"/></radialGradient>'
    # cast shadows (blurred ellipses) and the night vignette
    '<filter id="soft" x="-60%" y="-300%" width="220%" height="700%"><feGaussianBlur stdDeviation="1.3"/></filter>'
    '<radialGradient id="vig" cx="0.5" cy="0.45" r="0.72"><stop offset="0.5" stop-color="#000" stop-opacity="0"/>'
    '<stop offset="1" stop-color="#000" stop-opacity="0.5"/></radialGradient>'
    # night tints by distance from a lamp: near = warm, mid = half, far = the cool dim
    '<filter id="nightwarm" color-interpolation-filters="sRGB"><feComponentTransfer>'
    '<feFuncR type="linear" slope="0.98"/><feFuncG type="linear" slope="0.84"/><feFuncB type="linear" slope="0.62"/>'
    '</feComponentTransfer></filter>'
    '<filter id="nightmid" color-interpolation-filters="sRGB"><feComponentTransfer>'
    '<feFuncR type="linear" slope="0.74"/><feFuncG type="linear" slope="0.68"/><feFuncB type="linear" slope="0.64"/>'
    '</feComponentTransfer></filter>'
    '<filter id="nightdim" color-interpolation-filters="sRGB"><feComponentTransfer>'
    '<feFuncR type="linear" slope="0.5"/><feFuncG type="linear" slope="0.52"/><feFuncB type="linear" slope="0.66"/>'
    '</feComponentTransfer></filter>'
)


def sprite_defs():
    defs = ''.join(sprites.symbol(n, g) for n, g in sprites.SPRITES.items())
    return ('<svg class="sprite-defs" width="0" height="0" aria-hidden="true" focusable="false">'
            f'<defs>{NIGHT_DEFS}{defs}</defs></svg>')


# ---- css ------------------------------------------------------------------
def night(m):
    sels, decl = m.group(1).strip(), m.group(2).strip()
    parts = [x.strip() for x in sels.split(',')]
    sys_ = ', '.join(f':root:not([data-theme="light"]) {x}' for x in parts)
    tog = ', '.join(f':root[data-theme="dark"] {x}' for x in parts)
    return (f'    @media (prefers-color-scheme: dark) {{ {sys_} {{ {decl} }} }}\n'
            f'    {tog} {{ {decl} }}')


def css():
    c = read('landing.css')
    c = re.sub(r'^    /\*NIGHT\*/ ([^{]+)\{(.*)\}$', night, c, flags=re.M)
    c = c.replace('__MONT__', b64('montserrat.woff2'))
    c = c.replace('__SILK4__', b64('silk400.woff2'))
    c = c.replace('__SILK7__', b64('silk700.woff2'))
    c = c.strip('\n')
    if c.startswith('<style>'):
        c = c[len('<style>'):].strip('\n')
    if c.endswith('</style>'):
        c = c[:-len('</style>')].rstrip('\n')
    return c


# ---- the builder section --------------------------------------------------
# key: (w, h, name, what, TB) -- prices come from prices.json
PIECES = {
    'box': (17, 15, 'Storage box', '1 TB', 1),
    'dresser': (31, 23, 'Dresser', '5 TB', 5),
    'bookcase': (31, 44, 'Bookcase', '10 TB', 10),
    'plant': (14, 23, 'Plant', 'good vibes', 0),
    'lamp': (10, 13, 'Lamp', 'cozy light', 0),
    'frame': (16, 12, 'Picture', 'for the wall', 0),
    'cup': (8, 12, 'Coffee', 'fuel', 0),
    'mat': (6, 16, 'Yoga mat', 'matches your fit', 0),
    'rug': (32, 5, 'Rug', 'matches your wall', 0),
    'mascot': (16, 26, 'You', 'one of you', 0),
}
TRAY_GROUPS = (
    ('std', 'Storage', 'bigger holds more · $__PRICE_STD__/TB downstairs, $__PRICE_VAULT__/TB in the attic',
     ('box', 'dresser', 'bookcase')),
    ('decor', 'Decor', 'free, just vibes', ('plant', 'lamp', 'frame', 'cup', 'mat', 'rug', 'mascot')),
)
WALL_SW = [('midnight', '#125594'), ('matcha', '#b8cfb0'), ('oat', '#eee3d0'),
           ('blush', '#f6cfd0'), ('butter', '#f8e9b3'), ('lilac', '#d7cdf1')]
FIT_SW = [('midnight', '#1c3445'), ('matcha', '#5f7f5a'), ('oat', '#b59f7c'),
          ('blush', '#d9828f'), ('lilac', '#8f7cc8'), ('noir', '#2b2b2b')]


def tray():
    out = ''
    std, vault = P['standard']['annual'], P['vault']['annual']
    for tier, title, blurb, keys in TRAY_GROUPS:
        out += (f'<div class="tray-group tg-{tier}"><p class="tray-title"><i class="t-chip t-{tier}"></i><b>{title}</b>'
                f'<span>{blurb}</span></p><div class="tray" role="group" aria-label="{title} pieces">')
        for k in keys:
            w, h, nm, what, tb = PIECES[k]
            if tb:
                price, attic, cls = money(tb * std) + '/mo', 'attic ' + money(tb * vault), f'pc-price t-{tier}'
            else:
                price, attic, cls = 'free', '', 'pc-price free'
            # the button's visible text IS its accessible name (no aria-label:
            # a spoken name must contain the visible label)
            out += (f'<button class="piece-card" type="button" data-add="{k}">'
                    + spr(k if k != 'mascot' else 'mascot-wave', w, h, cls='px sticker')
                    + f'<span class="pc-name">{nm}</span><span class="pc-what">{what}</span><span class="{cls}">{price}</span>'
                    + (f'<span class="pc-attic">{attic}</span>' if attic else '') + '</button>')
        out += '</div></div>'
    return out


def builder():
    b = read('builder.html')
    walls = ''.join(f'<button class="swatch" type="button" data-wall="{k}" style="--sw:{c}" aria-label="wall: {k}" aria-pressed="false"></button>'
                    for k, c in WALL_SW)
    fits = ''.join(f'<button class="swatch" type="button" data-fit="{k}" style="--sw:{c}" aria-label="fit: {k}" aria-pressed="false"></button>'
                   for k, c in FIT_SW)
    b = (b.replace('__TRAY__', tray()).replace('__WALLS__', walls).replace('__FITS__', fits)
          .replace('__CUP__', spr('cup', 8, 12, 3))
          .replace('__PHONE__', spr('phone', 9, 16)).replace('__LAPTOP__', spr('laptop', 22, 13))
          .replace('__MASCOT__', spr('mascot-wave', 16, 26, 3)).replace('__BOX__', spr('box', 17, 15, 3)))
    return b.rstrip('\n')


def script():
    js = read('page.js').rstrip('\n') + '\n' + read('builder.js').strip('\n')
    js = js.replace('__PRICES_JSON__', json.dumps({
        'ground': P['standard']['annual'], 'attic': P['vault']['annual'], 'aws': P['competitors']['aws_s3'],
    }))
    return js


# ---- sources hash: the guard test recomputes this ---------------------------
def sources_hash():
    h = hashlib.sha256()
    for root, dirs, files in os.walk(HERE):
        dirs[:] = sorted(d for d in dirs if d not in ('browser', '__pycache__'))
        for name in sorted(files):
            if name.endswith(('.pyc',)):
                continue
            path = os.path.join(root, name)
            rel = os.path.relpath(path, HERE).replace(os.sep, '/')
            h.update(rel.encode() + b'\n')
            with open(path, 'rb') as f:
                h.update(f.read())
            h.update(b'\n')
    return h.hexdigest()


# ---- the dashboard's house ----------------------------------------------------
HOUSE_SPRITES = ('box', 'dresser', 'bookcase', 'lamp', 'plant', 'rug', 'mascot', 'mascot-wave')


def vibes_css():
    """Wall and outfit colours as classes, parsed from builder.js so there is
    one table (the builder's) for both pages."""
    js = read('builder.js')
    walls = re.findall(r"^\s+(\w+):\s*\['(#[0-9a-f]{6})',\s*'(#[0-9a-f]{6})',\s*'(#[0-9a-f]{6})'\]", js, flags=re.M)
    fits_line = re.search(r"var FITS = \{([^}]*)\}", js).group(1)
    fits = re.findall(r"(\w+):\s*'(#[0-9a-f]{6})'", fits_line)
    if len(walls) != 6 or len(fits) != 6:
        sys.exit(f'build.py: expected 6 walls and 6 fits in builder.js, found {len(walls)} / {len(fits)}')
    out = [f'    .house-room.wall-{n} {{ --wall-a: {a}; --wall-b: {b}; --wall-deep: {c}; }}' for n, a, b, c in walls]
    out += [f'    .house-room.fit-{n} {{ --fit: {c}; }}' for n, c in fits]
    return '\n'.join(out)


def house_css():
    c = read('house.css')
    c = sub(c, '__VIBES__', vibes_css())
    c = re.sub(r'^    /\*NIGHT\*/ ([^{]+)\{(.*)\}$', night, c, flags=re.M)
    c = c.strip('\n')
    if c.startswith('<style>'):
        c = c[len('<style>'):].strip('\n')
    if c.endswith('</style>'):
        c = c[:-len('</style>')].rstrip('\n')
    return c


def house_sprite_defs():
    defs = ''.join(sprites.symbol(n, sprites.SPRITES[n]) for n in HOUSE_SPRITES)
    return ('<svg class="sprite-defs" width="0" height="0" aria-hidden="true" focusable="false">'
            f'<defs>{NIGHT_DEFS}{defs}</defs></svg>')


def house_room():
    """The room background from builder.html (walls, roof, ladder, sign,
    windows, day/night light layers, vignette) plus the wall gradient, with
    the price labels removed — the dashboard writes its own fullness labels —
    and the sign's text handed to the template."""
    b = read('builder.html')
    a = b.index('<svg class="room-svg"')
    d0 = b.index('<defs>', a); d1 = b.index('</defs>', d0) + len('</defs>')
    defs = b[d0:d1]
    r0 = b.index('<g id="room-bg"', d1); r1 = b.index('<g id="room-shadows">', r0)
    room = b[r0:r1].strip()
    z0 = room.index('<g class="zone-labels">'); z1 = room.index('</g>', z0) + len('</g>')
    room = room[:z0] + room[z1:]
    room = sub(room, '<g id="room-bg" aria-hidden="true">', '<g class="room-bg" aria-hidden="true">')
    room = sub(room, ' id="room-sign"', '')
    room = sub(room, '>my place<', '>__HOUSE_NAME__<')
    return defs + room


def dashboard_page():
    parts = {'house-sprites': house_sprite_defs(), 'house-css': '<style>\n' + house_css() + '\n</style>', 'house-room': house_room()}
    for name, body in parts.items():
        if '{{' in body:
            sys.exit(f'build.py: stray "{{{{" in {name} (html/template would choke)')
    parts['house-room'] = parts['house-room'].replace('__HOUSE_NAME__', '{{.Name}}')
    stamp = (f'<!-- GENERATED by internal/api/landing/build.py (make landing); sources sha256:{sources_hash()}. '
             'Edit the sources (house.css, sprites.py, builder.html), not this file. -->')
    return stamp + '\n' + ''.join(f'{{{{define "{n}"}}}}{body}{{{{end}}}}\n' for n, body in parts.items())


def og_page():
    """A standalone 1200x630 page for the social preview (make og renders it)."""
    b = read('builder.html')
    a = b.index('<svg class="room-svg"'); z = b.index('</svg>', a) + 6
    room = prices(b[a:z].replace(' id="room"', ''))  # price tokens inside the floor labels
    pieces = ''
    # the same example house as the builder's starter (builder.js STARTER)
    for k, x, y, tag in (('box', 16, 88, 'PHOTOS 1 TB'), ('lamp', 40, 90, ''), ('box', 58, 88, 'PROJECTS 1 TB'),
                         ('mascot-wave', 96, 77, ''), ('plant', 113, 80, ''), ('dresser', 30, 31, 'BACKUPS 5 TB')):
        w, h = sprites.SPRITES[k].w, sprites.SPRITES[k].h
        pieces += f'<g transform="translate({x} {y})"><use href="#s-{k}" width="{w}" height="{h}"/></g>'
        if tag:
            tw = len(tag) * 3 + 3
            tx = x + round(w / 2 - tw / 2)
            fill = '#b9c7d2' if y < 54 else '#ffd400'
            pieces += (f'<g transform="translate({tx} {y - 6})"><rect width="{tw}" height="5" fill="{fill}"/>'
                       f'<text x="{tw / 2}" y="4" text-anchor="middle" font-family="Silkscreen, monospace" font-size="4" fill="#1a1a1a">{tag}</text></g>')
    room = room.replace('<g id="room-items"></g>', f'<g color="#1c3445">{pieces}</g>')
    return f"""<!DOCTYPE html>
<html lang="en"><head><meta charset="UTF-8"><title>og</title>
<style>
{css()}
html, body {{ margin: 0; width: 1200px; height: 630px; overflow: hidden; background: #f4f1ec; }}
.og {{ display: grid; grid-template-columns: 560px 1fr; gap: 40px; width: 1200px; height: 630px; padding: 36px 48px; box-sizing: border-box; align-items: center; }}
.og .room-svg {{ width: 560px; height: auto; display: block; color: #1c3445; box-shadow: 0 22px 40px -24px rgba(0,0,0,.45); }}
.og h1 {{ font: 800 44px/1.1 'Montserrat', sans-serif; letter-spacing: -0.02em; color: #2b2b2b; margin: 0 0 18px; }}
.og h1 em {{ display: block; font-style: normal; font-weight: 300; }}
.og .line {{ font: 400 18px 'Silkscreen', monospace; text-transform: uppercase; color: #4d4d4d; margin: 10px 0; }}
.og .tags {{ display: flex; flex-wrap: wrap; gap: 8px; margin-top: 22px; }}
.og .tag {{ font-size: 13px; padding: 7px 9px; }}
.og .brand {{ display: flex; align-items: center; gap: 12px; margin-bottom: 26px; font: 800 22px 'Montserrat', sans-serif; text-transform: uppercase; color: #2b2b2b; }}
.og .brand .px {{ --u: 2.5px; color: #2b2b2b; }}
</style></head>
<body style="--wall-a:#0a3a63;--wall-b:#125594;--wall-deep:#0b2f52;--fit:#1c3445">
{sprite_defs()}
<div class="og">
  {room}
  <div>
    <div class="brand">{spr('mark', 14, 14)}<span>stored.ge</span></div>
    <h1>Your photos, your projects, your backups. <em>Put away, not thrown away.</em></h1>
    <p class="line">downstairs ${PRICE_TOKENS['__PRICE_STD__']}/TB &middot; attic ${PRICE_TOKENS['__PRICE_VAULT__']}/TB</p>
    <div class="tags"><span class="tag">no meters</span><span class="tag">no API fees</span><span class="tag tag-green">S3 compatible</span><span class="tag">open-source engine</span></div>
  </div>
</div>
</body></html>"""


def main():
    page = read('landing.src.html')
    page = sub(page, '__CSS__', css())
    page = sub(page, '__SPRITES__', sprite_defs())
    page = sub(page, '__BUILDER__', builder())
    page = sub(page, '__SCRIPT__', script())
    page = prices(page)
    if '{{' in page.replace('{{if', '').replace('{{else', '').replace('{{end', ''):
        sys.exit('build.py: stray "{{" in output (html/template would choke)')
    stamp = f'<!-- GENERATED by internal/api/landing/build.py (make landing); sources sha256:{sources_hash()}. Edit the sources, not this file. -->'
    page = sub(page, '<!DOCTYPE html>\n', '<!DOCTYPE html>\n' + stamp + '\n')
    with open(OUT, 'w', encoding='utf-8') as f:
        f.write(page)
    print(f'wrote {os.path.relpath(OUT)} ({len(page.encode()) // 1024} KB)')
    dash = dashboard_page()
    os.makedirs(os.path.dirname(DASH_OUT), exist_ok=True)
    with open(DASH_OUT, 'w', encoding='utf-8') as f:
        f.write(dash)
    print(f'wrote {os.path.relpath(DASH_OUT)} ({len(dash.encode()) // 1024} KB)')


if __name__ == '__main__':
    if len(sys.argv) > 2 and sys.argv[1] == 'og':
        os.makedirs(sys.argv[2], exist_ok=True)
        with open(os.path.join(sys.argv[2], 'og.html'), 'w', encoding='utf-8') as f:
            f.write(og_page())
        print('wrote', os.path.join(sys.argv[2], 'og.html'))
    else:
        main()
