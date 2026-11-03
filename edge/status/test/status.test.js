import { test } from "node:test";
import assert from "node:assert/strict";
import worker, { StatusState } from "../src/index.js";

const TOKEN = "t".repeat(64);
const ORIGIN = "https://status.test";

// A Durable Object namespace in memory: one object, the storage a Map.
function makeEnv(extra = {}) {
  const map = new Map();
  const storage = { get: async (k) => structuredClone(map.get(k)), put: async (k, v) => { map.set(k, structuredClone(v)); } };
  const obj = new StatusState({ storage });
  return { STATUS_PUSH_TOKEN: TOKEN, STATE: { idFromName: (n) => n, get: () => ({ fetch: (r) => obj.fetch(r) }) }, ...extra };
}

const now = () => Math.floor(Date.now() / 1000);
const good = (over = {}) => ({
  v: 1,
  at: now(),
  build: "6c03823f3964",
  components: [
    { id: "web", name: "stored.cloud", state: "up", detail: "healthy" },
    { id: "s3", name: "S3 API (synthetic check)", state: "up", detail: "passing" },
    { id: "legacy", name: "stored.ge", state: "up", detail: "healthy" },
    { id: "dashboard", name: "Dashboard sign-in", state: "up", detail: "200" },
  ],
  alerts: [],
  ...over,
});

const push = (env, body, token = TOKEN) => worker.fetch(new Request(ORIGIN + "/push", {
  method: "POST",
  headers: { "content-type": "application/json", ...(token !== null ? { authorization: "Bearer " + token } : {}) },
  body: typeof body === "string" ? body : JSON.stringify(body),
}), env);
const get = (env, path = "/", method = "GET") => worker.fetch(new Request(ORIGIN + path, { method }), env);

function assertQuiet(res) {
  assert.equal(res.headers.get("cache-control"), "no-store");
  assert.equal(res.headers.get("set-cookie"), null, "never a cookie");
  const csp = res.headers.get("content-security-policy") || "";
  assert.match(csp, /default-src 'none'/, "no script, no beacon, no fetch can load");
}

test("no report yet: the page says so, never 'operational'", async () => {
  const env = makeEnv();
  const res = await get(env);
  assert.equal(res.status, 200);
  assertQuiet(res);
  const html = await res.text();
  assert.match(html, /No report yet/);
  assert.doesNotMatch(html, /All systems operational/);
  const js = await (await get(env, "/state.json")).json();
  assert.equal(js.verdict, "unknown");
});

test("push: no token, a wrong token, another scheme are 401 and change nothing", async () => {
  const env = makeEnv();
  for (const t of [null, "", "x".repeat(64), TOKEN + "x", TOKEN.slice(1)]) {
    const res = await push(env, good(), t);
    assert.equal(res.status, 401, `token ${t && t.length}`);
    assertQuiet(res);
  }
  const res = await worker.fetch(new Request(ORIGIN + "/push", { method: "POST", headers: { authorization: "Basic " + TOKEN }, body: JSON.stringify(good()) }), env);
  assert.equal(res.status, 401);
  assert.match(await (await get(env)).text(), /No report yet/);
});

test("push: a Worker without its secret (or a short one) refuses every push", async () => {
  for (const secret of [undefined, "", "short"]) {
    const env = makeEnv({ STATUS_PUSH_TOKEN: secret });
    const res = await push(env, good(), secret || "");
    assert.equal(res.status, 503);
  }
});

test("a good push renders: verdict, rows, build, last heard; /state.json carries the same", async () => {
  const env = makeEnv();
  const res = await push(env, good());
  assert.equal(res.status, 204);
  const html = await (await get(env)).text();
  assert.match(html, /All systems operational/);
  for (const s of ["stored.cloud", "S3 API (synthetic check)", "stored.ge", "Dashboard sign-in", "6c03823f3964", "Last heard"]) assert.ok(html.includes(s), s);
  assert.doesNotMatch(html, /No recent report/);
  const js = await (await get(env, "/state.json")).json();
  assert.equal(js.verdict, "operational");
  assert.equal(js.stale, false);
  assert.equal(js.build, "6c03823f3964");
  assert.equal(js.components.length, 4);
  assert.deepEqual(js.alerts, []);
});

