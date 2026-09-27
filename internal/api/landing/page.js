
    {{if not .SignupsOpen}}
    // Waitlist forms post to the existing /api/waitlist endpoint, then swap to
    // an inline confirmation. Both hero and footer forms share this handler.
    document.querySelectorAll('[data-waitlist]').forEach(function (form) {
        form.addEventListener('submit', async function (event) {
            event.preventDefault();
            var email = form.querySelector('input[name=email]').value;
            try {
                await fetch('/api/waitlist', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ email: email })
                });
            } catch (e) { /* endpoint degrades to 200; confirmation is best-effort */ }
            form.innerHTML = '<span class="waitlist-ok">You\'re on the list — see you October 31.</span>';
        });
    });
    {{end}}

    document.querySelectorAll('[data-copy]').forEach(function (btn) {
        btn.addEventListener('click', function () {
            var target = document.getElementById(btn.getAttribute('data-copy'));
            navigator.clipboard.writeText(target.innerText).then(function () {
                btn.textContent = 'copied';
                setTimeout(function () { btn.textContent = 'copy'; }, 1500);
            });
        });
    });

    // ---- 02: tiering ring. Illustrative 25 TB workload; segments animate as
    // the age slider moves, the centre price deliberately never changes.
    (function () {
        var slider = document.getElementById('age-slider');
        if (!slider) return;
        var TOTAL_TB = 25, R = 78, GAP = 3;
        var C = 2 * Math.PI * R;
        // keyframes: [day, hot%, optimized%, tape%]
        // keyframes: [day, hot%, restored%, tape%] — idle ≥14d demotes; ~15% hot at steady state
        var KF = [[0, 100, 0, 0], [14, 100, 0, 0], [30, 45, 3, 52], [60, 24, 4, 72], [120, 15, 5, 80]];
        var segs = {
            hot:  document.getElementById('seg-hot'),
            warm: document.getElementById('seg-warm'),
            tape: document.getElementById('seg-tape')
        };
        function mix(day) {
            var a = KF[0], b = KF[KF.length - 1], i;
            for (i = 0; i < KF.length - 1; i++) {
                if (day >= KF[i][0] && day <= KF[i + 1][0]) { a = KF[i]; b = KF[i + 1]; break; }
            }
            var t = (b[0] === a[0]) ? 0 : (day - a[0]) / (b[0] - a[0]);
            return [1, 2, 3].map(function (k) { return a[k] + (b[k] - a[k]) * t; });
        }
        function setSeg(el, frac, offsetFrac) {
            var len = Math.max(0, C * frac - (frac > 0.004 ? GAP : 0));
            el.style.strokeDasharray = len + ' ' + C;
            el.style.strokeDashoffset = String(-C * offsetFrac);
        }
        function fmtTB(frac) {
            var tb = TOTAL_TB * frac;
            return (tb >= 10 ? tb.toFixed(0) : tb.toFixed(1)) + ' TB';
        }
        function render(day) {
            var p = mix(day);
            var hot = p[0] / 100, warm = p[1] / 100, tape = p[2] / 100;
            setSeg(segs.hot, hot, 0);
            setSeg(segs.warm, warm, hot);
            setSeg(segs.tape, tape, hot + warm);
            document.getElementById('age-out').textContent = String(day);
            document.getElementById('val-hot').textContent = fmtTB(hot);
            document.getElementById('val-warm').textContent = fmtTB(warm);
            document.getElementById('val-tape').textContent = fmtTB(tape);
            document.getElementById('bar-hot').style.width = p[0] + '%';
            document.getElementById('bar-warm').style.width = p[1] + '%';
            document.getElementById('bar-tape').style.width = p[2] + '%';
            var idx = day < 14 ? 0 : (day < 60 ? 1 : 2);
            [0, 1, 2].forEach(function (i) {
                document.getElementById('stage-' + i).classList.toggle('active', i === idx);
            });
        }
        slider.addEventListener('input', function () { render(Number(slider.value)); });
        document.querySelectorAll('.scrub-presets button').forEach(function (btn) {
            btn.addEventListener('click', function () {
                slider.value = btn.getAttribute('data-day');
                render(Number(slider.value));
            });
        });
        render(Number(slider.value));
    })();

    // ---- 03: use-case tabs
    document.querySelectorAll('.uc-tab').forEach(function (tab) {
        tab.addEventListener('click', function () {
            document.querySelectorAll('.uc-tab').forEach(function (t) {
                t.classList.toggle('active', t === tab);
                t.setAttribute('aria-selected', t === tab ? 'true' : 'false');
            });
            var key = tab.getAttribute('data-uc');
            document.querySelectorAll('.uc-panel').forEach(function (p) {
                p.classList.toggle('active', p.getAttribute('data-panel') === key);
            });
        });
    });

    // ---- 04: resource chips
    document.querySelectorAll('.res-chip').forEach(function (chip) {
        chip.addEventListener('click', function () {
            document.querySelectorAll('.res-chip').forEach(function (c) {
                c.classList.toggle('active', c === chip);
            });
            var key = chip.getAttribute('data-res');
            document.querySelectorAll('.res-card').forEach(function (card) {
                card.classList.toggle('active', card.getAttribute('data-card') === key);
            });
        });
    });

    // ---- 05: pricing slider. Rates from the comparison table below it.
    (function () {
        var slider = document.getElementById('calc-tb');
        if (!slider) return;
        function money(n) {
            return '$' + n.toFixed(2).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
        }
        function moneyWhole(n) {
            return '$' + Math.round(n).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ',');
        }
        function render() {
            var tb = Number(slider.value);
            document.getElementById('calc-out').textContent = tb + ' TB';
            document.getElementById('calc-us').textContent = money(tb * __PRICE_STD__) + '/mo';
            document.getElementById('calc-b2').textContent = money(tb * __PRICE_B2__) + '/mo';
            document.getElementById('calc-aws').textContent = money(tb * __PRICE_AWS__) + '/mo';
            var saved = moneyWhole((__PRICE_AWS__ - __PRICE_STD__) * tb * 12);
            var note = 'That’s <strong>' + saved + '/yr</strong> kept out of AWS’s pocket.';
            note += ' Archive-only? <strong>Vault</strong> holds ' + tb +
                ' TB on tape for <strong>' + money(tb * __PRICE_VAULT_2DP__) + '/mo</strong> billed yearly.';
            document.getElementById('calc-note').innerHTML = note;
        }
        slider.addEventListener('input', render);
        render();
    })();
