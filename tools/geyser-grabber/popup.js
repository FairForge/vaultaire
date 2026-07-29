// Geyser Console Grabber — collects everything needed to drive the Geyser
// console API and the underlying Spectra Vail management API from a shell.
//
// Runs entirely locally: nothing is transmitted anywhere. Values come from
// (a) cookies for geyserdata.com, (b) the active tab's localStorage /
// sessionStorage, (c) resource-timing entries (which reveal the API URLs the
// console actually calls), and (d) the rendered DOM (access-key tables).

const CONSOLE_ORIGIN = "https://console.geyserdata.com";
const VAIL_ORIGIN = "https://la1.geyserdata.com";

// Scraped from the page context. Must be fully self-contained — it is
// serialized and injected, so it cannot close over anything above.
function collectFromPage() {
  const out = {
    url: location.href,
    localStorage: {},
    sessionStorage: {},
    apiCalls: [],
    tables: [],
    keyCandidates: [],
  };

  try {
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i);
      out.localStorage[k] = localStorage.getItem(k);
    }
  } catch (e) { out.localStorage._error = String(e); }

  try {
    for (let i = 0; i < sessionStorage.length; i++) {
      const k = sessionStorage.key(i);
      out.sessionStorage[k] = sessionStorage.getItem(k);
    }
  } catch (e) { out.sessionStorage._error = String(e); }

  // Resource timing shows every URL the SPA fetched — this is how we discover
  // the console's real API surface without opening DevTools.
  try {
    const seen = new Set();
    for (const entry of performance.getEntriesByType("resource")) {
      const u = entry.name;
      if (!/\/(api|sl)\//.test(u)) continue;
      if (/\.(js|css|woff2?|png|jpe?g|svg|ico)(\?|$)/.test(u)) continue;
      const clean = u.split("?")[0];
      if (seen.has(clean)) continue;
      seen.add(clean);
      out.apiCalls.push(`${entry.initiatorType.padEnd(6)} ${clean}`);
    }
    out.apiCalls.sort();
  } catch (e) { out.apiCalls.push("error: " + String(e)); }

  // Rendered tables — on /access-keys this is the key list.
  try {
    for (const table of document.querySelectorAll("table")) {
      const rows = [];
      for (const tr of table.querySelectorAll("tr")) {
        const cells = [...tr.querySelectorAll("th,td")]
          .map((c) => c.innerText.trim().replace(/\s+/g, " "))
          .filter(Boolean);
        if (cells.length) rows.push(cells.join("  |  "));
      }
      if (rows.length) out.tables.push(rows.join("\n"));
    }
  } catch (e) { out.tables.push("error: " + String(e)); }

  // Access-key-shaped strings anywhere on the page (Geyser S3 keys are
  // 20-char uppercase IDs; secrets are 40-char base64-ish).
  try {
    const text = document.body.innerText;
    const ids = text.match(/\b[A-Z0-9]{20}\b/g) || [];
    const secrets = text.match(/\b[A-Za-z0-9+/]{40}\b/g) || [];
    out.keyCandidates = [...new Set([...ids, ...secrets])];
  } catch (e) { out.keyCandidates.push("error: " + String(e)); }

  return out;
}