test("verdict: one row down is a partial outage, one degraded is degraded; firing alerts are listed by name", async () => {
  const env = makeEnv();
  const c = good().components;
  await push(env, good({ components: [{ ...c[0], state: "degraded", detail: "primary breaker open" }, ...c.slice(1)], alerts: ["SyntheticCheckFailing", "SyntheticCheckFailing", "HostRebootRequired"] }));
  let js = await (await get(env, "/state.json")).json();
  assert.equal(js.verdict, "degraded");
  assert.deepEqual(js.alerts, ["HostRebootRequired", "SyntheticCheckFailing"], "deduplicated, sorted");
  const html = await (await get(env)).text();
  assert.match(html, /Degraded/);
  assert.match(html, /SyntheticCheckFailing/);

  await push(env, good({ at: now() + 1, components: [c[0], { ...c[1], state: "down", detail: "failing" }, ...c.slice(2)] }));
  js = await (await get(env, "/state.json")).json();
  assert.equal(js.verdict, "outage");
  assert.match(await (await get(env)).text(), /Partial outage/);
});

test("a stale push shows the red banner and no verdict of health", async () => {
  const env = makeEnv({ STALE_AFTER_SECONDS: "10" });
  await push(env, good());
  const real = Date.now;
  try {
    Date.now = () => real() + 11_000;
    const html = await (await get(env)).text();
    assert.match(html, /class="banner"/);
    assert.match(html, /No recent report/);
    assert.doesNotMatch(html, /All systems operational/);
    const js = await (await get(env, "/state.json")).json();
    assert.equal(js.stale, true);
    assert.equal(js.verdict, "unknown");
    assert.ok(js.age_seconds >= 11);
  } finally {
    Date.now = real;
  }
  // The default threshold is five minutes.
  const env2 = makeEnv();
  await push(env2, good());
  try {
    Date.now = () => real() + 299_000;
    assert.equal((await (await get(env2, "/state.json")).json()).stale, false);
    Date.now = () => real() + 301_000;
    assert.equal((await (await get(env2, "/state.json")).json()).stale, true);
  } finally {
    Date.now = real;
  }
});

test("push: an older or replayed report is refused (409), a report off the clock is refused (400)", async () => {
  const env = makeEnv();
  const t = now();
  assert.equal((await push(env, good({ at: t }))).status, 204);
  assert.equal((await push(env, good({ at: t }))).status, 409, "the same report twice");
  assert.equal((await push(env, good({ at: t - 5 }))).status, 409, "an older report");
  assert.equal((await push(env, good({ at: t - 3600 }))).status, 400, "an hour old");
  assert.equal((await push(env, good({ at: t + 3600 }))).status, 400, "an hour ahead");
});

test("push: what does not validate is 400 and stores nothing", async () => {
  const env = makeEnv();
  const c = good().components[0];
  const bad = [
    "not json",
    JSON.stringify({ ...good(), v: 2 }),
    JSON.stringify({ ...good(), build: "<script>" }),
    JSON.stringify({ ...good(), components: [] }),
    JSON.stringify({ ...good(), components: Array.from({ length: 13 }, (_, i) => ({ ...c, id: "c" + i })) }),
    JSON.stringify({ ...good(), components: [{ ...c, state: "fine" }] }),
    JSON.stringify({ ...good(), components: [{ ...c, id: "Web!" }] }),
    JSON.stringify({ ...good(), components: [c, c] }),
    JSON.stringify({ ...good(), components: [{ ...c, name: "x".repeat(65) }] }),
    JSON.stringify({ ...good(), components: [{ ...c, detail: "x".repeat(121) }] }),
    JSON.stringify({ ...good(), alerts: ["has space"] }),
    JSON.stringify({ ...good(), alerts: Array.from({ length: 51 }, (_, i) => "A" + i) }),
    JSON.stringify({ ...good(), extra: "x".repeat(20000) }),
  ];
  for (const b of bad) {
    const res = await push(env, b);
    assert.equal(res.status, 400, b.slice(0, 60));
  }
  assert.match(await (await get(env)).text(), /No report yet/);
});

