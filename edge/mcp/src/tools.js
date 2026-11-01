// The tools an agent sees. Bucket tools are management-API calls with the
// grant's own key; object tools mint a short-lived S3 key for the one call
// (bounded by the grant's ceiling and caps: the server's rule), sign with it
// and revoke it before they answer. Nothing here decides what an agent may
// do: the origin does (approval_required, the Private mode, the caps), and a
// refusal is handed back to the agent word for word.

import { signS3, objectPath } from "./sigv4.js";

const MAX_OBJECT_BYTES = 4 << 20;

export const TOOLS = [
  { name: "list_buckets", description: "List the account's buckets (Private buckets are never listed to an agent).",
    inputSchema: { type: "object", properties: {}, additionalProperties: false } },
  { name: "create_bucket", description: "Create a bucket. Buckets an agent creates have versioning on, so a mistake is a version back.",
    inputSchema: { type: "object", properties: { name: { type: "string" } }, required: ["name"], additionalProperties: false } },
  { name: "delete_bucket", description: "Delete an empty bucket. Needs the account holder's approval: the answer then carries an approval_url to show them.",
    inputSchema: { type: "object", properties: { name: { type: "string" } }, required: ["name"], additionalProperties: false } },
  { name: "list_objects", description: "List the files in a bucket.",
    inputSchema: { type: "object", properties: { bucket: { type: "string" }, prefix: { type: "string" } }, required: ["bucket"], additionalProperties: false } },
  { name: "put_object", description: "Write a file (up to 4 MiB): text as is, or base64 with encoding=base64.",
    inputSchema: { type: "object", properties: { bucket: { type: "string" }, key: { type: "string" }, content: { type: "string" },
      encoding: { type: "string", enum: ["utf-8", "base64"] }, content_type: { type: "string" } }, required: ["bucket", "key", "content"], additionalProperties: false } },
  { name: "get_object", description: "Read a file (up to 4 MiB): text when it is UTF-8, else base64.",
    inputSchema: { type: "object", properties: { bucket: { type: "string" }, key: { type: "string" } }, required: ["bucket", "key"], additionalProperties: false } },
  { name: "delete_object", description: "Delete a file (in a versioned bucket the earlier versions stay).",
    inputSchema: { type: "object", properties: { bucket: { type: "string" }, key: { type: "string" } }, required: ["bucket", "key"], additionalProperties: false } },
];

class ToolError extends Error {}

