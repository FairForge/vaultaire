// stored-status — Stored's status page on status.stored.cloud (WP-DOMAIN-1
// part 3B; contract rule 11, docs/surfaces/DEVELOPER.md "The first ten" 2).
//
// It must say the truth when the origin is down, so it never asks the origin
// anything: the box PUSHES its state every minute (deploy/scripts/
// status-push.sh, POST /push with the STATUS_PUSH_TOKEN secret as its bearer)
// and the page renders the last report it was given. A report older than
// STALE_AFTER_SECONDS (5 min) is shown under a red banner and claims no
// health: silence from the box is itself the news.
//
// Routes, GET and HEAD unless said: / (the page), /state.json (the same as
// data), POST /push. Everything else is 404, and so is any request carrying
// S3 credentials (Architecture Decision 8, RP91-8). Every answer is no-store, sets no
// cookie, and carries a CSP that lets nothing load — no script, no beacon, no
// font, no image. One file, no bundling, no imports: the state is one Durable
// Object (StatusState) because a report must be read back exactly as written,
// at once, from any colo.


const MAX_BODY = 16 * 1024;
const MAX_SKEW = 600; // seconds a report's own clock may differ from ours
const STATES = ["up", "degraded", "down"];

const BASE_HEADERS = {
  "cache-control": "no-store",
  "x-content-type-options": "nosniff",
  "referrer-policy": "no-referrer",
  "content-security-policy": "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
};

function respond(status, body, type, head = false) {
  const headers = { ...BASE_HEADERS };
  if (type) headers["content-type"] = type;
  return new Response(head ? null : body, { status, headers });
}
const text = (status, body) => respond(status, body, "text/plain; charset=utf-8");

// StatusState holds the one last report. PUT stores a report newer than the
// stored one (409 otherwise: a delayed push never overwrites a later one);
// GET answers the stored record or null.
export class StatusState {
  constructor(state) {
    this.storage = state.storage;
  }

  async fetch(request) {
    if (request.method === "PUT") {
      const rec = await request.json();
      const prev = await this.storage.get("record");
      if (prev && rec.report.at <= prev.report.at) return new Response(null, { status: 409 });
      await this.storage.put("record", rec);
      return new Response(null, { status: 204 });
    }
    const rec = await this.storage.get("record");
    return new Response(JSON.stringify(rec ?? null), { headers: { "content-type": "application/json" } });
  }
}

const stub = (env) => env.STATE.get(env.STATE.idFromName("status"));

async function sha256(s) {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s)));
}

// tokenMatches compares the digests of the two tokens in constant time.
async function tokenMatches(given, want) {
  const [a, b] = await Promise.all([sha256(given), sha256(want)]);
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i];
  return diff === 0;
}

const isStr = (v, max, re) => typeof v === "string" && v.length <= max && (!re || re.test(v));
const PRINTABLE = /^[^\u0000-\u001f\u007f]*$/;

// validate returns the report as it will be stored, or a reason it is refused.
function validate(body, nowSec) {
  if (body === null || typeof body !== "object" || Array.isArray(body)) return { error: "not an object" };
  if (body.v !== 1) return { error: "v must be 1" };
  if (!Number.isInteger(body.at) || Math.abs(body.at - nowSec) > MAX_SKEW) return { error: "at is not this minute's unix time" };
  if (!isStr(body.build, 40, /^([0-9a-f]{7,40})?$/)) return { error: "build" };
  const comps = body.components;
  if (!Array.isArray(comps) || comps.length < 1 || comps.length > 12) return { error: "components: 1 to 12" };
  const seen = new Set();
  const components = [];
  for (const c of comps) {
    if (c === null || typeof c !== "object") return { error: "component" };
    if (!isStr(c.id, 32, /^[a-z][a-z0-9_-]*$/) || seen.has(c.id)) return { error: "component id" };
    if (!isStr(c.name, 64, PRINTABLE) || c.name === "") return { error: "component name" };
    if (!STATES.includes(c.state)) return { error: "component state" };
    const detail = c.detail ?? "";
    if (!isStr(detail, 120, PRINTABLE)) return { error: "component detail" };
    seen.add(c.id);
    components.push({ id: c.id, name: c.name, state: c.state, detail });
  }
  const alerts = body.alerts ?? [];
  if (!Array.isArray(alerts) || alerts.length > 50 || !alerts.every((a) => isStr(a, 64, /^[A-Za-z][A-Za-z0-9_:]*$/))) return { error: "alerts: names only" };
  return { report: { at: body.at, build: body.build, components, alerts: [...new Set(alerts)].sort() } };
}

function staleAfter(env) {
  const n = parseInt(env.STALE_AFTER_SECONDS ?? "", 10);
  return Number.isInteger(n) && n >= 10 && n <= 3600 ? n : 300;
}

// view is what both / and /state.json show.
function view(rec, env) {
  if (!rec) return { verdict: "unknown", label: "No report yet", stale: true, last_heard: null, age_seconds: null, build: "", components: [], alerts: [] };
  const age = Math.max(0, Math.floor((Date.now() - rec.received_at) / 1000));
  const stale = age > staleAfter(env);
  const { components, alerts, build } = rec.report;
  let verdict = "operational", label = "All systems operational";
  if (stale) [verdict, label] = ["unknown", "No recent report"];
  else if (components.some((c) => c.state === "down")) [verdict, label] = ["outage", "Partial outage"];
  else if (components.some((c) => c.state === "degraded")) [verdict, label] = ["degraded", "Degraded"];
  return { verdict, label, stale, last_heard: new Date(rec.received_at).toISOString(), age_seconds: age, build, components, alerts };
}

