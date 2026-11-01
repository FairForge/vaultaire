import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";
import worker from "../src/index.js";

const env = { STORED_API: "https://origin.test", STORED_S3: "https://s3.test", MCP_RESOURCE: "https://mcp.test/mcp" };
const KEY = "sk_live_" + "A".repeat(43);
let calls;

// A fake origin: the management API and the S3 endpoint.
beforeEach(() => {
  calls = [];
  globalThis.fetch = async (url, init = {}) => {
    const u = new URL(url);
    const headers = Object.fromEntries(Object.entries(init.headers || {}).map(([k, v]) => [k.toLowerCase(), v]));
    calls.push({ method: init.method || "GET", url: u.origin + u.pathname + u.search, headers, body: init.body });
    const j = (status, b) => new Response(JSON.stringify(b), { status, headers: { "content-type": "application/json" } });
    if (u.origin === env.STORED_API) {
      // The origin's issuer is its CANONICAL host, which need not be the
      // host the Worker reaches it on (prod: stored.ge until WP-DOMAIN-1).
      if (u.pathname === "/.well-known/oauth-authorization-server") return j(200, { issuer: "https://issuer.test" });
      if (u.pathname === "/api/v1/manage/buckets" && init.method === "POST") return j(201, { name: JSON.parse(init.body).name, mode: "standard", versioning: "Enabled" });
      if (u.pathname === "/api/v1/manage/buckets") return j(200, { data: [{ name: "notes", mode: "standard" }] });
      if (u.pathname.startsWith("/api/v1/manage/buckets/") && init.method === "DELETE")
        return j(403, { error: { code: "approval_required", message: "deleting a bucket needs the account holder's approval", approval_url: "https://origin.test/dashboard/agents#a1" } });
      if (u.pathname === "/api/v1/manage/keys" && init.method === "POST") return j(201, { id: "k1", key: "VLTCALL", secret: "s3cr3t" });
      if (u.pathname === "/api/v1/manage/keys/k1" && init.method === "DELETE") return j(200, { deleted: true });
    }
    if (u.origin === env.STORED_S3) {
      if ((init.method || "GET") === "PUT") return new Response(null, { status: 200 });
      if ((init.method || "GET") === "GET") return new Response("hello", { status: 200, headers: { "content-length": "5" } });
    }
    return j(404, { error: { code: "not_found" } });
  };
});

const post = (body, token = KEY) => worker.fetch(new Request(env.MCP_RESOURCE, {
  method: "POST", headers: { "content-type": "application/json", ...(token ? { authorization: "Bearer " + token } : {}) }, body: JSON.stringify(body) }), env);

test("no grant: 401 naming the protected-resource metadata, which names the origin's OAuth server", async () => {
  const res = await post({ jsonrpc: "2.0", id: 1, method: "initialize", params: {} }, "");
  assert.equal(res.status, 401);
  assert.match(res.headers.get("www-authenticate"), /resource_metadata="https:\/\/mcp\.test\/\.well-known\/oauth-protected-resource"/);
  const meta = await (await worker.fetch(new Request("https://mcp.test/.well-known/oauth-protected-resource"), env)).json();
  assert.deepEqual(meta.authorization_servers, ["https://issuer.test"], "the AS is named by the issuer the origin publishes (RFC 8414 § 3.3)");
  assert.equal(meta.resource, "https://mcp.test/mcp");
});

test("initialize, tools/list, and a notification answers 202", async () => {
  const init = await (await post({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18" } })).json();
  assert.equal(init.result.serverInfo.name, "stored");
  const list = await (await post({ jsonrpc: "2.0", id: 2, method: "tools/list" })).json();
  assert.ok(list.result.tools.some((t) => t.name === "delete_bucket"));
  const n = await post({ jsonrpc: "2.0", method: "notifications/initialized" });
  assert.equal(n.status, 202);
});

test("a bucket tool carries the grant's own key", async () => {
  const out = await (await post({ jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "create_bucket", arguments: { name: "agent-made" } } })).json();
  assert.match(out.result.content[0].text, /Created agent-made/);
  assert.equal(calls[0].headers.authorization, "Bearer " + KEY);
});

test("an approval the origin asks for reaches the agent with its link", async () => {
  const out = await (await post({ jsonrpc: "2.0", id: 4, method: "tools/call", params: { name: "delete_bucket", arguments: { name: "notes" } } })).json();
  assert.equal(out.result.isError, true);
  assert.match(out.result.content[0].text, /approval_required/);
  assert.match(out.result.content[0].text, /https:\/\/origin\.test\/dashboard\/agents#a1/);
});

test("an object write mints a one-call key on the one bucket, signs with it and revokes it", async () => {
  const out = await (await post({ jsonrpc: "2.0", id: 5, method: "tools/call", params: { name: "put_object", arguments: { bucket: "notes", key: "a/b.txt", content: "hi" } } })).json();
  assert.ok(!out.result.isError, JSON.stringify(out));
  const mint = calls.find((c) => c.url.endsWith("/api/v1/manage/keys") && c.method === "POST");
  assert.deepEqual(JSON.parse(mint.body).bucket_scope, ["notes"]);
  assert.deepEqual(JSON.parse(mint.body).permissions, ["PutObject"]);
  const put = calls.find((c) => c.url === "https://s3.test/notes/a/b.txt");
  assert.match(put.headers.authorization, /^AWS4-HMAC-SHA256 Credential=VLTCALL\//);
  assert.ok(calls.some((c) => c.url.endsWith("/api/v1/manage/keys/k1") && c.method === "DELETE"), "the call's key is revoked");
  const get = await (await post({ jsonrpc: "2.0", id: 6, method: "tools/call", params: { name: "get_object", arguments: { bucket: "notes", key: "a/b.txt" } } })).json();
  assert.equal(get.result.content[0].text, "hello");
});