async function api(env, token, method, path, body) {
  const res = await fetch(env.STORED_API + path, {
    method, headers: { authorization: "Bearer " + token, "content-type": "application/json", "user-agent": "stored-mcp/0.1.0" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let json = null;
  try { json = text ? JSON.parse(text) : {}; } catch { /* not JSON */ }
  if (!res.ok) {
    const e = json && json.error ? json.error : { message: text.slice(0, 500) };
    const parts = [`${res.status} ${e.code || ""}`.trim(), e.message || ""];
    if (e.approval_url) parts.push(`Ask the account holder to approve it at ${e.approval_url} — then call the tool again.`);
    throw new ToolError(parts.filter(Boolean).join(": "));
  }
  return json;
}

// withCallKey mints an S3 key for one call on one bucket, runs fn with it,
// and revokes it whatever fn did.
async function withCallKey(env, token, bucket, permissions, fn) {
  const expires = new Date(Date.now() + 15 * 60 * 1000).toISOString();
  const k = await api(env, token, "POST", "/api/v1/manage/keys",
    { name: "mcp: one call", permissions, bucket_scope: [bucket], expires_at: expires });
  try {
    return await fn(k.key, k.secret);
  } finally {
    await api(env, token, "DELETE", "/api/v1/manage/keys/" + encodeURIComponent(k.id)).catch(() => {});
  }
}

async function s3(env, method, bucket, key, ak, sk, body, contentType) {
  const url = env.STORED_S3 + objectPath(bucket, key);
  const headers = {};
  if (body) {
    headers["content-length"] = String(body.length);
    if (contentType) headers["content-type"] = contentType;
  }
  const signed = await signS3({ method, url, headers, body: body || new Uint8Array(), accessKey: ak, secretKey: sk });
  const res = await fetch(url, { method, headers: signed, body: body || undefined });
  if (!res.ok && res.status !== 204) {
    const text = await res.text();
    const code = (text.match(/<Code>([^<]+)<\/Code>/) || [])[1] || "";
    const msg = (text.match(/<Message>([^<]+)<\/Message>/) || [])[1] || "";
    const cap = res.headers.get("x-stored-cap");
    throw new ToolError(`${res.status} ${code}${msg ? ": " + msg : ""}${cap ? ` (the ${cap} cap)` : ""}`);
  }
  return res;
}

function b64decode(s) {
  const bin = atob(s);
  return Uint8Array.from(bin, (c) => c.charCodeAt(0));
}

function b64encode(bytes) {
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(s);
}

const text = (t) => ({ content: [{ type: "text", text: t }] });

export async function callTool(env, token, name, args = {}) {
  try {
    switch (name) {
      case "list_buckets": {
        const out = await api(env, token, "GET", "/api/v1/manage/buckets");
        return text(JSON.stringify((out.data || []).map((b) => ({ name: b.name, mode: b.mode, region: b.region })), null, 2));
      }
      case "create_bucket": {
        const out = await api(env, token, "POST", "/api/v1/manage/buckets", { name: args.name });
        return text(`Created ${out.name} (${out.mode}, versioning ${out.versioning || "on"}).`);
      }
      case "delete_bucket":
        await api(env, token, "DELETE", "/api/v1/manage/buckets/" + encodeURIComponent(args.name));
        return text(`Deleted ${args.name}.`);
      case "list_objects": {
        const q = args.prefix ? "?prefix=" + encodeURIComponent(args.prefix) : "";
        const out = await api(env, token, "GET", "/api/v1/manage/buckets/" + encodeURIComponent(args.bucket) + "/objects" + q);
        return text(JSON.stringify((out.data || []).map((o) => ({ key: o.key, size: o.size, last_modified: o.last_modified })), null, 2));
      }
      case "put_object": {
        const body = args.encoding === "base64" ? b64decode(args.content) : new TextEncoder().encode(args.content);
        if (body.length > MAX_OBJECT_BYTES) throw new ToolError("a file through this tool is at most 4 MiB; use an S3 client for larger ones");
        await withCallKey(env, token, args.bucket, ["PutObject"], (ak, sk) =>
          s3(env, "PUT", args.bucket, args.key, ak, sk, body, args.content_type || "application/octet-stream"));
        return text(`Wrote ${args.key} (${body.length} bytes) to ${args.bucket}.`);
      }
      case "get_object": {
        const bytes = await withCallKey(env, token, args.bucket, ["GetObject"], async (ak, sk) => {
          const res = await s3(env, "GET", args.bucket, args.key, ak, sk);
          if (Number(res.headers.get("content-length") || 0) > MAX_OBJECT_BYTES) {
            await res.body?.cancel();
            throw new ToolError("the file is over 4 MiB; use an S3 client for it");
          }
          return new Uint8Array(await res.arrayBuffer());
        });
        try {
          return text(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
        } catch {
          return text("base64:" + b64encode(bytes));
        }
      }
      case "delete_object":
        await withCallKey(env, token, args.bucket, ["DeleteObject"], (ak, sk) => s3(env, "DELETE", args.bucket, args.key, ak, sk));
        return text(`Deleted ${args.key} from ${args.bucket}.`);
      default:
        return { isError: true, content: [{ type: "text", text: `unknown tool ${name}` }] };
    }
  } catch (e) {
    if (e instanceof ToolError) return { isError: true, content: [{ type: "text", text: e.message }] };
    throw e;
  }
}
