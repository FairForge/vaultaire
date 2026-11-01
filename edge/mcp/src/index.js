// stored-mcp — Stored's MCP server on mcp.stored.cloud (WP-AGENT-1, Stage 1a
// Prompt 55; docs/ACCOUNTS.md § Agents). Streamable HTTP (JSON responses, no
// server-initiated stream) at POST /mcp. Its bearer is an agent grant's key:
// a client without one gets 401 with the protected-resource metadata
// (RFC 9728), which names the origin's OAuth server (OAuth 2.1 + PKCE,
// dynamic registration); the person consents on the dashboard with TOTP and
// the token endpoint answers the grant's key. `stored mcp` bridges a local
// stdio client to this endpoint.

import { TOOLS, callTool } from "./tools.js";

export const VERSION = "0.1.0";
const PROTOCOL = "2025-06-18";

const json = (status, body, extra = {}) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", "cache-control": "no-store", ...extra } });

function metadataURL(env) {
  return new URL("/.well-known/oauth-protected-resource", env.MCP_RESOURCE).toString();
}

function bearer(request) {
  const h = request.headers.get("authorization") || "";
  const m = h.match(/^Bearer\s+(sk_(live|test)_[0-9A-Za-z]+)$/);
  return m ? m[1] : "";
}

async function rpc(env, token, msg) {
  const { id, method, params = {} } = msg;
  const ok = (result) => ({ jsonrpc: "2.0", id, result });
  switch (method) {
    case "initialize":
      return ok({ protocolVersion: params.protocolVersion === "2025-03-26" ? "2025-03-26" : PROTOCOL,
        capabilities: { tools: { listChanged: false } }, serverInfo: { name: "stored", version: VERSION },
        instructions: "Stored is S3-compatible storage. You act for the account holder who connected you: what could lose or expose data asks them first (the tool answers with an approval link to show them). Rehearse with the sandbox key before live data." });
    case "ping":
      return ok({});
    case "tools/list":
      return ok({ tools: TOOLS });
    case "tools/call":
      return ok(await callTool(env, token, params.name, params.arguments || {}));
    default:
      if (id === undefined) return null; // a notification
      return { jsonrpc: "2.0", id, error: { code: -32601, message: `method ${method} not found` } };
  }
}

// originIssuer is the issuer of the origin's authorization-server metadata,
// or "" when the origin did not answer one.
async function originIssuer(env) {
  try {
    const res = await fetch(env.STORED_API + "/.well-known/oauth-authorization-server");
    if (!res.ok) return "";
    const doc = await res.json();
    return typeof doc.issuer === "string" && /^https:\/\/[^/]+$/.test(doc.issuer) ? doc.issuer : "";
  } catch {
    return "";
  }
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/.well-known/oauth-protected-resource" || url.pathname === "/.well-known/oauth-protected-resource/mcp") {
      // The authorization server is named by the issuer the origin itself
      // publishes: a client fetches <issuer>/.well-known/… and refuses a
      // document whose issuer differs (RFC 8414 § 3.3), and the canonical
      // host is the origin's setting, not this Worker's.
      const issuer = await originIssuer(env);
      return json(200, { resource: env.MCP_RESOURCE, authorization_servers: [issuer || env.STORED_API], bearer_methods_supported: ["header"],
        scopes_supported: ["stored"], resource_documentation: (issuer || env.STORED_API) + "/docs/api#agents" },
        { "cache-control": issuer ? "public, max-age=300" : "no-store" });
    }
    if (url.pathname === "/.well-known/oauth-authorization-server") {
      // Clients that look on the resource's host get the origin's document.
      const res = await fetch(env.STORED_API + "/.well-known/oauth-authorization-server");
      return new Response(res.body, { status: res.status, headers: { "content-type": "application/json" } });
    }
    if (url.pathname !== "/mcp") return json(404, { error: "not found" });
    if (request.method !== "POST") return json(405, { error: "POST JSON-RPC to /mcp" }, { allow: "POST" });
    const token = bearer(request);
    if (!token) {
      return json(401, { error: "invalid_token", error_description: "connect through Stored's OAuth flow" },
        { "www-authenticate": `Bearer resource_metadata="${metadataURL(env)}"` });
    }
    let body;
    try {
      body = await request.json();
    } catch {
      return json(400, { jsonrpc: "2.0", id: null, error: { code: -32700, message: "parse error" } });
    }
    const batch = Array.isArray(body);
    const out = [];
    for (const msg of batch ? body : [body]) {
      const r = await rpc(env, token, msg);
      if (r) out.push(r);
    }
    if (out.length === 0) return new Response(null, { status: 202 });
    return json(200, batch ? out : out[0]);
  },
};
