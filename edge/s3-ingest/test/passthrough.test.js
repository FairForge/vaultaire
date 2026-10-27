// RP70-5 / RB3-9: every request the Worker passes to the origin goes with
// `cache: "no-store"` — the first pass-through after a content deploy lost
// the signed `range` and `if-none-match` to Cloudflare's cache layer (the
// 0.3.0 deploy's capture, docs/reviews/WP-EDGE-2B.md § RB-9). A fake global
// fetch records the init of every call the Worker makes.
import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import worker, { passThrough } from "../src/index.js";

const sig = "AWS4-HMAC-SHA256 Credential=VKX/20261027/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=00";
const env = { VAULTAIRE_ORIGIN: "https://origin.test", VAULTAIRE_EDGE_TOKEN: "tok" };
const ctx = { waitUntil: () => {} };
const realFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = realFetch;
});

/** recordFetch installs a fake fetch: control-plane calls answer `admit`
 * (and an ack for a commit), backend calls answer `backend`, and every call
 * whose input is the client's Request is recorded as a pass-through. */
function recordFetch({ admit = null, admitStatus = 200, backend = null } = {}) {
  const passes = [];
  globalThis.fetch = async (input, init) => {
    if (input instanceof Request) {
      passes.push({ url: input.url, init });
      return new Response("origin", { status: 200 });
    }
    const url = String(input);
    if (url.endsWith("/api/v1/edge/admit")) return new Response(JSON.stringify(admit), { status: admitStatus, headers: { "content-type": "application/json" } });
    if (url.endsWith("/api/v1/edge/commit")) return new Response("{}", { status: 200 });
    return backend ? backend(url, init) : new Response("", { status: 500 });
  };
  return passes;
}

function put(headers = {}) {
  return new Request("https://s3.stored.cloud/b/k.bin", { method: "PUT", headers: { authorization: sig, "content-length": "5", ...headers }, body: "hello" });
}

test("passThrough: the client's request, never through the edge cache", async () => {
  const passes = recordFetch();
  const req = new Request("https://s3.stored.cloud/b/k", { method: "GET", headers: { range: "bytes=0-1", "if-none-match": '"x"' } });
  await passThrough(req);
  assert.equal(passes.length, 1);
  assert.equal(passes[0].init.cache, "no-store");
});

test("every pass-through site sends cache: no-store", async () => {
  const cases = [
    ["a request that is not the edge's (kind pass)", () => new Request("https://s3.stored.cloud/b/k", { method: "DELETE", headers: { authorization: sig } }), env, {}],
    ["no origin configured", () => put(), {}, {}],
    ["a refused CODEC_TAMPER", () => put(), { ...env, CODEC_TAMPER: "etag", ENVIRONMENT: "production" }, {}],
    ["an admit that is not 200", () => put(), env, { admitStatus: 503 }],
    ["an admit that answers edge:false", () => put(), env, { admit: { edge: false, reason: "flag_off" } }],
    ["a read the backend did not serve (fallback)", () => new Request("https://s3.stored.cloud/b/k.bin", { method: "GET", headers: { authorization: sig } }), env,
      { admit: { edge: true, admit_id: "a1", url: "https://backend.test/x", expect_status: 206, status: 200, send_headers: { range: "bytes=0-9" } }, backend: () => new Response("no", { status: 500 }) }],
  ];
  for (const [name, mk, e, opts] of cases) {
    const passes = recordFetch(opts);
    await worker.fetch(mk(), e, ctx);
    assert.equal(passes.length, 1, `${name}: one pass-through`);
    assert.equal(passes[0].init && passes[0].init.cache, "no-store", `${name}: cache no-store`);
  }
});
