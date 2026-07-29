// Geyser Console Grabber — collects everything needed to drive the Geyser
// console API and the underlying Spectra Vail management API from a shell.
//
// Runs entirely locally: nothing is transmitted anywhere.
//
// The console does NOT authenticate with readable cookies (v1 of this tool
// assumed it did, per geyser_admin.go — that flow is stale). So instead of
// exfiltrating a credential, this version does two more reliable things:
//   1. installs a fetch/XHR hook in the page so the *app's own* outgoing auth
//      headers get captured — /api/keepalive fires on a timer, so reopening
//      the popup ~30s later reveals the real scheme; and
//   2. probes candidate endpoints from inside the page, where whatever auth
//      the app uses applies automatically.

const CONSOLE_ORIGIN = "https://console.geyserdata.com";
const VAIL_ORIGIN = "https://la1.geyserdata.com";

// Endpoint guesses: observed calls plus the feature areas named by the role
// permissions in localStorage.user (MANAGE_CLOUD_LIBRARIES, VIEW_SITES, ...).
const PROBE_PATHS = [
  "/api/buckets", "/api/keys", "/api/keepalive", "/api/invoices",
  "/api/tapeCollections", "/api/cloudLibraries", "/api/sites", "/api/events",
  "/api/users", "/api/userGroups", "/api/roles", "/api/customers",
  "/api/orgs", "/api/billing", "/api/usage", "/api/notifications",
  "/api/me", "/api/session",
];