const esc = (s) => String(s).replace(/[&<>"']/g, (ch) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[ch]);
const utc = (iso) => iso.replace("T", " ").slice(0, 19) + " UTC";
const ago = (s) => (s < 120 ? `${s} s` : s < 7200 ? `${Math.floor(s / 60)} min` : `${Math.floor(s / 3600)} h`);
const WORD = { up: "Operational", degraded: "Degraded", down: "Down" };

function page(v, env) {
  const banner = !v.last_heard
    ? `<p class="banner">No report has reached this page yet.</p>`
    : v.stale
      ? `<p class="banner">No report from the service for ${esc(ago(v.age_seconds))} (the last at ${esc(utc(v.last_heard))}). The service may be down, or only its reports; what is below is the last thing it said.</p>`
      : "";
  const rows = v.components.map((c) => `<tr><td>${esc(c.name)}</td><td class="s ${esc(c.state)}">${esc(WORD[c.state])}</td><td class="d">${esc(c.detail)}</td></tr>`).join("");
  const alerts = v.alerts.length ? `<h2>Alerts firing</h2><ul>${v.alerts.map((a) => `<li><code>${esc(a)}</code></li>`).join("")}</ul>` : "";
  const heard = v.last_heard ? `<p class="m">Last heard: ${esc(utc(v.last_heard))} (${esc(ago(v.age_seconds))} ago)${v.build ? ` · build <code>${esc(v.build)}</code>` : ""}</p>` : "";
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="60">
<title>Stored status</title>
<style>
:root{--bg:#fff;--fg:#111;--mut:#555;--line:#ddd;--up:#0a7a3d;--deg:#a15c00;--down:#b3141b;--unk:#555}
@media (prefers-color-scheme:dark){:root{--bg:#111;--fg:#eee;--mut:#aaa;--line:#333;--up:#4cc27c;--deg:#f0a640;--down:#ff6b6b;--unk:#aaa}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
main{max-width:720px;margin:0 auto;padding:32px 16px}
h1{font-size:20px;margin:0 0 24px}h2{font-size:16px;margin:28px 0 8px}
.v{display:inline-block;padding:6px 12px;border:3px solid currentColor;font-weight:700}
.v.operational{color:var(--up)}.v.degraded{color:var(--deg)}.v.outage{color:var(--down)}.v.unknown{color:var(--unk)}
.banner{background:var(--down);color:#fff;padding:12px 16px;font-weight:700}
table{width:100%;border-collapse:collapse;margin-top:24px}td{padding:8px 4px;border-bottom:1px solid var(--line);vertical-align:top}
.s{font-weight:700;white-space:nowrap}.s.up{color:var(--up)}.s.degraded{color:var(--deg)}.s.down{color:var(--down)}
.d,.m{color:var(--mut)}.m{font-size:14px;margin-top:24px}code{font-size:14px}
</style>
</head>
<body>
<main>
<h1>Stored status</h1>
${banner}
<p class="v ${esc(v.verdict)}">${esc(v.label)}</p>
${rows ? `<table>${rows}</table>` : ""}
${alerts}
${heard}
<p class="m">This page is served by Cloudflare from the last report the service pushed to it; it never asks the service, so it stays up when the service does not. Machine-readable: <code>/state.json</code>.</p>
</main>
</body>
</html>
`;
}

async function handlePush(request, env) {
  const want = env.STATUS_PUSH_TOKEN || "";
  if (want.length < 32) return text(503, "push is not configured\n");
  const m = (request.headers.get("authorization") || "").match(/^Bearer ([\x21-\x7e]+)$/);
  if (!m || !(await tokenMatches(m[1], want))) return text(401, "unauthorized\n");
  const raw = await request.text();
  if (raw.length > MAX_BODY) return text(400, "too large\n");
  let body;
  try {
    body = JSON.parse(raw);
  } catch {
    return text(400, "not JSON\n");
  }
  const { report, error } = validate(body, Math.floor(Date.now() / 1000));
  if (error) return text(400, error + "\n");
  const res = await stub(env).fetch(new Request("https://state/", { method: "PUT", body: JSON.stringify({ report, received_at: Date.now() }) }));
  if (res.status === 409) return text(409, "not newer than the stored report\n");
  return respond(204, null);
}

// carriesS3Credentials is the origin's credential gate's predicate
// (Architecture Decision 8, auth.IsAWSAuthorization + signedS3Request; RP91-8):
// an Authorization whose scheme token — before the first space, any case — is
// SigV4, SigV4A or SigV2, or a presigned query. Such a request is an S3 client's
// and gets a 404 here, never the page as its object bytes.
const AWS_SCHEMES = ["aws4-hmac-sha256", "aws4-ecdsa-p256-sha256", "aws"];
function carriesS3Credentials(request, url) {
  const h = request.headers.get("authorization") || "";
  const sp = h.indexOf(" ");
  if (sp > 0 && AWS_SCHEMES.includes(h.slice(0, sp).toLowerCase())) return true;
  const q = url.searchParams;
  return ["X-Amz-Signature", "X-Amz-Credential", "X-Amz-Algorithm", "AWSAccessKeyId"].some((k) => q.has(k));
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const { pathname } = url;
    const method = request.method;
    if (carriesS3Credentials(request, url)) return text(404, "not found\n");
    if (pathname === "/push" && method === "POST") return handlePush(request, env);
    if ((pathname === "/" || pathname === "/state.json") && (method === "GET" || method === "HEAD")) {
      const rec = await (await stub(env).fetch(new Request("https://state/"))).json();
      const v = view(rec, env);
      const head = method === "HEAD";
      if (pathname === "/state.json") {
        const { label, ...data } = v;
        return respond(200, JSON.stringify(data), "application/json", head);
      }
      return respond(200, page(v, env), "text/html; charset=utf-8", head);
    }
    return text(404, "not found\n");
  },
};
