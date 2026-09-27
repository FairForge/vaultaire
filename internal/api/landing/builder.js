
    // ---- 02: build-your-house. Furniture is storage and the FLOOR is the
    // tier: downstairs = Standard ($__PRICE_STD__/TB), attic = Vault ($__PRICE_VAULT__/TB). Decor is
    // free. The house, vibe and name persist on this device and travel in
    // #room= links.
    (function () {
        var NS = 'http://www.w3.org/2000/svg';
        var PRICE = __PRICES_JSON__;
        var PHOTOS_PER_TB = 250000; // ~4 MB a photo
        var ZONE = {
            attic:  { tag: '#b9c7d2', floor: 54,  name: 'Attic', tier: 'Vault' },
            ground: { tag: '#ffd400', floor: 103, name: 'Downstairs', tier: 'Standard' }
        };
        var W = 128, H = 115, MAX = 30;
        var P = {
            box:      { w: 17, h: 15, name: 'Storage box', plural: 'storage boxes', tb: 1 },
            dresser:  { w: 31, h: 23, name: 'Dresser', plural: 'dressers', tb: 5 },
            bookcase: { w: 31, h: 44, name: 'Bookcase', plural: 'bookcases', tb: 10 },
            plant:    { w: 14, h: 23, name: 'Plant' },
            lamp:     { w: 10, h: 13, name: 'Lamp', glow: [5, 3, 18, 13] },
            frame:    { w: 16, h: 12, name: 'Picture', wall: true },
            cup:      { w: 8,  h: 12, name: 'Coffee' },
            mat:      { w: 6,  h: 16, name: 'Yoga mat' },
            rug:      { w: 32, h: 5,  name: 'Rug', flat: true, deep: true },
            mascot:   { w: 16, h: 26, name: 'You', max: 1, alt: 'mascot-wave', anim: 'wave' }
        };
        // what you pack: label key -> [piece, label]; labels ride on pieces, links and the image
        var LABEL = { p: 'photos', v: 'videos', w: 'projects', e: 'everything' };
        var PACK = { p: 'box', v: 'dresser', w: 'box', e: 'bookcase' };
        // earlier share links: the pieces they used, and the floor they land on
        Object.keys(P).forEach(function (k) { P[k].k = k; });
        var ALIAS = { shelf: ['bookcase', 'attic'], safe: ['bookcase', 'attic'], lockbox: ['box', 'attic'], fireplace: ['lamp'] };
        var WALLS = {
            midnight: ['#0a3a63', '#125594', '#0b2f52'],
            matcha:   ['#9fbc98', '#b8cfb0', '#6f8f6a'],
            oat:      ['#e2d3ba', '#eee3d0', '#c4a983'],
            blush:    ['#eeb6bb', '#f6cfd0', '#d98c98'],
            butter:   ['#f1d98a', '#f8e9b3', '#d4ae46'],
            lilac:    ['#c2b3e6', '#d7cdf1', '#9d8ccf']
        };
        var FITS = { midnight: '#1c3445', matcha: '#5f7f5a', oat: '#b59f7c', blush: '#d9828f', lilac: '#8f7cc8', noir: '#2b2b2b' };
        var STARTER = [['dresser', 36, 80], ['lamp', 56, 70], ['box', 17, 88], ['plant', 74, 80], ['mascot', 94, 77], ['box', 30, 39]];

        var store = {
            get: function (k) { try { return window.localStorage.getItem(k); } catch (e) { return null; } },
            set: function (k, v) { try { window.localStorage.setItem(k, v); } catch (e) { /* private mode: fine */ } }
        };
        function clamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }
        function money(n) { return '$' + n.toFixed(2).replace(/\B(?=(\d{3})+(?!\d))/g, ','); }
        function cleanName(s) { return String(s || '').replace(/[^\p{L}\p{N} _'-]/gu, '').slice(0, 16); }
        function mkItem(k, x, y, l) {
            var p = P[k], it = { k: k, x: clamp(Math.round(x), 0, W - p.w), y: clamp(Math.round(y), 0, H - p.h) };
            if (l && LABEL[l]) it.l = l;
            return it;
        }
        // which floor a piece is on: judged by its middle, so a half-dragged
        // piece already reads as "going up" while it moves
        function zoneOf(item) { return item.y + P[item.k].h / 2 < ZONE.attic.floor + 3 ? 'attic' : 'ground'; }
        // storage pieces always stand on a floor; decor may float (lamp on a dresser)
        function settle(item) {
            var p = P[item.k];
            if (!p.tb) return item;
            item.y = ZONE[zoneOf(item)].floor - p.h;
            return item;
        }
        function place(k, zone, l) {
            var p = P[k], y = p.flat ? ZONE[zone].floor - 5 : (p.wall ? (zone === 'attic' ? 20 : 74) : ZONE[zone].floor - p.h);
            return settle(mkItem(k, freeX(p, zone, null, l), y, l));
        }
        function starter() { return STARTER.map(function (s) { return settle(mkItem(s[0], s[1], s[2])); }); }
        function rateOf(item) { return PRICE[zoneOf(item)]; }
        function costOf(item) { return P[item.k].tb ? P[item.k].tb * rateOf(item) : 0; }
        function what(item) {
            var p = P[item.k];
            return p.tb ? (item.l ? LABEL[item.l] + ', ' : '') + p.tb + ' TB, ' + ZONE[zoneOf(item)].name.toLowerCase() + ' (' + ZONE[zoneOf(item)].tier + ')' : 'decor, free';
        }
        function tagText(item) { return (item.l ? LABEL[item.l].toUpperCase() + ' ' : '') + P[item.k].tb + ' TB'; }

        var state = { wall: 'midnight', fit: 'midnight', name: '', items: starter() };

        // ---- vibe: wall + fit colours are site-wide CSS variables
        function applyVibe() {
            var w = WALLS[state.wall], r = document.documentElement.style;
            r.setProperty('--wall-a', w[0]);
            r.setProperty('--wall-b', w[1]);
            r.setProperty('--wall-deep', w[2]);
            r.setProperty('--fit', FITS[state.fit]);
            document.querySelectorAll('[data-wall]').forEach(function (b) {
                b.setAttribute('aria-pressed', b.getAttribute('data-wall') === state.wall ? 'true' : 'false');
            });
            document.querySelectorAll('[data-fit]').forEach(function (b) {
                b.setAttribute('aria-pressed', b.getAttribute('data-fit') === state.fit ? 'true' : 'false');
            });
        }

        // ---- share format: v2.<wall>.<fit>.<name>.<k-x-y_k-x-y...> (y in house
        // coordinates; the floor is implied). v1 links (single room) are mapped
        // onto the ground floor, with the old archive pieces sent to the attic.
        function encode() {
            return 'v2.' + state.wall + '.' + state.fit + '.' + encodeURIComponent(state.name) + '.' +
                state.items.map(function (i) { return i.k + '-' + i.x + '-' + i.y + (i.l ? '-' + i.l : ''); }).join('_');
        }
        function decode(str) {
            var parts = String(str || '').split('.');
            if (parts.length !== 5 || (parts[0] !== 'v1' && parts[0] !== 'v2') || !WALLS[parts[1]] || !FITS[parts[2]]) return null;
            var v1 = parts[0] === 'v1', name = '';
            try { name = cleanName(decodeURIComponent(parts[3])); } catch (e) { name = ''; }
            var items = [], you = 0;
            parts[4].split('_').forEach(function (t) {
                var m = /^([a-z]+)-(\d{1,3})-(\d{1,3})(?:-([pvwe]))?$/.exec(t);
                if (!m) return;
                var k = m[1], zone = null;
                if (Object.prototype.hasOwnProperty.call(ALIAS, k)) { zone = ALIAS[k][1] || null; k = ALIAS[k][0]; }
                if (!Object.prototype.hasOwnProperty.call(P, k) || items.length >= MAX) return;
                if (k === 'mascot' && you++) return;
                var y = Number(m[3]);
                if (v1) y = zone === 'attic' ? ZONE.attic.floor - P[k].h : y + (ZONE.ground.floor - 64);
                items.push(settle(mkItem(k, Number(m[2]), y, m[4])));
            });
            return { wall: parts[1], fit: parts[2], name: name, items: items };
        }
        function save() { store.set('sg-room', encode()); }

        var fromLink = false;
        (function load() {
            var h = window.location.hash, got = null;
            if (h.indexOf('#room=') === 0) { got = decode(h.slice(6)); fromLink = !!got; }
            if (!got) got = decode(store.get('sg-room'));
            if (got) state = got;
        })();
        applyVibe();

        // ---- SVG helpers
        function el(tag, attrs) {
            var n = document.createElementNS(NS, tag);
            for (var a in attrs) { if (Object.prototype.hasOwnProperty.call(attrs, a)) n.setAttribute(a, attrs[a]); }
            return n;
        }
        function svgPoint(svg, cx, cy) {
            var pt = svg.createSVGPoint();
            pt.x = cx; pt.y = cy;
            return pt.matrixTransform(svg.getScreenCTM().inverse());
        }

        // Drag any .piece around its SVG, snapped to the pixel grid. opts.outside
        // reports a drop beyond the SVG's box (the builder treats that as remove).
        function makeDraggable(svg, g, pos, w, h, vb, opts) {
            opts = opts || {};
            g.addEventListener('pointerdown', function (e) {
                if (e.button > 0) return;
                e.preventDefault();
                var start = svgPoint(svg, e.clientX, e.clientY), ox = pos.x, oy = pos.y, moved = false, out = false;
                try { g.setPointerCapture(e.pointerId); } catch (err) { /* older engines */ }
                g.classList.add('lift');
                g.parentNode.appendChild(g);
                if (g.focus) { try { g.focus({ preventScroll: true }); } catch (err) { /* no focus on svg */ } }
                function mv(ev) {
                    var p = svgPoint(svg, ev.clientX, ev.clientY);
                    var nx = Math.round(ox + p.x - start.x), ny = Math.round(oy + p.y - start.y);
                    if (nx !== ox || ny !== oy) { moved = true; if (opts.moving) opts.moving(); }
                    var r = svg.getBoundingClientRect();
                    out = !!opts.outside && (ev.clientX < r.left - 8 || ev.clientX > r.right + 8 || ev.clientY < r.top - 8 || ev.clientY > r.bottom + 8);
                    pos.x = out ? nx : clamp(nx, 0, vb[0] - w);
                    pos.y = out ? ny : clamp(ny, 0, vb[1] - h);
                    g.setAttribute('transform', 'translate(' + pos.x + ' ' + pos.y + ')');
                    g.style.opacity = out ? '0.35' : '';
                    if (opts.follow) opts.follow();
                }
                function up() {
                    g.removeEventListener('pointermove', mv);
                    g.classList.remove('lift');
                    g.style.opacity = '';
                    if (out) { opts.outside(); return; }
                    pos.x = clamp(pos.x, 0, vb[0] - w);
                    pos.y = clamp(pos.y, 0, vb[1] - h);
                    g.setAttribute('transform', 'translate(' + pos.x + ' ' + pos.y + ')');
                    if (moved && navigator.vibrate) { try { navigator.vibrate(6); } catch (err) { /* ignore */ } }
                    if (opts.end) opts.end(moved);
                }
                g.addEventListener('pointermove', mv);
                g.addEventListener('pointerup', up, { once: true });
                g.addEventListener('pointercancel', up, { once: true });
            });
        }

        // ---- lighting: is it night, where are the lights, how does a piece cast a shadow
        var darkMQ = window.matchMedia('(prefers-color-scheme: dark)');
        function isNight() {
            var t = document.documentElement.getAttribute('data-theme');
            return t ? t === 'dark' : darkMQ.matches;
        }
        // a cast shadow under a piece: soft and centred by default, stretched away
        // from the strongest nearby light (a lamp at night, the window by day)
        function shadowShape(cx, bottom, w, lights, night) {
            var best = 0, dir = 0;
            lights.forEach(function (L) {
                var dx = cx - L.x, d = Math.abs(dx);
                if (d >= L.reach) return;
                var s = (1 - d / L.reach) * L.weight;
                if (s > best) { best = s; dir = dx >= 0 ? 1 : -1; }
            });
            return {
                cx: cx + dir * best * w * 0.45, cy: bottom - 0.6,
                rx: w * 0.55 + best * w * 0.7, ry: 2.2 + best * 0.8,
                o: Math.min((night ? 0.34 : 0.2) + best * 0.18, 0.55)
            };
        }
        function setShadow(node, s) {
            node.setAttribute('cx', s.cx); node.setAttribute('cy', s.cy);
            node.setAttribute('rx', s.rx); node.setAttribute('ry', s.ry);
            node.setAttribute('fill-opacity', s.o);
        }
        function shadowNode() { return el('ellipse', { 'class': 'shadow', fill: '#000', filter: 'url(#soft)' }); }
        // how lit a point is by the lamps (0..1), from the ambient pool geometry
        function lampLevel(px, py, lamps) {
            var lvl = 0;
            lamps.forEach(function (L) {
                var dx = (px - L.x) / L.rx, dy = (py - L.y) / L.ry;
                lvl = Math.max(lvl, 1 - Math.sqrt(dx * dx + dy * dy));
            });
            return lvl;
        }
        function tintFor(lvl) { return lvl > 0.45 ? 'url(#nightwarm)' : lvl > 0.12 ? 'url(#nightmid)' : 'url(#nightdim)'; }

        // decorative scenes (hero room, final CTA): pick things up, put them down;
        // pieces cast shadows and take the lamp light like the builder's do
        document.querySelectorAll('svg[data-play]').forEach(function (svg) {
            var vb = svg.getAttribute('viewBox').split(' ').map(Number).slice(2), entries = [];
            var shadows = el('g', { 'class': 'scene-shadows', 'aria-hidden': 'true' });
            var first = svg.querySelector('.piece');
            if (first) first.parentNode.insertBefore(shadows, first);
            function relightScene() {
                var night = isNight(), lamps = [], lights = [];
                entries.forEach(function (e) {
                    if (!e.core) return;
                    lamps.push({ x: e.pos.x + e.core.cx, y: e.pos.y + e.core.cy + 5, rx: e.core.rx * 3.6, ry: e.core.ry * 3.1 });
                    lights.push({ x: e.pos.x + e.core.cx, reach: e.core.rx * 4, weight: 1 });
                });
                if (!night) lights = [{ x: -40, reach: vb[0] + 80, weight: 0.35 }]; // daylight from the left
                entries.forEach(function (e) {
                    setShadow(e.shadow, shadowShape(e.pos.x + e.w / 2, e.pos.y + e.h, e.w, lights, night));
                    if (!e.core) e.g.style.setProperty('--nf', tintFor(night ? lampLevel(e.pos.x + e.w / 2, e.pos.y + e.h / 2, lamps) : 0));
                });
            }
            svg.querySelectorAll('.piece').forEach(function (g) {
                var m = /translate\((-?\d+) (-?\d+)\)/.exec(g.getAttribute('transform') || '');
                var hit = g.querySelector('.hit');
                if (!m || !hit) return;
                var pos = { x: Number(m[1]), y: Number(m[2]) }, w = Number(hit.getAttribute('width')), h = Number(hit.getAttribute('height'));
                var glows = g.querySelectorAll('ellipse.glow'), c = glows.length ? glows[glows.length - 1] : null;
                var e = { g: g, pos: pos, w: w, h: h, shadow: shadowNode(), core: c ? { cx: +c.getAttribute('cx'), cy: +c.getAttribute('cy'), rx: +c.getAttribute('rx'), ry: +c.getAttribute('ry') } : null };
                entries.push(e);
                shadows.appendChild(e.shadow);
                makeDraggable(svg, g, pos, w, h, vb, { follow: relightScene, end: relightScene });
            });
            document.addEventListener('sg-theme', relightScene);
            relightScene();
        });

        var room = document.getElementById('room');
        if (!room) return;
        var itemsLayer = document.getElementById('room-items');
        var tagsLayer = document.getElementById('room-tags');
        var lightLayer = document.getElementById('room-light');
        var shadowLayer = document.getElementById('room-shadows');
        var roomBox = document.querySelector('.b-room');
        var sign = document.getElementById('room-sign');
        var live = document.getElementById('b-live');
        var nameInput = document.getElementById('vault-name');
        var receipt = document.getElementById('receipt');
        var tools = document.getElementById('b-piece-tools');
        var moveBtn = document.getElementById('b-move');
        var openBtn = document.getElementById('b-open');
        var removeBtn = document.getElementById('b-remove');
        var emptyText = document.getElementById('room-empty');
        var selected = null;

        function announce(msg) { if (live) live.textContent = msg; }
        function roomXY(x, y) {
            var r = room.getBoundingClientRect(), b = roomBox.getBoundingClientRect();
            return { left: r.left - b.left + (x / W) * r.width, top: r.top - b.top + (y / H) * r.height };
        }
        function floatNote(text, x, y) {
            var at = roomXY(x, y), n = document.createElement('span');
            n.className = 'float-note';
            n.textContent = text;
            n.style.left = at.left + 'px';
            n.style.top = at.top + 'px';
            roomBox.appendChild(n);
            setTimeout(function () { n.remove(); }, 1400);
        }
        function toast(msg, action, fn) {
            document.querySelectorAll('.toast').forEach(function (t) { t.remove(); });
            var t = document.createElement('div');
            t.className = action ? 'toast act' : 'toast';
            t.setAttribute('role', 'status');
            var m = document.createElement('span');
            m.textContent = msg;
            t.appendChild(m);
            if (action) {
                var b = document.createElement('button');
                b.type = 'button';
                b.textContent = action;
                b.addEventListener('click', function () { t.remove(); fn(); });
                t.appendChild(b);
            }
            document.body.appendChild(t);
            setTimeout(function () { t.remove(); }, action ? 5000 : 2500);
        }

        function signText() { return state.name ? state.name.toLowerCase() + '’s place' : 'my place'; }
        function renderSign() {
            if (!sign) return;
            sign.textContent = signText();
            if (signText().length > 11) {
                sign.setAttribute('textLength', '38');
                sign.setAttribute('lengthAdjust', 'spacingAndGlyphs');
            } else {
                sign.removeAttribute('textLength');
                sign.removeAttribute('lengthAdjust');
            }
        }

        // ---- the selected piece gets floating buttons: move floor (storage) + remove
        function placeTools() {
            if (!selected || !selected.isConnected) { tools.hidden = true; return; }
            var it = selected._item, p = P[it.k], at = roomXY(it.x + p.w / 2, it.y);
            tools.style.left = at.left + 'px';
            tools.style.top = at.top + 'px';
            moveBtn.hidden = !p.tb;
            openBtn.hidden = !p.tb;
            if (p.tb) {
                var up = zoneOf(it) === 'ground';
                moveBtn.textContent = up ? 'to attic' : 'bring down';
                moveBtn.setAttribute('aria-label', up ? 'Move to the attic, $__PRICE_VAULT__ per TB' : 'Bring downstairs, $__PRICE_STD__ per TB');
            }
            tools.hidden = false;
        }
        function select(g) { selected = g; placeTools(); }
        function unselect() {
            setTimeout(function () {
                var a = document.activeElement;
                if (a === removeBtn || a === moveBtn || a === openBtn) return;
                if (selected && a === selected) return;
                selected = null;
                tools.hidden = true;
            }, 0);
        }
        [openBtn, moveBtn, removeBtn].forEach(function (b) {
            b.addEventListener('mousedown', function (e) { e.preventDefault(); });
            b.addEventListener('blur', unselect);
        });
        removeBtn.addEventListener('click', function () { if (selected) removeItem(selected._item); });
        // "get it back any time": downstairs answers instantly, the attic in minutes
        openBtn.addEventListener('click', function () {
            if (!selected) return;
            var it = selected._item, p = P[it.k], up = zoneOf(it) === 'attic';
            floatNote(up ? 'still there \u2713 back in minutes' : 'still there \u2713 instantly', it.x + p.w / 2, it.y);
            announce((it.l ? LABEL[it.l] : p.name) + (up ? ' is in the attic: it comes back in minutes.' : ' is downstairs: it opens instantly.'));
        });
        moveBtn.addEventListener('click', function () {
            if (!selected) return;
            var it = selected._item, p = P[it.k], from = zoneOf(it), to = from === 'ground' ? 'attic' : 'ground';
            it.y = ZONE[to].floor - p.h;
            it.x = freeX(p, to, it);
            settled(it, from);
            render(it);
            save();
        });
        window.addEventListener('resize', placeTools);

        // after a storage piece lands: note the floor + price change, if any
        function settled(item, from) {
            var p = P[item.k], to = zoneOf(item);
            if (!p.tb || from === to) return;
            var delta = p.tb * (PRICE[to] - PRICE[from]);
            floatNote(ZONE[to].name.toLowerCase() + ': ' + (delta < 0 ? '−' : '+') + money(Math.abs(delta)) + '/mo', item.x + p.w / 2, item.y);
            announce(p.name + ' moved to the ' + ZONE[to].name.toLowerCase() + ', now ' + money(costOf(item)) + ' a month. Total ' + money(totals().total) + '.');
            if (navigator.vibrate) { try { navigator.vibrate(8); } catch (e) { /* ignore */ } }
        }

        function order() {
            return state.items.slice().sort(function (a, b) {
                var fa = P[a.k].flat ? 0 : 1, fb = P[b.k].flat ? 0 : 1;
                return fa - fb || (a.y + P[a.k].h) - (b.y + P[b.k].h);
            });
        }
        function uses(g, k, p) {
            if (p.alt) {
                g.appendChild(el('use', { href: '#s-' + k, width: p.w, height: p.h, 'class': p.anim + '-a' }));
                g.appendChild(el('use', { href: '#s-' + p.alt, width: p.w, height: p.h, 'class': p.anim + '-b' }));
            } else {
                g.appendChild(el('use', { href: '#s-' + k, width: p.w, height: p.h }));
            }
        }
        // a lamp's light at night: a wide ambient pool that fills the room plus a
        // bright core around the shade; both sit on the light layer above the furniture
        function lightPool(p) {
            var g = el('g', { 'class': 'light-pool', 'aria-hidden': 'true' });
            g.appendChild(el('ellipse', { 'class': 'glow', cx: p.glow[0], cy: p.glow[1] + 10, rx: p.glow[2] * 6, ry: p.glow[3] * 4.6, fill: 'url(#lampfill)' }));
            g.appendChild(el('ellipse', { 'class': 'glow', cx: p.glow[0], cy: p.glow[1] + 5, rx: p.glow[2] * 3.6, ry: p.glow[3] * 3.1, fill: 'url(#lampambient)' }));
            g.appendChild(el('ellipse', { 'class': 'glow', cx: p.glow[0], cy: p.glow[1], rx: p.glow[2], ry: p.glow[3], fill: 'url(#lampglow)' }));
            return g;
        }
        function tagNode(item) {
            var p = P[item.k], t = tagText(item), tw = t.length * 3 + 3;
            var tg = el('g', { 'class': 'tb-tag', 'aria-hidden': 'true' });
            tg._dx = Math.round(p.w / 2 - tw / 2);
            tg._rect = el('rect', { width: tw, height: 5 });
            tg.appendChild(tg._rect);
            var tx = el('text', { x: tw / 2, y: 4, 'text-anchor': 'middle', 'font-family': 'Silkscreen, monospace', 'font-size': '4', fill: '#1a1a1a' });
            tx.textContent = t;
            tg.appendChild(tx);
            return tg;
        }
        function pieceNode(item) {
            var p = P[item.k];
            var label = p.name + ': ' + what(item) + (p.tb ? ', ' + money(costOf(item)) + ' a month' : '');
            var g = el('g', {
                'class': p.glow ? 'piece light' : 'piece', transform: 'translate(' + item.x + ' ' + item.y + ')',
                tabindex: '0', role: 'button',
                'aria-label': label + '. Drag or use arrow keys to move; Delete removes it.'
            });
            var title = el('title', {});
            title.textContent = label;
            g.appendChild(title);
            if (p.deep) g.style.color = 'var(--wall-deep)';
            if (p.glow) g._light = lightPool(p);
            if (!p.flat && !p.wall) g._shadow = shadowNode();
            g.appendChild(el('rect', { 'class': 'hit', width: p.w, height: p.h }));
            uses(g, item.k, p);
            if (p.tb) g._tag = tagNode(item);
            function follow() {
                if (g._light) g._light.setAttribute('transform', 'translate(' + item.x + ' ' + item.y + ')');
                relight();
                if (!g._tag) return;
                g._tag.setAttribute('transform', 'translate(' + (item.x + g._tag._dx) + ' ' + (item.y - 6) + ')');
                g._tag._rect.setAttribute('fill', ZONE[zoneOf(item)].tag);
            }
            follow();
            var fromZone = zoneOf(item);
            makeDraggable(room, g, item, p.w, p.h, [W, H], {
                outside: function () { removeItem(item); },
                moving: function () { tools.hidden = true; },
                follow: follow,
                end: function (moved) {
                    if (!moved) { select(g); return; }
                    settle(item);
                    settled(item, fromZone);
                    render(item);
                    save();
                }
            });
            g.addEventListener('focus', function () { select(g); });
            g.addEventListener('blur', unselect);
            g.addEventListener('keydown', function (e) {
                var step = e.shiftKey ? 4 : 1, dx = 0, dy = 0;
                if (e.key === 'ArrowLeft') dx = -step;
                else if (e.key === 'ArrowRight') dx = step;
                else if (e.key === 'ArrowUp') dy = -step;
                else if (e.key === 'ArrowDown') dy = step;
                else if (e.key === 'Delete' || e.key === 'Backspace') { e.preventDefault(); removeItem(item, true); return; }
                else if (e.key === 'Enter' && p.tb) { e.preventDefault(); moveBtn.click(); return; }
                else return;
                e.preventDefault();
                if (p.tb && dy) { // storage hops between floors instead of floating
                    var was = zoneOf(item);
                    item.y = ZONE[dy < 0 ? 'attic' : 'ground'].floor - p.h;
                    item.x = clamp(item.x, 0, W - p.w);
                    if (zoneOf(item) !== was) { settled(item, was); render(item); save(); }
                    return;
                }
                item.x = clamp(item.x + dx, 0, W - p.w);
                item.y = clamp(item.y + dy, 0, H - p.h);
                g.setAttribute('transform', 'translate(' + item.x + ' ' + item.y + ')');
                follow();
                placeTools();
                save();
            });
            g._item = item;
            return g;
        }
        function render(focusItem, popItem) {
            selected = null;
            tools.hidden = true;
            while (itemsLayer.firstChild) itemsLayer.removeChild(itemsLayer.firstChild);
            while (tagsLayer.firstChild) tagsLayer.removeChild(tagsLayer.firstChild);
            while (lightLayer.firstChild) lightLayer.removeChild(lightLayer.firstChild);
            while (shadowLayer.firstChild) shadowLayer.removeChild(shadowLayer.firstChild);
            order().forEach(function (item) {
                var g = pieceNode(item);
                itemsLayer.appendChild(g);
                if (g._shadow) shadowLayer.appendChild(g._shadow);
                if (g._light) lightLayer.appendChild(g._light);
                if (g._tag) tagsLayer.appendChild(g._tag);
                if (item === popItem) g.classList.add('pop');
                if (item === focusItem) { try { g.focus({ preventScroll: true }); } catch (e) { /* ignore */ } }
            });
            if (emptyText) emptyText.setAttribute('display', state.items.length ? 'none' : 'inline');
            relight();
            updateTray();
            updateReceipt();
        }

        // the lights on a floor: lamps at night, the window by day (walls block the other floor)
        var WINDOW_X = { ground: 99, attic: 104 };
        function lampsOn(zone) {
            var lamps = [];
            state.items.forEach(function (o) {
                var q = P[o.k];
                if (!q.glow || zoneOf(o) !== zone) return;
                lamps.push({ x: o.x + q.glow[0], y: o.y + q.glow[1] + 5, rx: q.glow[2] * 3.6, ry: q.glow[3] * 3.1, reach: q.glow[2] * 4 });
            });
            return lamps;
        }
        function lightsFor(zone, night, lamps) {
            if (night) return lamps.map(function (L) { return { x: L.x, reach: L.reach, weight: 1 }; });
            return [{ x: WINDOW_X[zone], reach: 80, weight: 0.5 }];
        }
        function pieceLight(item, night, lamps) {
            var p = P[item.k];
            return night && !p.glow ? lampLevel(item.x + p.w / 2, item.y + p.h / 2, lamps) : 0;
        }
        // shadows stretch away from the light and pieces near a lamp take a warm tint
        function relight() {
            var night = isNight(), cache = {};
            itemsLayer.querySelectorAll('.piece').forEach(function (g) {
                var it = g._item;
                if (!it) return;
                var p = P[it.k], zone = zoneOf(it);
                if (!cache[zone]) { var lamps = lampsOn(zone); cache[zone] = { lamps: lamps, lights: lightsFor(zone, night, lamps) }; }
                if (g._shadow) setShadow(g._shadow, shadowShape(it.x + p.w / 2, it.y + p.h, p.w, cache[zone].lights, night));
                if (!p.glow) g.style.setProperty('--nf', tintFor(pieceLight(it, night, cache[zone].lamps)));
            });
        }
        document.addEventListener('sg-theme', relight);

        // a free x on the given floor, clear of the ladder and other floor pieces;
        // a storage piece claims the wider of itself and its tag so labels never overlap
        function spanOf(k, l) {
            var p = P[k], tw = p.tb ? (tagText({ k: k, l: l }).length * 3 + 3) : 0, w = Math.max(p.w, tw);
            return { off: Math.round(p.w / 2 - w / 2), w: w };
        }
        function freeX(p, zone, self, l) {
            var me = spanOf(p.k, self ? self.l : l);
            for (var x = 16; x <= W - p.w - 2; x += 2) {
                var a0 = x + me.off, a1 = a0 + me.w;
                var clash = state.items.some(function (i) {
                    var q = P[i.k];
                    if (i === self || q.flat || q.wall || zoneOf(i) !== zone) return false;
                    var sp = spanOf(i.k, i.l), b0 = i.x + sp.off, b1 = b0 + sp.w;
                    return a0 < b1 + 2 && a1 + 2 > b0;
                });
                if (!clash) return x;
            }
            return 16 + Math.floor(Math.random() * (W - p.w - 20));
        }
        function canAdd(k) {
            if (state.items.length >= MAX) return false;
            if (P[k].max && state.items.filter(function (i) { return i.k === k; }).length >= P[k].max) return false;
            return true;
        }
        function addItem(k, x, y, zone, l, quiet) {
            var p = P[k];
            if (!p) return false;
            if (!canAdd(k)) { toast(state.items.length >= MAX ? 'house is full: remove something first' : 'there is only one you'); return false; }
            var item = x === undefined ? place(k, zone || 'ground', l) : settle(mkItem(k, x, y, l));
            if (quiet) { state.items.push(item); return true; }
            state.items.push(item);
            render(null, item);
            floatNote(p.tb ? '+' + p.tb + ' TB ' + ZONE[zoneOf(item)].name.toLowerCase() : 'free', item.x + p.w / 2, item.y);
            announce('Added ' + p.name + ': ' + what(item) + '. Total ' + money(totals().total) + ' a month.');
            if (navigator.vibrate) { try { navigator.vibrate(8); } catch (e) { /* ignore */ } }
            save();
            return true;
        }
        function removeItem(item, viaKeyboard) {
            var i = state.items.indexOf(item);
            if (i < 0) return;
            state.items.splice(i, 1);
            var p = P[item.k];
            render();
            floatNote(p.tb ? '−' + p.tb + ' TB' : 'bye', item.x + p.w / 2, clamp(item.y, 4, H - 8));
            announce('Removed ' + p.name + '. Total ' + money(totals().total) + ' a month.');
            toast(p.name + ' removed', 'undo', function () {
                state.items.push(item);
                render(item, item);
                save();
                announce('Put back ' + p.name + '.');
            });
            if (viaKeyboard) {
                var next = itemsLayer.querySelector('.piece');
                if (next) next.focus(); else document.querySelector('.piece-card').focus();
            }
            save();
        }

        function totals() {
            var t = { ground: 0, attic: 0, decor: 0, count: { ground: {}, attic: {} } };
            state.items.forEach(function (i) {
                var p = P[i.k];
                if (!p.tb) { t.decor++; return; }
                var z = zoneOf(i);
                t[z] += p.tb;
                var ck = i.k + (i.l ? '|' + i.l : '');
                t.count[z][ck] = (t.count[z][ck] || 0) + 1;
            });
            t.total = t.ground * PRICE.ground + t.attic * PRICE.attic;
            return t;
        }
        function line(list, label, qty, amt, cls) {
            var li = document.createElement('li');
            if (cls) li.className = cls;
            var a = document.createElement('span'); a.textContent = label;
            var q = document.createElement('span'); q.textContent = qty;
            var d = document.createElement('span'); d.className = 'dots';
            var v = document.createElement('span'); v.textContent = amt;
            li.appendChild(a); li.appendChild(q); li.appendChild(d); li.appendChild(v);
            list.appendChild(li);
        }
        function subline(list, counts, extra) {
            var parts = [];
            ['bookcase', 'dresser', 'box'].forEach(function (k) {
                Object.keys(counts).forEach(function (ck) {
                    if (ck.split('|')[0] !== k) return;
                    var n = counts[ck], l = ck.split('|')[1];
                    parts.push(n + ' ' + (n === 1 ? P[k].name.toLowerCase() : P[k].plural) + (l ? ' of ' + LABEL[l] : ''));
                });
            });
            if (!parts.length) return;
            var li = document.createElement('li');
            li.className = 'sub';
            li.textContent = parts.join(', ') + (extra ? ' \u00b7 ' + extra : '');
            list.appendChild(li);
        }
        function photos(tb) {
            var n = tb * PHOTOS_PER_TB;
            if (n >= 1e6) return (Math.round(n / 1e5) / 10) + ' million';
            return n.toLocaleString('en-US');
        }
        function updateReceipt() {
            var t = totals(), list = document.getElementById('r-lines');
            while (list.firstChild) list.removeChild(list.firstChild);
            if (t.ground) { line(list, 'Downstairs', t.ground + ' TB', money(t.ground * PRICE.ground)); subline(list, t.count.ground); }
            if (t.attic) {
                line(list, 'Attic', t.attic + ' TB', money(t.attic * PRICE.attic));
                subline(list, t.count.attic, 'saves ' + money(t.attic * (PRICE.ground - PRICE.attic)) + '/mo vs downstairs');
            }
            if (t.decor) line(list, 'decor', t.decor + (t.decor === 1 ? ' pc' : ' pcs'), 'free', 'muted');
            if (!state.items.length) line(list, 'nothing yet', '', receipt.getAttribute('data-open') === '1' ? '5 GB free' : 'add a piece', 'muted');
            document.getElementById('r-total').textContent = money(t.total);
            var allTB = t.ground + t.attic;
            document.getElementById('r-save').textContent = allTB
                ? 'the same ' + allTB + ' TB on AWS S3: ' + money(allTB * PRICE.aws) + '/mo'
                : '';
            document.getElementById('r-warn').textContent = (t.ground && !t.attic)
                ? 'Tip: anything you rarely open can go up to the attic for $__PRICE_VAULT__/TB.'
                : '';
            var tb = t.ground + t.attic, meter = document.getElementById('b-meter');
            if (meter) {
                while (meter.firstChild) meter.removeChild(meter.firstChild);
                var b = document.createElement('b');
                b.textContent = tb + ' TB';
                meter.appendChild(b);
                meter.appendChild(document.createTextNode(tb
                    ? ' in your house, about ' + photos(tb) + ' photos'
                    : ' so far. Add a storage piece to start.'));
                var sm = document.createElement('small');
                sm.textContent = 'at roughly 4 MB a photo';
                if (tb) meter.appendChild(sm);
            }
        }
        function updateTray() {
            document.querySelectorAll('.piece-card').forEach(function (c) {
                c.setAttribute('aria-disabled', canAdd(c.getAttribute('data-add')) ? 'false' : 'true');
            });
        }

        // ---- tray: tap/click adds downstairs, mouse can drag straight onto either floor
        var suppressClick = false;
        document.querySelectorAll('.piece-card').forEach(function (card) {
            var k = card.getAttribute('data-add');
            card.addEventListener('click', function () {
                if (suppressClick) { suppressClick = false; return; }
                addItem(k);
            });
            card.addEventListener('pointerdown', function (e) {
                if (e.pointerType === 'touch' || e.button > 0 || !canAdd(k)) return;
                var sx = e.clientX, sy = e.clientY, ghost = null;
                function inside(ev) {
                    var r = room.getBoundingClientRect();
                    return ev.clientX >= r.left && ev.clientX <= r.right && ev.clientY >= r.top && ev.clientY <= r.bottom;
                }
                function mv(ev) {
                    if (!ghost && Math.abs(ev.clientX - sx) + Math.abs(ev.clientY - sy) > 6) {
                        ghost = document.createElement('div');
                        ghost.className = 'drag-ghost sticker';
                        ghost.appendChild(card.querySelector('svg').cloneNode(true));
                        document.body.appendChild(ghost);
                    }
                    if (ghost) {
                        ghost.style.left = ev.clientX + 'px';
                        ghost.style.top = ev.clientY + 'px';
                        roomBox.classList.toggle('drop-ok', inside(ev));
                    }
                }
                function up(ev) {
                    window.removeEventListener('pointermove', mv);
                    roomBox.classList.remove('drop-ok');
                    if (!ghost) return;
                    ghost.remove();
                    suppressClick = true;
                    setTimeout(function () { suppressClick = false; }, 0);
                    if (inside(ev)) {
                        var pt = svgPoint(room, ev.clientX, ev.clientY);
                        addItem(k, pt.x - P[k].w / 2, pt.y - P[k].h * 0.85);
                    }
                }
                window.addEventListener('pointermove', mv);
                window.addEventListener('pointerup', up, { once: true });
            });
        });

        // tier cards: the sprite is a sticker you can drop into your house
        document.querySelectorAll('.grab-piece[data-add]').forEach(function (b) {
            b.addEventListener('click', function () {
                var k = b.getAttribute('data-add'), zone = b.getAttribute('data-zone') || 'ground';
                document.getElementById('build').scrollIntoView({ behavior: 'smooth', block: 'start' });
                setTimeout(function () { addItem(k, undefined, undefined, zone); }, 550);
            });
        });

        document.querySelectorAll('[data-wall]').forEach(function (b) {
            b.addEventListener('click', function () { state.wall = b.getAttribute('data-wall'); applyVibe(); save(); });
        });
        document.querySelectorAll('[data-fit]').forEach(function (b) {
            b.addEventListener('click', function () { state.fit = b.getAttribute('data-fit'); applyVibe(); save(); });
        });
        document.getElementById('b-reset').addEventListener('click', function () {
            state.items = starter();
            render();
            announce('House reset.');
            save();
        });
        nameInput.value = state.name;
        nameInput.addEventListener('input', function () {
            var c = cleanName(nameInput.value);
            if (c !== nameInput.value) nameInput.value = c;
            state.name = c;
            renderSign();
            save();
        });

        function shareURL() { return window.location.origin + window.location.pathname + '#room=' + encode(); }
        document.getElementById('b-share').addEventListener('click', function () {
            var url = shareURL();
            if (navigator.share) {
                navigator.share({ title: signText() + ' on stored.ge', url: url }).catch(function () { /* dismissed */ });
                return;
            }
            if (navigator.clipboard) {
                navigator.clipboard.writeText(url).then(function () { toast('link copied. go post it'); }, function () { toast('copy failed'); });
            }
        });

        // ---- save as image: rebuild the house as a standalone SVG with its live
        // (day or night) colours, rasterise it, then letter the sign, floor labels,
        // TB tags and caption with the page's loaded fonts.
        function bgMarkup(wa, wb) {
            var src = document.getElementById('room-bg'), copy = src.cloneNode(true);
            var from = src.querySelectorAll('*'), to = copy.querySelectorAll('*');
            for (var i = 0; i < from.length; i++) {
                if (from[i].tagName.toLowerCase() === 'text') continue;
                var cs = getComputedStyle(from[i]), f = from[i].getAttribute('fill');
                if (f) to[i].setAttribute('fill', f === 'url(#wallgrad)' ? 'url(#wg)' : (f.indexOf('url(') === 0 ? f : cs.fill));
                to[i].setAttribute('opacity', cs.opacity);
                to[i].removeAttribute('class');
            }
            return '<defs><linearGradient id="wg" x1="0" y1="0" x2="0" y2="1">' +
                '<stop offset="0" style="stop-color:' + wa + '"/><stop offset="1" style="stop-color:' + wb + '"/></linearGradient></defs>' +
                copy.innerHTML.replace(/<text[\s\S]*?<\/text>/g, '');
        }
        document.getElementById('b-save').addEventListener('click', function () {
            var cs = getComputedStyle(document.documentElement);
            var wa = cs.getPropertyValue('--wall-a').trim(), wb = cs.getPropertyValue('--wall-b').trim();
            var deep = cs.getPropertyValue('--wall-deep').trim(), fit = cs.getPropertyValue('--fit').trim();
            var night = document.documentElement.classList.contains('is-dark');
            var need = {}, body = '', lights = '', shadows = '', ordered = order(), lampCache = {};
            ordered.forEach(function (i) {
                var p = P[i.k], sym = p.alt || i.k, zone = zoneOf(i);
                if (!lampCache[zone]) { var lm = lampsOn(zone); lampCache[zone] = { lamps: lm, lights: lightsFor(zone, night, lm) }; }
                if (!p.flat && !p.wall) {
                    var sh = shadowShape(i.x + p.w / 2, i.y + p.h, p.w, lampCache[zone].lights, night);
                    shadows += '<ellipse cx="' + sh.cx + '" cy="' + sh.cy + '" rx="' + sh.rx + '" ry="' + sh.ry + '" fill="#000" fill-opacity="' + sh.o + '" filter="url(#soft)"/>';
                }
                need[sym] = 1;
                body += '<g transform="translate(' + i.x + ' ' + i.y + ')" color="' + (p.deep ? deep : fit) + '"' +
                    (night && !p.glow ? ' filter="' + tintFor(pieceLight(i, night, lampCache[zone].lamps)) + '"' : '') + '>' +
                    '<use href="#s-' + sym + '" width="' + p.w + '" height="' + p.h + '"/></g>';
                if (night && p.glow) {
                    lights += '<g transform="translate(' + i.x + ' ' + i.y + ')" style="mix-blend-mode:screen">' +
                        '<ellipse cx="' + p.glow[0] + '" cy="' + (p.glow[1] + 10) + '" rx="' + (p.glow[2] * 6) + '" ry="' + (p.glow[3] * 4.6) + '" fill="url(#lampfill)"/>' +
                        '<ellipse cx="' + p.glow[0] + '" cy="' + (p.glow[1] + 5) + '" rx="' + (p.glow[2] * 3.6) + '" ry="' + (p.glow[3] * 3.1) + '" fill="url(#lampambient)"/>' +
                        '<ellipse cx="' + p.glow[0] + '" cy="' + p.glow[1] + '" rx="' + p.glow[2] + '" ry="' + p.glow[3] + '" fill="url(#lampglow)"/></g>';
                }
            });
            body = shadows + body + lights;
            var defs = '';
            ['soft', 'vig'].concat(night ? ['lampglow', 'lampambient', 'lampfill', 'nightdim', 'nightmid', 'nightwarm'] : []).forEach(function (id) {
                defs += document.getElementById(id).outerHTML;
            });
            Object.keys(need).forEach(function (id) { defs += document.getElementById('s-' + id).outerHTML; });
            var S = 8, CW = W * S, CH = H * S, BAND = 124;
            var svg = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ' + W + ' ' + H + '" width="' + CW + '" height="' + CH + '" shape-rendering="crispEdges">' +
                '<defs>' + defs + '</defs>' + bgMarkup(wa, wb) + body + '</svg>';
            var img = new Image();
            img.onload = function () {
                var c = document.createElement('canvas');
                c.width = CW; c.height = CH + BAND;
                var x = c.getContext('2d');
                x.fillStyle = '#fffaf0';
                x.fillRect(0, 0, CW, CH + BAND);
                x.imageSmoothingEnabled = false;
                x.drawImage(img, 0, 0, CW, CH);
                x.textAlign = 'center';
                x.fillStyle = night ? '#a39c8c' : '#fdf3dc';
                x.font = '40px Silkscreen';
                x.fillText(signText(), 41 * S, 67.6 * S, 38 * S);
                x.textAlign = 'right';
                x.font = '32px Silkscreen';
                x.fillStyle = '#e8dcc8';
                x.fillText('ATTIC  $__PRICE_VAULT__/TB', 124 * S, 7.4 * S);
                x.fillStyle = night ? '#e8dcc8' : '#5a3418';
                x.fillText('DOWNSTAIRS  $__PRICE_STD__/TB', 124 * S, 112.6 * S);
                x.textAlign = 'center';
                ordered.forEach(function (i) {
                    var p = P[i.k];
                    if (!p.tb) return;
                    var t = tagText(i), tw = t.length * 3 + 3, tx = i.x + Math.round(p.w / 2 - tw / 2), ty = i.y - 6;
                    x.fillStyle = ZONE[zoneOf(i)].tag;
                    x.fillRect(tx * S, ty * S, tw * S, 5 * S);
                    x.fillStyle = '#1a1a1a';
                    x.font = '28px Silkscreen';
                    x.fillText(t, (tx + tw / 2) * S, (ty + 4) * S);
                });
                var t = totals(), tb = t.ground + t.attic;
                x.textAlign = 'left';
                x.fillStyle = '#2b2b2b';
                x.font = '800 38px Montserrat';
                x.fillText(signText(), 36, CH + 60);
                x.font = '20px Silkscreen';
                x.fillStyle = '#6b6b6b';
                x.fillText(tb + ' TB · ' + money(t.total) + '/mo · built on stored.ge', 36, CH + 96);
                x.textAlign = 'right';
                x.fillStyle = '#2b2b2b';
                x.font = '800 30px Montserrat';
                x.fillText('STORED.GE', CW - 36, CH + 76);
                c.toBlob(function (blob) {
                    if (!blob) { toast('could not save image'); return; }
                    var a = document.createElement('a');
                    a.href = URL.createObjectURL(blob);
                    a.download = 'my-stored-house.png';
                    document.body.appendChild(a);
                    a.click();
                    setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
                    toast('saved. go post it');
                });
            };
            img.onerror = function () { toast('could not save image'); };
            img.src = 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg);
        });

        // ---- the why strip: tap what you'd hate to lose, it packs into a labelled
        // piece, then a button moves the packed pieces into the house on either floor
        (function () {
            var packed = [], area = document.getElementById('packed'), lineEl = document.getElementById('packed-line');
            var down = document.getElementById('pack-down'), up = document.getElementById('pack-up');
            if (!area || !down || !up) return;
            var chips = document.querySelectorAll('.chip[data-pack]');
            function svgFor(k) {
                var p = P[k], svg = el('svg', { 'class': 'px sticker', viewBox: '0 0 ' + p.w + ' ' + p.h, 'aria-hidden': 'true', focusable: 'false' });
                svg.style.setProperty('--w', p.w); svg.style.setProperty('--h', p.h);
                svg.appendChild(el('use', { href: '#s-' + k }));
                return svg;
            }
            function draw() {
                while (area.firstChild) area.removeChild(area.firstChild);
                var tb = 0;
                if (!packed.length) {
                    var e = document.createElement('span');
                    e.className = 'packed-empty';
                    e.textContent = 'nothing packed yet';
                    area.appendChild(e);
                }
                packed.forEach(function (l) {
                    var k = PACK[l], b = document.createElement('button');
                    b.type = 'button';
                    b.className = 'packed-item';
                    b.setAttribute('aria-label', 'Unpack ' + LABEL[l]);
                    b.appendChild(svgFor(k));
                    var t = document.createElement('span');
                    t.className = 'tag';
                    t.textContent = LABEL[l] + ' ' + P[k].tb + ' TB';
                    b.appendChild(t);
                    b.addEventListener('click', function () { toggle(l); });
                    area.appendChild(b);
                    tb += P[k].tb;
                });
                chips.forEach(function (c) { c.setAttribute('aria-pressed', packed.indexOf(c.getAttribute('data-pack')) >= 0 ? 'true' : 'false'); });
                down.disabled = up.disabled = !packed.length;
                lineEl.textContent = tb
                    ? tb + ' TB packed: ' + money(tb * PRICE.ground) + '/mo downstairs, or ' + money(tb * PRICE.attic) + '/mo in the attic.'
                    : 'Each box is real space at a flat price. Bigger holds more.';
            }
            function toggle(l) {
                var i = packed.indexOf(l);
                if (i >= 0) packed.splice(i, 1); else packed.push(l);
                draw();
            }
            chips.forEach(function (c) { c.addEventListener('click', function () { toggle(c.getAttribute('data-pack')); }); });
            function moveIn(zone) {
                var n = 0, last = null;
                packed.forEach(function (l) {
                    if (addItem(PACK[l], undefined, undefined, zone, l, true)) { n++; last = state.items[state.items.length - 1]; }
                });
                if (!n) return;
                packed = [];
                draw();
                render(null, last);
                save();
                var t = totals();
                announce(n + (n === 1 ? ' piece' : ' pieces') + ' moved ' + (zone === 'attic' ? 'to the attic' : 'downstairs') + '. Total ' + money(t.total) + ' a month.');
                toast(n + (n === 1 ? ' piece' : ' pieces') + ' moved ' + (zone === 'attic' ? 'up to the attic' : 'in downstairs'));
                document.querySelector('.b-room').scrollIntoView({ behavior: 'smooth', block: 'center' });
            }
            down.addEventListener('click', function () { moveIn('ground'); });
            up.addEventListener('click', function () { moveIn('attic'); });
            draw();
        })();

        renderSign();
        render();
        if (fromLink) {
            setTimeout(function () { document.getElementById('build').scrollIntoView({ block: 'start' }); }, 50);
        }
    })();

    // ---- light/dark toggle: follows the system until the visitor picks one;
    // the pick is shared with /docs and /changelog via localStorage sg-theme.
    (function () {
        var root = document.documentElement, mq = window.matchMedia('(prefers-color-scheme: dark)');
        function dark() {
            var t = root.getAttribute('data-theme');
            return t ? t === 'dark' : mq.matches;
        }
        function sync() {
            var d = dark();
            root.classList.toggle('is-dark', d);
            document.querySelectorAll('.theme-toggle').forEach(function (b) {
                b.setAttribute('aria-label', d ? 'Switch to light mode' : 'Switch to dark mode');
            });
            document.querySelectorAll('meta[name="theme-color"]').forEach(function (m) {
                m.setAttribute('content', d ? '#101010' : '#383838');
            });
            document.dispatchEvent(new Event('sg-theme'));
        }
        document.querySelectorAll('.theme-toggle').forEach(function (b) {
            b.addEventListener('click', function () {
                var next = dark() ? 'light' : 'dark';
                root.setAttribute('data-theme', next);
                try { window.localStorage.setItem('sg-theme', next); } catch (e) { /* private mode */ }
                sync();
            });
        });
        if (mq.addEventListener) mq.addEventListener('change', sync);
        sync();
    })();