test("what is pushed is escaped in the page", async () => {
  const env = makeEnv();
  const c = good().components;
  await push(env, good({ components: [{ ...c[0], name: "<b>x</b>", detail: "\"><img src=x>" }] }));
  const html = await (await get(env)).text();
  assert.ok(!html.includes("<b>x</b>"));
  assert.ok(!html.includes("<img"));
  assert.ok(html.includes("&lt;b&gt;x&lt;/b&gt;"));
});

test("the route list is closed: everything else is 404, never cached; HEAD has no body", async () => {
  const env = makeEnv();
  for (const [method, path] of [["GET", "/push"], ["GET", "/health"], ["GET", "/dashboard"], ["PUT", "/"], ["DELETE", "/state.json"], ["POST", "/"], ["GET", "/state.json/x"], ["GET", "/index.html"]]) {
    const res = await get(env, path, method);
    assert.equal(res.status, 404, `${method} ${path}`);
    assertQuiet(res);
  }
  const head = await get(env, "/", "HEAD");
  assert.equal(head.status, 200);
  assert.equal(await head.text(), "");
  const hj = await get(env, "/state.json", "HEAD");
  assert.equal(hj.status, 200);
  assert.equal(await hj.text(), "");
});

test("the Worker never fetches anything: no origin dependency", async () => {
  const env = makeEnv();
  const real = globalThis.fetch;
  globalThis.fetch = () => { throw new Error("the status Worker must not fetch"); };
  try {
    await push(env, good());
    await get(env);
    await get(env, "/state.json");
  } finally {
    globalThis.fetch = real;
  }
});

// RP91-8 (Architecture Decision 8): a request carrying S3 credentials never
// gets a page as its bytes — the same shapes as the origin's gate
// (auth.IsAWSAuthorization: the scheme token before the first space, any
// case; and the presigned query parameters) answer 404 on every path.
test("a request carrying S3 credentials is a 404, never the page", async () => {
  const env = makeEnv();
  assert.equal((await push(env, good())).status, 204);
  const headers = [
    "AWS4-HMAC-SHA256 Credential=AKIA/20261102/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc",
    "aws4-hmac-sha256 Credential=x",
    "AWS4-ECDSA-P256-SHA256 Credential=x",
    "AWS AKIAEXAMPLE:c2lnbmF0dXJl",
  ];
  for (const path of ["/", "/state.json", "/some-bucket/key"]) {
    for (const method of ["GET", "HEAD"]) {
      for (const h of headers) {
        const res = await worker.fetch(new Request(ORIGIN + path, { method, headers: { authorization: h } }), env);
        assert.equal(res.status, 404, `${method} ${path} ${h}`);
        assertQuiet(res);
      }
    }
    for (const q of ["X-Amz-Signature=abc", "X-Amz-Credential=AKIA%2F20261102", "X-Amz-Algorithm=AWS4-HMAC-SHA256", "AWSAccessKeyId=AKIA&Signature=x&Expires=1"]) {
      const res = await get(env, path + "?" + q);
      assert.equal(res.status, 404, `${path}?${q}`);
    }
  }
  // A signed POST /push is no push either.
  const res = await worker.fetch(new Request(ORIGIN + "/push", { method: "POST", headers: { authorization: headers[0] }, body: JSON.stringify(good()) }), env);
  assert.equal(res.status, 404);
  // Not an AWS scheme: the page as before.
  for (const h of ["Bearer abc", "Basic dXNlcjpwYXNz", "AWS4-HMAC-SHA256", "AWSX foo"]) {
    const ok = await worker.fetch(new Request(ORIGIN + "/", { headers: { authorization: h } }), env);
    assert.equal(ok.status, 200, h);
  }
});
