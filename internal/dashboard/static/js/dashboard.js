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
    }
});