function shQuote(v) {
  return "'" + String(v).replace(/'/g, `'\\''`) + "'";
}

function renderBlock(title, body, note) {
  const section = document.createElement("section");
  const empty = !body || !body.trim();

  const head = document.createElement("div");
  head.className = "head";
  const label = document.createElement("span");
  label.className = "title";
  label.textContent = title;
  if (note) {
    const count = document.createElement("span");
    count.className = "count";
    count.textContent = note;
    label.appendChild(count);
  }
  const btn = document.createElement("button");
  btn.textContent = "Copy";
  btn.disabled = empty;
  btn.addEventListener("click", async () => {
    await navigator.clipboard.writeText(body);
    btn.textContent = "Copied";
    btn.classList.add("copied");
    setTimeout(() => {
      btn.textContent = "Copy";
      btn.classList.remove("copied");
    }, 1200);
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
  const cookies = await chrome.cookies.getAll({ domain: "geyserdata.com" });
  const jar = Object.fromEntries(cookies.map((c) => [c.name, c.value]));
  const cookieHeader = cookies.map((c) => `${c.name}=${c.value}`).join("; ");

  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  let page = null;
  let pageErr = null;
  if (tab && /^https:\/\/[^/]*geyserdata\.com/.test(tab.url || "")) {
    try {
      const [res] = await chrome.scripting.executeScript({
        target: { tabId: tab.id },
        func: collectFromPage,
      });
      page = res.result;
    } catch (e) {
      pageErr = String(e);
    }
  }

  // Token hunt: cookies first, then storage keys that look like credentials.
  const token =
    jar.accessToken || jar.access_token || jar.token || jar.jwt || "";
  const userId = jar.userId || jar.userID || jar.user_id || "";
  const storageTokens = [];
  for (const store of ["localStorage", "sessionStorage"]) {
    for (const [k, v] of Object.entries((page && page[store]) || {})) {
      if (/token|jwt|auth|session|key|cred/i.test(k)) {
        storageTokens.push(`${store}.${k} = ${v}`);
      }
    }
  }

  if (!cookies.length && !page) {
    setStatus("err", "No geyserdata.com cookies and no page access. Open the console tab and log in first.");
  } else if (!token) {
    setStatus("warn", `${cookies.length} cookie(s) found, but no obvious token — check the raw cookie block and storage block below.`);
  } else {
    setStatus("ok", `Session found: ${cookies.length} cookie(s)${page ? ", page read OK" : ", page not read (open the console tab)"}.`);
  }
  if (pageErr) renderBlock("Page read error", pageErr);

  renderBlock(
    "Shell env (paste into .env.bench)",
    [
      token ? `export GEYSER_ACCESS_TOKEN=${shQuote(token)}` : "# no token cookie found",
      userId ? `export GEYSER_USER_ID=${shQuote(userId)}` : "# no userId cookie found",
      `export GEYSER_COOKIE=${shQuote(cookieHeader)}`,
    ].join("\n"),
    "for geyser_admin.go"
  );

  renderBlock(
    "Vail management API test (is the console token a Vail JWT?)",
    token
      ? `curl -sS ${VAIL_ORIGIN}/sl/api/buckets \\\n` +
        `  -H ${shQuote("Authorization: Bearer " + token)} \\\n` +
        `  -w '\\nHTTP %{http_code}\\n' | head -c 800`
      : "",
    "200 = full control plane"
  );

  renderBlock(
    "Console API calls (cookie-authenticated)",
    cookieHeader
      ? ["buckets", "keys", "invoices", "tapeCollections"]
          .map(
            (p) =>
              `curl -sS ${CONSOLE_ORIGIN}/api/${p} \\\n` +
              `  -H ${shQuote("Cookie: " + cookieHeader)} \\\n` +
              `  -H ${shQuote("Origin: " + CONSOLE_ORIGIN)} \\\n` +
              `  -H ${shQuote("Referer: " + CONSOLE_ORIGIN + "/")} \\\n` +
              `  -w '\\nHTTP %{http_code}\\n'`
          )
          .join("\n\n")
      : "",
    "bucket/key/billing/tape ops"
  );

  renderBlock(
    "Discovered API endpoints (from this page's network activity)",
    page ? page.apiCalls.join("\n") : "",
    page ? `${page.apiCalls.length} unique` : "open the console tab"
  );

  renderBlock(
    "Access keys / table contents on this page",
    page ? page.tables.join("\n\n") : "",
    "S3 keypairs live on /access-keys"
  );

  renderBlock(
    "Key-shaped strings on this page",
    page ? page.keyCandidates.join("\n") : "",
    "20-char IDs, 40-char secrets"
  );

  renderBlock("Credential-ish storage entries", storageTokens.join("\n"));

  renderBlock(
    "All cookies (raw JSON)",
    JSON.stringify(
      cookies.map((c) => ({
        name: c.name,
        value: c.value,
        domain: c.domain,
        path: c.path,
        httpOnly: c.httpOnly,
        secure: c.secure,
        expires: c.expirationDate
          ? new Date(c.expirationDate * 1000).toISOString()
          : "session",
      })),
      null,
      2
    ),
    `${cookies.length} total`
  );

  renderBlock(
    "Full storage dump",
    page
      ? JSON.stringify(
          { localStorage: page.localStorage, sessionStorage: page.sessionStorage },
          null,
          2
        )
      : ""
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
