// AWS Signature Version 4 for one S3 request, with WebCrypto (Workers and
// Node 20+). The object tools sign with a short-lived S3 key the agent grant
// mints for the call, so the grant's ceiling and caps apply to every byte.

const enc = new TextEncoder();

const toHex = (buf) => [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, "0")).join("");

export async function sha256Hex(data) {
  const bytes = typeof data === "string" ? enc.encode(data) : data;
  return toHex(await crypto.subtle.digest("SHA-256", bytes));
}

async function hmac(key, msg) {
  const k = await crypto.subtle.importKey("raw", typeof key === "string" ? enc.encode(key) : key,
    { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return crypto.subtle.sign("HMAC", k, enc.encode(msg));
}

// amzDate is YYYYMMDDTHHMMSSZ.
export function amzDate(d) {
  return d.toISOString().replace(/[-:]/g, "").replace(/\.\d{3}/, "");
}

// signS3 returns the headers to send: the given headers plus x-amz-date,
// x-amz-content-sha256 and Authorization. `headers` keys are lower-case and
// are all signed (host is added from the URL). The path is used as escaped
// (S3 never double-encodes).
export async function signS3({ method, url, headers = {}, body = new Uint8Array(), accessKey, secretKey, region = "us-east-1", now = new Date() }) {
  const u = new URL(url);
  const date = amzDate(now);
  const day = date.slice(0, 8);
  const payloadHash = await sha256Hex(body);
  const h = { ...headers, host: u.host, "x-amz-date": date, "x-amz-content-sha256": payloadHash };
  const names = Object.keys(h).map((k) => k.toLowerCase()).sort();
  const lower = Object.fromEntries(Object.entries(h).map(([k, v]) => [k.toLowerCase(), String(v).trim()]));
  const query = [...u.searchParams.entries()]
    .map(([k, v]) => [encodeRFC3986(k), encodeRFC3986(v)])
    .sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : a[1] < b[1] ? -1 : 1))
    .map(([k, v]) => `${k}=${v}`).join("&");
  const canonical = [method, u.pathname || "/", query, names.map((n) => `${n}:${lower[n]}\n`).join(""), names.join(";"), payloadHash].join("\n");
  const scope = `${day}/${region}/s3/aws4_request`;
  const toSign = ["AWS4-HMAC-SHA256", date, scope, await sha256Hex(canonical)].join("\n");
  let k = await hmac("AWS4" + secretKey, day);
  k = await hmac(k, region);
  k = await hmac(k, "s3");
  k = await hmac(k, "aws4_request");
  const sig = toHex(await hmac(k, toSign));
  const out = { ...headers, "x-amz-date": date, "x-amz-content-sha256": payloadHash };
  out.authorization = `AWS4-HMAC-SHA256 Credential=${accessKey}/${scope}, SignedHeaders=${names.join(";")}, Signature=${sig}`;
  return out;
}

export function encodeRFC3986(s) {
  return encodeURIComponent(s).replace(/[!'()*]/g, (c) => "%" + c.charCodeAt(0).toString(16).toUpperCase());
}

// objectPath is /<bucket>/<key> with each key segment escaped as S3 expects.
export function objectPath(bucket, key) {
  return "/" + encodeRFC3986(bucket) + "/" + key.split("/").map(encodeRFC3986).join("/");
}
