# stored.ge design guide

*The visual and product language of the public site, written down so the dashboard (and anything else) can match it. Source of truth for values is `internal/api/landing/landing.css`; source of truth for prices is `internal/api/landing/prices.json`. Written 2026-09-27 after the landing redesign (PRs #479–#498). Inspiration: the SELFBOX brand/app study on Behance ("like Dropbox, but in real life"): flat pixel art, one bold accent, very few controls per screen.*

## 1. The idea in one line

**Storage is a house.** Furniture is data, bigger pieces hold more, the **floor is the tier**: downstairs is Standard (in reach, $4.49/TB), the attic is Vault (put away, $2/TB). The receipt is the plan. Every screen that shows storage should be explainable with that sentence.

Vocabulary, everywhere a customer reads:

| Say | Never say (to customers) | Internal id |
|---|---|---|
| downstairs | Standard tier, hot | `standard` |
| attic | Vault, cold, archive, Glacier | `vault` / `archive` |
| storage box / dresser / bookcase (1 / 5 / 10 TB) | quota units, GB | whole TB |
| put it away / bring it down | demote / promote / restore | tiering |
| opens instantly / back in minutes | latency, staging | hot read / restore |
| your house, your place | account, tenant | tenant |

Honesty rules: prices are printed where the thing is; the attic always says "back in minutes"; nothing is deleted by billing; no phone app exists yet, so say "from your laptop, a backup app, or the dashboard".

## 2. Colour

Light is the default; dark follows the system or the toggle (`data-theme` on `<html>`, saved as `localStorage.sg-theme`, shared by every page).

| Token | Light | Dark | Use |
|---|---|---|---|
| `--bg` | `#f4f1ec` (oat) | `#1a1a1a` | page |
| `--card` | `#ffffff` | `#242424` | cards, receipts' base |
| `--panel` / `--panel-hover` | `#ebe6de` / `#dfd8cd` | `#2f2f2f` / `#3a3a3a` | trays, chips, sliders |
| `--ink` | `#2b2b2b` | `#f3f3f3` | headings, strong |
| `--body` | `#4d4d4d` | `#cfcfcf` | text |
| `--dim` | `#737373` (`#666666` on the dashboard, whose captions sit on `--bg`) | `#a0a0a0` | captions (min 4.5:1 on its background) |
| `--line` | `#e3ddd3` | `#363636` | hairlines |
| `--bar` | `#383838` | `#101010` | top bar, footer |
| `--btn` / `--btn-ink` | `#333333` / `#ffffff` | `#f3f3f3` / `#1a1a1a` | primary buttons |
| `--yellow` | `#ffd400` | same | THE accent: tags, focus ring, highlights, downstairs |
| `--red` | `#d9321d` | same | destructive / warning tags only |
| `--green` | `#27c24c` | same | free / ok / bullets |
| `--blue` | `#5bc0eb` | same | secondary category |
| `--orange` | `#ff8a3d` | same | tertiary category |
| attic tag | `#b9c7d2` | same | grey-blue = put away |
| `--hl` | `#fff4bf` | `rgba(255,212,0,.12)` | "you are here" rows |
| walls (vibes) | midnight `#0a3a63→#125594`, matcha `#9fbc98→#b8cfb0`, oat `#e2d3ba→#eee3d0`, blush `#eeb6bb→#f6cfd0`, butter `#f1d98a→#f8e9b3`, lilac `#c2b3e6→#d7cdf1` | | room walls, user-picked |
| fits (outfit) | midnight `#1c3445`, matcha `#5f7f5a`, oat `#b59f7c`, blush `#d9828f`, lilac `#8f7cc8`, noir `#2b2b2b` | | mascot hoodie, tinted sprites |

Rules: one accent (yellow) does the pointing; red only for remove/danger; never a gradient on UI (gradients exist only as light in the rooms); colour is never the only signal (tags carry text, floors carry labels).

## 3. Type

- **Montserrat** (variable 300–800): headings 800 with a `-0.02em` track, the second line of a headline in 300 ("Pick it up. Put it away. *That's your storage.*"), body 400, buttons 600.
- **Silkscreen** (pixel, 400/700): captions, tags, kickers, table headers, counters. Uppercase, 10–12 px, `letter-spacing .03–.05em`. Silkscreen draws `&` like `$`, so its `unicode-range` skips `&`; write "and".
- **System mono** for code and the receipt body.
- Scale: h1 `clamp(2.3rem, 5.2vw, 3.7rem)`, section h2 `clamp(1.9rem, 3.8vw, 2.7rem)`, card h3 1.1–1.45rem, body 16px/1.6, captions 0.85–0.95rem.
- Fonts are embedded (base64 in the landing CSS; serve from the binary for other pages). No third-party requests, ever.

## 4. Shape and layout

- **Square everything.** No border radius on cards, buttons, inputs, tags. Sprites are pixel-crisp (`shape-rendering: crispEdges`).
- **Cards** are flat `--card` blocks with a 2 px transparent border that turns `--ink` on hover/selection. Emphasis by a 6 px left bar in `--btn`, `--yellow` or `--green`, never by shadow (shadows are reserved for the rooms).
- **Buttons:** primary = `--btn` block with a `▸` glyph after the label; ghost = 2 px `--ink` outline; "swipe bar" link = `--panel` block with a 6 px `--btn` left edge and a `▸` that slides on hover. Small pixel buttons (Silkscreen 10–11 px) for tools: charcoal with a 3 px offset shadow in yellow, red for remove.
- **Tags:** Silkscreen, 5×7 px padding, yellow by default; `.tag-red`, `.tag-green`; the attic grey-blue.
- **Inputs:** underline only (2 px `--dim`, `--ink` on focus). Sliders: square 26 px thumb, 10 px track.
- **Receipt:** `#fffaf0` paper, mono type, dotted leaders, torn edges (radial-gradient masks), café-style "name on the cup".
- Container 1120 px, 20 px gutters (16 px under 480 px), sections 80 px apart (60 on phones), phone width 390 px must never scroll sideways.
- Focus ring: 3 px `--yellow`, 2 px offset, on everything focusable.

## 5. Pixel art

- Sprites are generated by `internal/api/landing/sprites.py` from ASCII grids and small procedures, one `<symbol id="s-name">` per sprite, one `<path>` per colour, placed with `<use>`. Scale with `.px` and `--u` (px per pixel: 2 in trays and bands, 3 on tier cards, 4 in the why strip).
- 3D pieces use `cuboid()` (front face + top + side at 3 px depth); every standing piece ends in a 1 px `o` shadow row. Palette letters live at the top of `sprites.py`; `#` = `currentColor` for the tintable hoodie.
- Size means capacity: box 17×15 (1 TB), dresser 31×23 (5 TB), bookcase 31×44 (10 TB); the mascot is 26 tall, so a dresser is chest-high and a bookcase taller than a person. Keep that ordering if you add a piece.
- "Stickers" (`.sticker`) get a 2 px white die-cut via four drop-shadows plus a soft drop; used in trays, tier cards, drag ghosts.
- Two-frame animations (mascot wave, flame flicker) are two `<use>`s toggled by CSS opacity keyframes, never JS timers.
- Add a sprite: draw it in `sprites.py`, add to `SPRITES`, `make landing`, look at `sprites-preview` before using it.

## 6. Rooms and light

- A room is an SVG with a fixed viewBox (house 128×115: roof 0–10, attic 10–54, plank 54–57, downstairs 57–103, floorboards 103–115). Layers, bottom to top: background, shadows, pieces, light, tags.
- **Day:** wall gradient from the vibe, a sun shaft and floor patch under each window, soft contact shadows nudged away from the window.
- **Night** (dark mode): a `#070b24` dim at 50 %, a vignette, moon and stars in the windows, cool moonlight patches; lamps get three screen-blended pools (core, ambient, room fill) and a 3.8 s breathe; unlit furniture takes a night tint in three steps by distance from a lamp (`nightwarm` / `nightmid` / `nightdim` filters); shadows stretch away from the nearest lamp. Everything recomputes when a piece moves.
- Night CSS is written once with a `/*NIGHT*/` marker and expanded by the generator into the system-dark and `data-theme="dark"` selector forms, so dark is right on first paint.

## 7. Motion

- Short and physical: 120–180 ms eases for hover lifts, a `cubic-bezier(.3,1.6,.5,1)` pop when a piece lands, cards rise 16 px on scroll where `animation-timeline: view()` exists.
- Feedback words float up from the thing you touched ("+5 TB downstairs", "attic: −$12.45/mo", "still there ✓ instantly"). Toasts sit bottom-centre in Silkscreen with a yellow offset shadow; destructive actions always offer undo.
- Respect `prefers-reduced-motion`: no band scroll, no waves, no breathing, no rises.

## 8. Voice

Short, warm, concrete, never salesy. Second person. Lowercase pixel captions ("psst: the furniture moves"), sentence-case headings, one joke per screen at most. Say what things cost where they are; say what's not built yet in the same breath ("whole-TB checkout is on its way"). The mascot is "you".

## 9. Accessibility bar

Lighthouse 100 on every public page: every control has a name that contains its visible text, no role without its children, headings in order, tables with row headers, contrast ≥ 4.5:1 for text (use `--body`, not `--dim`, on tinted backgrounds), keyboard paths for every pointer gesture (arrow keys move pieces, Enter flips floors, Delete removes), `aria-live` for price changes.

## 10. How to work on it

1. Branch from `origin/main` in a **git worktree** (another session may be using the main checkout).
2. Edit sources under `internal/api/landing/`, never `landing.html`; run `make landing`. Prices only in `prices.json`. The `landing_build_test` guards the stamp.
3. `make landing-browser` (33 headless-Chrome checks) and `go test ./internal/api/...`; screenshot at 1280 light/dark and 390 wide before opening a PR.
4. `make og` if the house scene or prices changed.
5. PRs auto-merge on green; the main ruleset needs an up-to-date branch and resolved review threads (CodeQL comments count). Deploys restart the server; in-memory user cache refreshes then.
6. For the dashboard: port the tokens in §2–§4 into `layouts/base.html`, serve the two fonts from the binary, reuse the sprite defs via the generator's dashboard output (`make landing` also writes `internal/dashboard/templates/generated/house.html`; the house's own CSS lives in `landing/house.css`), and follow `docs/DASHBOARD_PLAN.md`.
