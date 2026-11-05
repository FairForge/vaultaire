// WP-LOG-2 (D-LOG-1, D-LOG-2): emit is the ONE place a line or a report
// leaves the Worker. EDGE_LOG = report (also when unset): failures are
// reported to the origin, nothing is written; errors: failures reported and
// written; all: every kind written, failures also reported; off: nothing.
// A report is {reports:[{kind, request_id, admit_id, op, status, detail}]}
// — exactly six fields, never a URL, a path or a header — at most 20 a
// minute per isolate, the rest sent as `suppressed` with the next one.
import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { emit, guarded, resetReports, REPORT_KINDS } from "../src/index.js";
import { event as codecEvent } from "../src/codec.js";

const env = (mode) => ({ VAULTAIRE_ORIGIN: "https://origin.test", VAULTAIRE_EDGE_TOKEN: "tok", ...(mode === undefined ? {} : { EDGE_LOG: mode }) });
const realFetch = globalThis.fetch;
const realLog = console.log;
let lines;
let reports;
let waited;
const ctx = { waitUntil: (p) => waited.push(p) };

beforeEach(() => {
  lines = [];
  reports = [];
  waited = [];
  resetReports();
  console.log = (s) => lines.push(s);
  globalThis.fetch = async (input, init) => {
    const url = input instanceof Request ? input.url : String(input);
    if (url.endsWith("/api/v1/edge/report")) {
      reports.push({ url, headers: init.headers, body: JSON.parse(init.body) });
      return new Response("{}", { status: 200 });
    }
    if (input instanceof Request) return new Response("origin", { status: 200, headers: { "x-origin": "1" } });
    return new Response("", { status: 500 });
  };
});
afterEach(() => {
  console.log = realLog;
  globalThis.fetch = realFetch;
});

const settle = () => Promise.all(waited);

test("the mode × kind table", async () => {
  const table = [
    // mode, kind, written, reported
    [undefined, "pass", false, false],
    [undefined, "admit_failed", false, true],
    ["report", "get", false, false],
    ["report", "commit_failed", false, true],
    ["errors", "put", false, false],
    ["errors", "exception", true, true],
    ["all", "post", true, false],
    ["all", "codec_failed", true, true],
    ["off", "mirror_settled", false, false],
    ["off", "admit_unavailable", false, false],
    ["nonsense", "admit_failed", false, true], // an unknown mode is report
  ];
  for (const [mode, kind, written, reported] of table) {
    lines = [];
    reports = [];
    waited = [];
    resetReports();
    emit(env(mode), ctx, kind, { request_id: "r1", error: "boom" });
    await settle();
    assert.equal(lines.length, written ? 1 : 0, `${mode} ${kind}: written`);
    assert.equal(reports.length, reported ? 1 : 0, `${mode} ${kind}: reported`);
    if (written) assert.equal(JSON.parse(lines[0]).edge, kind);
  }
});

test("a report is exactly six fields, the detail cut to 300, never the URL, path or headers", async () => {
  emit(env("report"), ctx, "admit_failed", {
    request_id: "rid", admit_id: "aid", op: "put", status: 502, error: "x".repeat(500),
    path: "/bucket/secret-key.txt", url: "https://s3.stored.cloud/bucket/secret-key.txt", headers: { authorization: "AWS4-HMAC-SHA256 ..." },
  });
  await settle();
  assert.equal(reports.length, 1);
  assert.equal(reports[0].url, "https://origin.test/api/v1/edge/report");
  assert.equal(reports[0].headers["x-vaultaire-edge-token"], "tok");
  const item = reports[0].body.reports[0];
  assert.deepEqual(Object.keys(item).sort(), ["admit_id", "detail", "kind", "op", "request_id", "status"]);
  assert.equal(item.kind, "admit_failed");
  assert.equal(item.status, 502);
  assert.equal(item.detail.length, 300);
  const raw = JSON.stringify(reports[0].body);
  assert.ok(!raw.includes("secret-key"), "no path, no URL");
  assert.ok(!raw.includes("AWS4"), "no header");
  assert.deepEqual(Object.keys(reports[0].body), ["reports"], "no suppressed field when none was");
});

test("20 reports a minute per isolate, the rest sent as suppressed on the next", async () => {
  for (let i = 0; i < 25; i++) emit(env(), ctx, "commit_failed", { request_id: `r${i}` });
  await settle();
  assert.equal(reports.length, 20);
  resetReports({ keepSuppressed: true });
  emit(env(), ctx, "commit_failed", { request_id: "next" });
  await settle();
  assert.equal(reports.length, 21);
  assert.equal(reports[20].body.suppressed, 5);
});

test("REPORT_KINDS is the origin's closed list", () => {
  assert.deepEqual([...REPORT_KINDS].sort(), ["admit_failed", "admit_unavailable", "catalogue_unavailable", "codec_failed", "codec_unavailable",
    "commit_failed", "exception", "mirror_settle_failed", "post_short_body", "post_unread", "refused"]);
});

test("codec.js's events reach emit through the sink", async () => {
  codecEvent(env("errors"), "catalogue_unavailable", { error: "catalogue filter answered 503" });
  // The sink has no ctx: the report is sent unawaited — let it run.
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(lines.length, 1);
  assert.equal(JSON.parse(lines[0]).edge, "catalogue_unavailable");
  assert.equal(reports.length, 1);
  assert.equal(reports[0].body.reports[0].kind, "catalogue_unavailable");
});

test("a throwing handler: an unused body passes through, a used body is a 503, one exception reported", async () => {
  const boom = async () => {
    throw new Error("kaboom");
  };
  const get = new Request("https://s3.stored.cloud/b/k", { method: "GET" });
  const r1 = await guarded(boom, get, env(), ctx);
  assert.equal(r1.status, 200);
  assert.equal(r1.headers.get("x-origin"), "1", "the origin answered");
  await settle();
  assert.equal(reports.length, 1);
  assert.equal(reports[0].body.reports[0].kind, "exception");
  assert.match(reports[0].body.reports[0].detail, /kaboom/);

  reports = [];
  const put = new Request("https://s3.stored.cloud/b/k", { method: "PUT", body: "hello" });
  const usesBody = async (req) => {
    await req.text();
    throw new Error("after the body");
  };
  const r2 = await guarded(usesBody, put, env(), ctx);
  assert.equal(r2.status, 503);
  assert.equal(r2.headers.get("content-type"), "application/xml");
  assert.ok(r2.headers.get("x-amz-request-id"), "a request id");
  assert.match(await r2.text(), /<Code>SlowDown<\/Code>/);
  await settle();
  assert.equal(reports.length, 1);
  assert.equal(reports[0].body.reports[0].kind, "exception");
});