// Injected into the page's MAIN world. Fully self-contained.
async function collectFromPage(probePaths) {
  const out = {
    url: location.href,
    localStorage: {},
    sessionStorage: {},
    indexedDB: {},
    apiCalls: [],
    captured: [],
    probes: [],
    grids: [],
    keyCandidates: [],
  };

  const dump = (store, into) => {
    try {
      for (let i = 0; i < store.length; i++) into[store.key(i)] = store.getItem(store.key(i));
    } catch (e) { into._error = String(e); }
  };
  dump(localStorage, out.localStorage);
  dump(sessionStorage, out.sessionStorage);

  // --- IndexedDB: where SPAs commonly park tokens -------------------------
  try {
    const dbs = (await indexedDB.databases?.()) || [];
    for (const { name } of dbs) {
      if (!name) continue;
      out.indexedDB[name] = {};
      const db = await new Promise((res, rej) => {
        const r = indexedDB.open(name);
        r.onsuccess = () => res(r.result);
        r.onerror = () => rej(r.error);
      });
      for (const storeName of [...db.objectStoreNames]) {
        const rows = await new Promise((res) => {
          try {
            const r = db.transaction(storeName, "readonly").objectStore(storeName).getAll();
            r.onsuccess = () => res(r.result);
            r.onerror = () => res(["<read error>"]);
          } catch (e) { res(["<" + String(e) + ">"]); }
        });
        out.indexedDB[name][storeName] = JSON.stringify(rows).slice(0, 4000);
      }
      db.close();
    }
  } catch (e) { out.indexedDB._error = String(e); }

  // --- Hook fetch + XHR so the app's own auth headers get recorded --------
  try {
    if (!window.__geyserGrab) {
      window.__geyserGrab = [];
      const record = (method, url, headers) => {
        try {
          if (/\/(api|sl)\//.test(String(url))) {
            window.__geyserGrab.push({ method, url: String(url), headers });
            if (window.__geyserGrab.length > 40) window.__geyserGrab.shift();
          }
        } catch (_) {}
      };

      const origFetch = window.fetch;
      window.fetch = function (input, init) {
        try {
          const url = typeof input === "string" ? input : input?.url;
          const h = {};
          const src = init?.headers || (typeof input === "object" ? input?.headers : null);
          if (src) {
            if (typeof src.forEach === "function") src.forEach((v, k) => (h[k] = v));
            else Object.assign(h, src);
          }
          record((init?.method || "GET").toUpperCase(), url, h);
        } catch (_) {}
        return origFetch.apply(this, arguments);
      };

      const origOpen = XMLHttpRequest.prototype.open;
      const origSet = XMLHttpRequest.prototype.setRequestHeader;
      const origSend = XMLHttpRequest.prototype.send;
      XMLHttpRequest.prototype.open = function (m, u) {
        this.__g = { method: m, url: u, headers: {} };
        return origOpen.apply(this, arguments);
      };
      XMLHttpRequest.prototype.setRequestHeader = function (k, v) {
        if (this.__g) this.__g.headers[k] = v;
        return origSet.apply(this, arguments);
      };
      XMLHttpRequest.prototype.send = function () {
        if (this.__g) record(this.__g.method, this.__g.url, this.__g.headers);
        return origSend.apply(this, arguments);
      };
      out.captured.push("hook installed — leave this tab open ~30s (keepalive fires), then reopen the popup");
    }
    for (const c of window.__geyserGrab || []) {
      const hdrs = Object.entries(c.headers || {})
        .map(([k, v]) => `${k}: ${v}`)
        .join(" | ");
      out.captured.push(`${c.method} ${c.url}${hdrs ? "\n    " + hdrs : "  (no explicit headers — cookie/credentials mode)"}`);
    }
  } catch (e) { out.captured.push("hook error: " + String(e)); }

  // --- Resource timing: what this page already called ---------------------
  try {
    const seen = new Set();
    for (const e of performance.getEntriesByType("resource")) {
      if (!/\/(api|sl)\//.test(e.name)) continue;
      if (/\.(js|css|woff2?|png|jpe?g|svg|ico)(\?|$)/.test(e.name)) continue;
      const clean = e.name.split("?")[0];
      if (!seen.has(clean)) { seen.add(clean); out.apiCalls.push(`${e.initiatorType.padEnd(14)} ${clean}`); }
    }
    out.apiCalls.sort();
  } catch (e) { out.apiCalls.push("error: " + String(e)); }

  // --- Probe endpoints from inside the page (auth applies automatically) --
  for (const p of probePaths) {
    try {
      const r = await fetch(p, { credentials: "include" });
      const body = (await r.text()).replace(/\s+/g, " ").slice(0, 220);
      out.probes.push(`${String(r.status).padEnd(4)} ${p.padEnd(22)} ${body}`);
    } catch (e) {
      out.probes.push(`ERR  ${p.padEnd(22)} ${String(e).slice(0, 120)}`);
    }
  }

  // --- Rendered data: real <table>s and div/aria grids --------------------
  try {
    const nodes = [
      ...document.querySelectorAll("table"),
      ...document.querySelectorAll('[role="table"],[role="grid"],[class*="table" i],[class*="grid" i]'),
    ];
    const seen = new Set();
    for (const n of nodes.slice(0, 12)) {
      const t = (n.innerText || "").trim().replace(/\n{3,}/g, "\n\n");
      if (t && t.length > 20 && !seen.has(t)) { seen.add(t); out.grids.push(t.slice(0, 1500)); }
    }
  } catch (e) { out.grids.push("error: " + String(e)); }

  try {
    const text = document.body.innerText;
    out.keyCandidates = [
      ...new Set([...(text.match(/\b[A-Z0-9]{20}\b/g) || []), ...(text.match(/\b[A-Za-z0-9+/]{40}\b/g) || [])]),
    ];
  } catch (_) {}

  return out;
}

function shQuote(v) {
  return "'" + String(v).replace(/'/g, `'\\''`) + "'";
}

function renderBlock(title, body, note) {
  const section = document.createElement("section");
  const empty = !body || !String(body).trim();

  const head = document.createElement("div");
  head.className = "head";
  const label = document.createElement("span");
  label.className = "title";
  label.textContent = title;
  if (note) {
    const c = document.createElement("span");
    c.className = "count";
    c.textContent = note;
    label.appendChild(c);
  }
  const btn = document.createElement("button");
  btn.textContent = "Copy";
  btn.disabled = empty;
  btn.addEventListener("click", async () => {
    await navigator.clipboard.writeText(body);
    btn.textContent = "Copied";
    btn.classList.add("copied");
    setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("copied"); }, 1200);
  });
  head.append(label, btn);

  const pre = document.createElement("pre");
  pre.textContent = empty ? "(nothing found)" : body;
  if (empty) pre.className = "empty";

  section.append(head, pre);
  document.getElementById("blocks").appendChild(section);
}

function setStatus(cls, text) {
  const el = document.getElementById("status");
  el.className = "status " + cls;
  el.textContent = text;
}

async function main() {
  // Query per-URL as well as per-domain: partitioned/host-only cookies on the
  // app subdomain do not always come back from a bare domain query.
  const byDomain = await chrome.cookies.getAll({ domain: "geyserdata.com" });
  const byUrl = [
    ...(await chrome.cookies.getAll({ url: CONSOLE_ORIGIN + "/" })),
    ...(await chrome.cookies.getAll({ url: VAIL_ORIGIN + "/" })),
  ];
  const cookies = [...byDomain, ...byUrl].filter(
    (c, i, a) => a.findIndex((x) => x.name === c.name && x.domain === c.domain) === i
  );
  const appCookies = cookies.filter((c) => /console|la1|^\.?geyserdata/.test(c.domain));
  const cookieHeader = appCookies.map((c) => `${c.name}=${c.value}`).join("; ");

  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  const onConsole = /^https:\/\/console\.geyserdata\.com/.test(tab?.url || "");
  let page = null, pageErr = null;

  if (tab && /geyserdata\.com/.test(tab.url || "")) {
    try {
      const [res] = await chrome.scripting.executeScript({
        target: { tabId: tab.id },
        world: "MAIN",
        func: collectFromPage,
        args: [PROBE_PATHS],
      });
      page = res.result;
    } catch (e) { pageErr = String(e); }
  }

  if (!onConsole) {
    setStatus("warn", "Active tab is not console.geyserdata.com — open the console app and click again.");
  } else if (page) {
    const ok = (page.probes || []).filter((p) => p.startsWith("200")).length;
    setStatus("ok", `Console read OK — ${ok} endpoint(s) returned 200. Leave the tab open 30s and reopen for captured auth headers.`);
  } else {
    setStatus("err", "Could not read the page. " + (pageErr || ""));
  }
  if (pageErr) renderBlock("Page read error", pageErr);

  renderBlock(
    "Endpoint probe (run inside the page — real auth applied)",
    page ? page.probes.join("\n") : "",
    "status  path  response"
  );

  renderBlock(
    "Captured auth headers (app's own requests)",
    page ? page.captured.join("\n") : "",
    "reopen after ~30s for keepalive"
  );

  renderBlock(
    "IndexedDB (common token hiding place)",
    page ? JSON.stringify(page.indexedDB, null, 2) : ""
  );

  renderBlock(
    "Discovered API endpoints (resource timing)",
    page ? page.apiCalls.join("\n") : "",
    page ? `${page.apiCalls.length} unique` : ""
  );

  renderBlock(
    "Rendered tables / grids (access keys, buckets, tapes)",
    page ? page.grids.join("\n\n———\n\n") : ""
  );

  renderBlock("Key-shaped strings", page ? page.keyCandidates.join("\n") : "");

  renderBlock(
    "Shell env (paste into .env.bench)",
    [
      cookieHeader ? `export GEYSER_COOKIE=${shQuote(cookieHeader)}` : "# no app cookies — console auth is not cookie-based",
      ...appCookies
        .filter((c) => /token|session|auth|jwt/i.test(c.name))
        .map((c) => `# ${c.name} (${c.domain}) = ${c.value.slice(0, 24)}…`),
    ].join("\n")
  );

  renderBlock(
    "All cookies (raw JSON)",
    JSON.stringify(
      cookies.map((c) => ({
        name: c.name, domain: c.domain, path: c.path,
        httpOnly: c.httpOnly, secure: c.secure,
        value: c.value.length > 80 ? c.value.slice(0, 80) + "…" : c.value,
      })),
      null, 2
    ),
    `${cookies.length} total`
  );

  renderBlock(
    "Full storage dump",
    page ? JSON.stringify({ localStorage: page.localStorage, sessionStorage: page.sessionStorage }, null, 2) : ""
  );

  document.getElementById("all").addEventListener("click", async (e) => {
    const report = [...document.querySelectorAll("section")]
      .map((s) => `### ${s.querySelector(".title").textContent}\n${s.querySelector("pre").textContent}`)
      .join("\n\n");
    await navigator.clipboard.writeText(report);
    e.target.textContent = "Copied everything";
    setTimeout(() => (e.target.textContent = "Copy everything as one report"), 1400);
  });
}

main().catch((e) => setStatus("err", "Grabber failed: " + String(e)));
