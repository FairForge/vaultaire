/* stored.ge billing: the house steppers. Prices come from data attributes
   the server fills from prices.json; the maths mirrors billing.QuoteHouse
   (annual = per TB per month billed yearly; the attic's monthly price has a
   flat minimum for 1 TB). No inline handlers (CSP), works without JS. */
(function () {
    var form = document.getElementById('house-form');
    if (!form) return;
    var P = {
        stdAnnual: +form.dataset.stdAnnual, stdMonthly: +form.dataset.stdMonthly,
        vaultAnnual: +form.dataset.vaultAnnual, vaultMonthly: +form.dataset.vaultMonthly,
        vaultMonthlyMin: +form.dataset.vaultMonthlyMin, pinHot: +form.dataset.pinHot
    };
    var std = document.getElementById('house-std');
    var vault = document.getElementById('house-vault');
    var pin = document.getElementById('house-pin');
    var monthlyEl = document.getElementById('house-monthly');
    var chargeEl = document.getElementById('house-charge');
    var chargeWrap = document.getElementById('house-charge-wrap');
    var linesBody = document.querySelector('#house-lines tbody');

    function cents(usd) { return Math.round(usd * 100); }
    function money(c) { return '$' + Math.floor(c / 100) + '.' + String(c % 100).padStart(2, '0'); }
    function val(el) { var n = parseInt(el.value, 10); return isNaN(n) || n < 0 ? 0 : n; }
    function period() {
        var r = form.querySelector('input[name="period"]:checked') || form.querySelector('input[name="period"]');
        return r && r.value === 'monthly' ? 'monthly' : 'annual';
    }
    function clamp(el) {
        var n = val(el), min = parseInt(el.min, 10) || 0, max = parseInt(el.max, 10) || 300;
        if (n < min) n = min;
        if (n > max) n = max;
        el.value = n;
        return n;
    }
    function render() {
        var s = clamp(std), v = clamp(vault);
        pin.max = s;
        var p = clamp(pin);
        var monthly = period() === 'monthly';
        var lines = [];
        if (s) lines.push(['Downstairs', s, s * cents(monthly ? P.stdMonthly : P.stdAnnual)]);
        if (v) {
            var vc = monthly ? (v === 1 ? cents(P.vaultMonthlyMin) : v * cents(P.vaultMonthly)) : v * cents(P.vaultAnnual);
            lines.push(['Attic', v, vc]);
        }
        if (p) lines.push(['Pin-hot', p, p * cents(P.pinHot)]);
        var total = 0;
        linesBody.textContent = '';
        lines.forEach(function (l) {
            total += l[2];
            var tr = document.createElement('tr');
            var th = document.createElement('th'); th.scope = 'row'; th.textContent = l[0];
            var td1 = document.createElement('td'); td1.textContent = l[1] + ' TB';
            var td2 = document.createElement('td'); td2.textContent = money(l[2]);
            tr.appendChild(th); tr.appendChild(td1); tr.appendChild(td2);
            linesBody.appendChild(tr);
        });
        monthlyEl.textContent = money(total);
        chargeEl.textContent = money(total * 12);
        chargeWrap.hidden = monthly;
        form.querySelector('button[type="submit"]').disabled = form.querySelector('button[type="submit"]').getAttribute('aria-disabled') === 'true' || (s === 0 && v === 0);
    }
    form.addEventListener('click', function (e) {
        var b = e.target.closest('.step');
        if (!b) return;
        var el = document.getElementById(b.dataset.for);
        el.value = val(el) + parseInt(b.dataset.step, 10);
        render();
    });
    form.addEventListener('input', render);
    form.addEventListener('change', render);
    render();
})();
