    // The pricing page's calculator (WP-PRICE-1): the house builder's maths —
    // whole TB downstairs (Standard) and in the attic (Vault) times the rate
    // of the period, the Vault monthly minimum on a monthly attic — and the
    // same TB at the competitors' list storage prices. Rates from prices.json.
    (function () {
        var R = __PRICING_JSON__;
        var form = document.getElementById('price-calc');
        if (!form) return;
        function money(n) { return '$' + n.toFixed(2).replace(/\B(?=(\d{3})+(?!\d))/g, ','); }
        function tb(id) {
            var v = Math.floor(Number(document.getElementById(id).value));
            return isFinite(v) && v > 0 ? Math.min(v, 10000) : 0;
        }
        function render() {
            var std = tb('pc-std'), vault = tb('pc-vault');
            var monthly = form.querySelector('input[name=period]:checked').value === 'monthly';
            var total = std * (monthly ? R.std_monthly : R.std_annual);
            if (vault > 0) total += monthly ? Math.max(vault * R.vault_monthly, R.vault_min) : vault * R.vault_annual;
            var all = std + vault;
            document.getElementById('pc-total').textContent = money(total) + '/mo';
            document.getElementById('pc-egress').textContent = (std * R.egress_std + vault * R.egress_vault) + ' TB';
            document.getElementById('pc-b2').textContent = money(all * R.b2) + '/mo';
            document.getElementById('pc-wasabi').textContent = money(all * R.wasabi) + '/mo';
            document.getElementById('pc-r2').textContent = money(all * R.r2) + '/mo';
            document.getElementById('pc-aws').textContent = money(all * R.aws) + '/mo';
        }
        form.addEventListener('input', render);
        form.addEventListener('submit', function (e) { e.preventDefault(); render(); });
        render();
    })();
