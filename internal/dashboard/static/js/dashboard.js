function showTab(btn, id) {
    btn.closest('.code-tabs').querySelectorAll('.tab-panel').forEach(function(p) { p.classList.remove('tab-visible'); });
    btn.closest('.tab-bar').querySelectorAll('.tab-btn').forEach(function(b) { b.classList.remove('tab-active'); });
    document.getElementById(id).classList.add('tab-visible');
    btn.classList.add('tab-active');
}

document.addEventListener('click', function(e) {
    var btn = e.target.closest('.btn-copy');
    if (!btn) return;
    var url = btn.getAttribute('data-url');
    if (url) {
        navigator.clipboard.writeText(url).then(function() {
            btn.textContent = 'Copied!';
            setTimeout(function() { btn.textContent = 'Copy URL'; }, 1500);
        });
        return;
    }
    // B4: copy a code snippet by element id (onboarding quickstart tabs).
    var targetId = btn.getAttribute('data-copy-target');
    if (targetId) {
        var el = document.getElementById(targetId);
        if (!el) return;
        navigator.clipboard.writeText(el.innerText).then(function() {
            btn.textContent = 'Copied!';
            setTimeout(function() { btn.textContent = 'Copy'; }, 1500);
        });
    }
});

// Light/dark: the pick is shared with the site through localStorage "sg-theme"
// (the layouts set data-theme before first paint); the toggle button flips it.
(function () {
    var r = document.documentElement, q = window.matchMedia('(prefers-color-scheme: dark)');
    function dark() { var t = r.getAttribute('data-theme'); return t ? t === 'dark' : q.matches; }
    function sync() {
        var k = dark();
        r.classList.toggle('is-dark', k);
        document.querySelectorAll('.theme-toggle').forEach(function (b) {
            b.setAttribute('aria-label', k ? 'Switch to light mode' : 'Switch to dark mode');
        });
        var m = document.querySelector('meta[name="theme-color"]');
        if (m) m.content = k ? '#101010' : '#383838';
    }
    document.addEventListener('click', function (e) {
        var b = e.target.closest('.theme-toggle');
        if (!b) return;
        var n = dark() ? 'light' : 'dark';
        r.setAttribute('data-theme', n);
        try { localStorage.setItem('sg-theme', n); } catch (err) {}
        sync();
        document.dispatchEvent(new CustomEvent('sg-theme'));
    });
    if (q.addEventListener) q.addEventListener('change', sync);
    sync();
})();
