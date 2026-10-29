    // ---- scenes + 02: pick your space. One box is one terabyte: cardboard
    // boxes are Standard ($__PRICE_STD__/TB), steel crates are Vault
    // ($__PRICE_VAULT__/TB). Every room on the page is a scene: pick things up,
    // put them down; they fall, stack, cast shadows and take the lamp light.
    (function () {
        var NS = 'http://www.w3.org/2000/svg';
        var PRICE = __PRICES_JSON__;
        var PHOTOS_PER_TB = 250000; // ~4 MB a photo
        var MAX_TB = 1000;

        function clamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }
        function money(n) { return '$' + n.toFixed(2).replace(/\B(?=(\d{3})+(?!\d))/g, ','); }

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

        // Drag any .piece around its SVG, snapped to the pixel grid.
        function makeDraggable(svg, g, pos, w, h, vb, opts) {
            opts = opts || {};
            g.addEventListener('pointerdown', function (e) {
                if (e.button > 0) return;
                e.preventDefault();
                var id = e.pointerId, start = svgPoint(svg, e.clientX, e.clientY), ox = pos.x, oy = pos.y, moved = false, done = false;
                g.classList.add('lift');
                if (opts.start) opts.start();
                // Raise the piece BEFORE taking the pointer: moving a node in the
                // DOM releases any capture it holds, which left pieces stuck "in
                // hand" after a click. The move and the drop are read on the window,
                // so a drop is never missed even where capture is not granted.
                g.parentNode.appendChild(g);
                try { g.setPointerCapture(id); } catch (err) { /* older engines */ }
                if (g.focus) { try { g.focus({ preventScroll: true }); } catch (err) { /* no focus on svg */ } }
                function mv(ev) {
                    if (ev.pointerId !== id) return;
                    var p = svgPoint(svg, ev.clientX, ev.clientY);
                    var nx = Math.round(ox + p.x - start.x), ny = Math.round(oy + p.y - start.y);
                    if (nx !== ox || ny !== oy) moved = true;
                    pos.x = clamp(nx, 0, vb[0] - w);
                    pos.y = clamp(ny, 0, vb[1] - h);
                    g.setAttribute('transform', 'translate(' + pos.x + ' ' + pos.y + ')');
                    if (opts.follow) opts.follow();
                }
                function up(ev) {
                    if (done || (ev && ev.pointerId !== undefined && ev.pointerId !== id)) return;
                    done = true;
                    window.removeEventListener('pointermove', mv);
                    window.removeEventListener('pointerup', up);
                    window.removeEventListener('pointercancel', up);
                    g.removeEventListener('lostpointercapture', up);
                    try { g.releasePointerCapture(id); } catch (err) { /* not captured */ }
                    g.classList.remove('lift');
                    if (moved && navigator.vibrate) { try { navigator.vibrate(6); } catch (err) { /* ignore */ } }
                    if (opts.end) opts.end(moved);
                }
                window.addEventListener('pointermove', mv);
                window.addEventListener('pointerup', up);
                window.addEventListener('pointercancel', up);
                g.addEventListener('lostpointercapture', up);
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

        // A scene: every .piece in the svg can be picked up and put down. It
        // has a floor and things stand on things: a piece dropped in mid-air
        // falls to the floor or onto whatever is under it (a box on a box, the
        // lamp on a box); take the box away and what stood on it falls too. A
        // tap just bounces the piece. add/remove let the picker swap its boxes.
        function playScene(svg) {
            var vb = svg.getAttribute('viewBox').split(' ').map(Number).slice(2), entries = [];
            var shadows = el('g', { 'class': 'scene-shadows', 'aria-hidden': 'true' });
            var first = svg.querySelector('.piece');
            if (first) first.parentNode.insertBefore(shadows, first);
            function relight() {
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
            var floor = 0, REST = 2; // pieces sit 2 px into their support, like the art
            function overlapX(a, b) { return Math.min(a.pos.x + a.w, b.pos.x + b.w) - Math.max(a.pos.x, b.pos.x); }
            function restY(e) {
                var y = floor - e.h;
                entries.forEach(function (o) {
                    // only something at least as wide can hold a piece (a box on
                    // a box, not a box on a lamp), and it must fit above it
                    if (o === e || o.lifted || o.w < e.w || overlapX(e, o) < e.w * 0.5) return;
                    var top = o.pos.y - e.h + REST;
                    if (top < 0 || top < e.pos.y - 8) return; // the support is above the piece, not under it
                    if (top < y) y = top;
                });
                return y;
            }
            function settle(e) {
                var y = restY(e);
                if (Math.abs(y - e.pos.y) <= 3) return false;
                e.pos.y = y;
                e.g.classList.add('settle');
                setTimeout(function () { e.g.classList.remove('settle'); }, 260);
                e.g.setAttribute('transform', 'translate(' + e.pos.x + ' ' + e.pos.y + ')');
                return true;
            }
            function bounce(e) {
                e.g.classList.remove('pop');
                void e.g.getBoundingClientRect();
                e.g.classList.add('pop');
            }
            function settleAll(except) {
                for (var pass = 0; pass < 6; pass++) {
                    var fell = false;
                    entries.forEach(function (o) { if (o !== except && o.grounded && settle(o)) fell = true; });
                    if (!fell) break;
                }
            }
            function land(e, moved) {
                e.lifted = false;
                if (moved) {
                    settle(e);
                    e.grounded = true;
                    settleAll(e);
                }
                bounce(e);
                relight();
            }
            function add(g) {
                var m = /translate\((-?\d+) (-?\d+)\)/.exec(g.getAttribute('transform') || '');
                var hit = g.querySelector('.hit');
                if (!m || !hit) return;
                var pos = { x: Number(m[1]), y: Number(m[2]) }, w = Number(hit.getAttribute('width')), h = Number(hit.getAttribute('height'));
                var glows = g.querySelectorAll('ellipse.glow'), c = glows.length ? glows[glows.length - 1] : null;
                var e = { g: g, pos: pos, w: w, h: h, shadow: shadowNode(), core: c ? { cx: +c.getAttribute('cx'), cy: +c.getAttribute('cy'), rx: +c.getAttribute('rx'), ry: +c.getAttribute('ry') } : null };
                entries.push(e);
                shadows.appendChild(e.shadow);
                makeDraggable(svg, g, pos, w, h, vb, { start: function () { e.lifted = true; }, follow: relight, end: function (moved) { land(e, moved); } });
                return e;
            }
            svg.querySelectorAll('.piece').forEach(add);
            // the floor is where the lowest piece stands; a piece that already
            // rests on it (or on another piece) takes part in the falling
            entries.forEach(function (e) { floor = Math.max(floor, e.pos.y + e.h); });
            entries.forEach(function (e) { e.grounded = Math.abs(restY(e) - e.pos.y) <= 3; });
            document.addEventListener('sg-theme', relight);
            relight();
            return {
                // new pieces are already placed where they rest
                add: function (g) { var e = add(g); if (e) e.grounded = true; },
                remove: function (g) {
                    entries = entries.filter(function (e) {
                        if (e.g !== g) return true;
                        e.shadow.remove();
                        return false;
                    });
                    g.remove();
                },
                settle: function () { settleAll(null); relight(); }
            };
        }

        var scenes = {};
        document.querySelectorAll('svg[data-play]').forEach(function (svg, i) { scenes[svg.id || i] = playScene(svg); });

        // ---- 02: the box picker
        var pick = document.getElementById('pick-scene');
        if (!pick) return;
        var scene = scenes['pick-scene'];
        var boxesLayer = document.getElementById('pick-boxes');
        var tagsLayer = document.getElementById('pick-tags');
        var receipt = document.getElementById('receipt');
        var live = document.getElementById('b-live');
        var inputs = { std: document.getElementById('pick-std'), vault: document.getElementById('pick-vault') };
        // two piles of up to 3 x 4 boxes, standing on the floor (y + 15 = 60)
        var PILE = {
            std:   { xs: [3, 20, 37], sprite: 'box', tag: '#ffd400', name: 'Standard' },
            vault: { xs: [106, 123, 140], sprite: 'crate', tag: '#b9c7d2', name: 'Vault' }
        };
        var ROWS = [45, 32, 19, 6], SHOWN = 12;
        var state = { std: Number(inputs.std.value) || 0, vault: Number(inputs.vault.value) || 0, monthly: false, touched: false };

        function photos(tb) {
            var n = tb * PHOTOS_PER_TB;
            if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + ' million';
            return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
        }
        function lineCost(kind, tb) {
            if (!tb) return 0;
            if (kind === 'std') return tb * (state.monthly ? PRICE.std_monthly : PRICE.std);
            return state.monthly ? Math.max(tb * PRICE.vault_monthly, PRICE.vault_min) : tb * PRICE.vault;
        }

        function drawPile(kind) {
            var p = PILE[kind], tb = state[kind], n = Math.min(tb, SHOWN), top = 60;
            for (var i = 0; i < n; i++) {
                var x = p.xs[i % 3], y = ROWS[Math.floor(i / 3)];
                var g = el('g', { 'class': 'piece pick-box', transform: 'translate(' + x + ' ' + y + ')', 'data-kind': kind });
                g.appendChild(el('rect', { 'class': 'hit', width: 17, height: 15 }));
                g.appendChild(el('use', { href: '#s-' + p.sprite, width: 17, height: 15 }));
                boxesLayer.appendChild(g);
                scene.add(g);
                top = Math.min(top, y);
            }
            if (!tb) return;
            var text = p.name.toUpperCase() + ' ' + tb + ' TB' + (tb > SHOWN ? ' (' + SHOWN + ' SHOWN)' : '');
            // centred over the columns the pile uses (a box is 14 px of front + 3 of depth)
            var cols = Math.min(n, 3), tw = text.length * 3 + 3, cx = p.xs[0] + (cols * 17 - 3) / 2, tx = clamp(Math.round(cx - tw / 2), 1, 159 - tw);
            var t = el('g', { 'class': 'tb-tag', transform: 'translate(' + tx + ' ' + (top - 7) + ')' });
            t.appendChild(el('rect', { width: tw, height: 5, fill: p.tag }));
            var label = el('text', { x: tw / 2, y: 4, 'text-anchor': 'middle', 'font-family': 'Silkscreen, monospace', 'font-size': 4, fill: '#1a1a1a' });
            label.textContent = text;
            t.appendChild(label);
            tagsLayer.appendChild(t);
        }

        function render() {
            boxesLayer.querySelectorAll('.pick-box').forEach(function (g) { scene.remove(g); });
            while (tagsLayer.firstChild) tagsLayer.removeChild(tagsLayer.firstChild);
            drawPile('std');
            drawPile('vault');
            scene.settle();

            var all = state.std + state.vault, total = lineCost('std', state.std) + lineCost('vault', state.vault);
            var lines = '';
            ['std', 'vault'].forEach(function (k) {
                if (!state[k]) return;
                lines += '<li><span>' + PILE[k].name + '</span><span>' + state[k] + ' TB</span><span class="dots"></span><span>' + money(lineCost(k, state[k])) + '</span></li>';
                if (k === 'vault' && state.monthly && state.vault * PRICE.vault_monthly < PRICE.vault_min) {
                    lines += '<li class="sub">Vault monthly minimum ' + money(PRICE.vault_min) + '</li>';
                }
            });
            document.getElementById('r-lines').innerHTML = lines || '<li class="muted">add a box to start</li>';
            document.getElementById('r-total').textContent = money(total);
            document.getElementById('r-period').textContent = 'total / mo, billed ' + (state.monthly ? 'monthly' : 'yearly');
            document.getElementById('r-save').textContent = all ? 'the same ' + all + ' TB on AWS S3: ' + money(all * PRICE.aws) + '/mo' : '';
            document.getElementById('b-meter').innerHTML = all
                ? '<b>' + all + ' TB</b> holds about ' + photos(all) + ' photos<small>at roughly 4 MB a photo</small>'
                : '<b>0 TB</b> pick a box or two<small>one box is one terabyte</small>';
            ['std', 'vault'].forEach(function (k) {
                if (Number(inputs[k].value) !== state[k] && document.activeElement !== inputs[k]) inputs[k].value = state[k];
                document.querySelectorAll('[data-step="' + k + '"][data-by="-1"]').forEach(function (b) { b.disabled = state[k] <= 0; });
            });
            // the plan rides into sign-up once the visitor has made it theirs
            var cta = receipt.querySelector('.r-actions a.btn-primary');
            if (cta && receipt.getAttribute('data-open') === '1') {
                cta.setAttribute('href', state.touched ? '/register?std_tb=' + state.std + '&vault_tb=' + state.vault : '/register');
            }
            if (live && state.touched) live.textContent = 'Standard ' + state.std + ' TB, Vault ' + state.vault + ' TB, ' + money(total) + ' a month';
        }

        function set(kind, tb) {
            tb = clamp(Math.floor(tb) || 0, 0, MAX_TB);
            if (tb === state[kind]) return;
            state[kind] = tb;
            state.touched = true;
            if (window.sgPing) window.sgPing('builder.edit');
            render();
        }
        document.querySelectorAll('[data-step]').forEach(function (b) {
            b.addEventListener('click', function () {
                var k = b.getAttribute('data-step');
                set(k, state[k] + Number(b.getAttribute('data-by')));
            });
        });
        Object.keys(inputs).forEach(function (k) {
            inputs[k].addEventListener('input', function () { if (inputs[k].value !== '') set(k, Number(inputs[k].value)); });
            inputs[k].addEventListener('change', function () { set(k, Number(inputs[k].value)); inputs[k].value = state[k]; });
        });
        document.querySelectorAll('input[name="pick-period"]').forEach(function (r) {
            r.addEventListener('change', function () { state.monthly = r.value === 'monthly' && r.checked; render(); });
        });

        // what the waitlist form sends along: the TB per tier, once chosen
        window.sgHouse = function () {
            return state.touched ? { std: state.std, vault: state.vault, room: '' } : { std: 0, vault: 0, room: '' };
        };
        render();
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
