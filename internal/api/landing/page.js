
    // Where this visit came from: the landing URL's utm_* labels and the
    // referring site (host only). Sent with the waitlist e-mail and appended
    // to /register links so a sign-up can be attributed to the post that
    // produced it. No cookie, no storage, the values live on this page only.
    function attribution() {
        var out = {};
        try {
            var q = new URLSearchParams(window.location.search);
            ['utm_source', 'utm_medium', 'utm_campaign'].forEach(function (k) { var v = q.get(k); if (v) out[k] = v.slice(0, 100); });
            if (document.referrer) { var u = new URL(document.referrer); if (u.hostname && u.hostname !== window.location.hostname) out.referrer = u.hostname; }
        } catch (e) { /* attribution is a hint */ }
        return out;
    }
    // ---- cookieless site statistics (POST /api/ping): one named event, no
    // identifier, no storage. Nothing is sent when the page is not served over
    // http(s) (the browser harness runs it from a file).
    var pinged = {};
    function ping(name, once) {
        if (once && pinged[name]) return;
        pinged[name] = true;
        try {
            if (window.location.protocol !== 'http:' && window.location.protocol !== 'https:') return;
            var a = attribution();
            var body = JSON.stringify({ event: name, referrer: a.referrer || '', utm_source: a.utm_source || '', utm_medium: a.utm_medium || '', utm_campaign: a.utm_campaign || '' });
            if (navigator.sendBeacon) { navigator.sendBeacon('/api/ping', new Blob([body], { type: 'application/json' })); return; }
            fetch('/api/ping', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: body, keepalive: true }).catch(function () { /* best-effort */ });
        } catch (e) { /* statistics never break the page */ }
    }
    window.sgPing = function (name) { ping(name, true); };
    (function () {
        if (!('IntersectionObserver' in window)) return;
        var reached = { build: 'build.view', pricing: 'pricing.view' };
        var io = new IntersectionObserver(function (entries) {
            entries.forEach(function (en) {
                if (en.isIntersecting && reached[en.target.id]) { ping(reached[en.target.id], true); io.unobserve(en.target); }
            });
        }, { threshold: 0.25 });
        Object.keys(reached).forEach(function (id) { var el = document.getElementById(id); if (el) io.observe(el); });
    })();
    document.querySelectorAll('a[href^="/register"]').forEach(function (a) {
        a.addEventListener('click', function () { ping('cta.register'); });
    });

    (function () {
        var a = attribution();
        if (!a.utm_source && !a.utm_medium && !a.utm_campaign && !a.referrer) return;
        document.querySelectorAll('a.btn').forEach(function (link) {
            if (link.getAttribute('href') !== '/register') return;
            var p = new URLSearchParams();
            if (a.utm_source) p.set('utm_source', a.utm_source);
            if (a.utm_medium) p.set('utm_medium', a.utm_medium);
            if (a.utm_campaign) p.set('utm_campaign', a.utm_campaign);
            if (a.referrer) p.set('ref', a.referrer);
            link.setAttribute('href', '/register?' + p.toString());
        });
    })();

    {{if not .SignupsOpen}}
    // Waitlist forms post to the existing /api/waitlist endpoint, then swap to
    // an inline confirmation. Both hero and footer forms share this handler.
    // the TB the visitor picked (if the picker is on the page) ride along
    function withHouse(payload) {
        var a = attribution();
        Object.keys(a).forEach(function (k) { payload[k] = a[k]; });
        if (typeof window.sgHouse !== 'function') return payload;
        var h = window.sgHouse();
        if (h.std || h.vault) { payload.std_tb = h.std; payload.vault_tb = h.vault; payload.room = h.room; }
        return payload;
    }
    document.querySelectorAll('[data-waitlist]').forEach(function (form) {
        form.addEventListener('submit', async function (event) {
            event.preventDefault();
            ping('waitlist.submit', true);
            var email = form.querySelector('input[name=email]').value;
            try {
                await fetch('/api/waitlist', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(withHouse({ email: email }))
                });
            } catch (e) { /* endpoint degrades to 200; confirmation is best-effort */ }
            form.innerHTML = '<span class="waitlist-ok">You\'re on the list, see you October 31.</span>';
        });
    });
    {{end}}

    document.querySelectorAll('[data-copy]').forEach(function (btn) {
        btn.addEventListener('click', function () {
            var target = document.getElementById(btn.getAttribute('data-copy'));
            ping('copy');
            navigator.clipboard.writeText(target.innerText).then(function () {
                btn.textContent = 'copied';
                setTimeout(function () { btn.textContent = 'copy'; }, 1500);
            });
        });
    });

    // ---- 03: tools tabs (the beacon counts them under their old res.* names)
    document.querySelectorAll('.uc-tab').forEach(function (tab) {
        tab.addEventListener('click', function () {
            document.querySelectorAll('.uc-tab').forEach(function (t) {
                t.classList.toggle('active', t === tab);
                t.setAttribute('aria-selected', t === tab ? 'true' : 'false');
            });
            var key = tab.getAttribute('data-uc');
            ping('res.' + key);
            document.querySelectorAll('.uc-panel').forEach(function (p) {
                p.classList.toggle('active', p.getAttribute('data-panel') === key);
            });
        });
    });
