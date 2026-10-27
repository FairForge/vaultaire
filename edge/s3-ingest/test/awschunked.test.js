// WP-EDGE-2 Part C: aws-cli's DEFAULT upload at the edge. The mover forwards
// an aws-chunked body untouched under the header-signed request the control
// plane minted (D-EDGE-30); the codec decodes the framing and verifies the
// trailer (D-EDGE-31).
import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import worker, { classify } from "../src/index.js";
import { CRC32, CRC64NVME, awsChunkedDecoder, base64 } from "../src/awschunked.js";
import { runCodec } from "../src/codec.js";
import { loadGo, noWasm } from "./wasm-loader.js";

const sig = "AWS4-HMAC-SHA256 Credential=VKX/20261027/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=00";
const env = { VAULTAIRE_ORIGIN: "https://origin.test", VAULTAIRE_EDGE_TOKEN: "tok" };
const realFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = realFetch;
});

const enc = new TextEncoder();

/** frame is the aws-chunked encoding of body in pieces of `chunk` bytes with a `name` trailer of value `value`. */
function frame(body, chunk, name, value) {
  const parts = [];
  for (let o = 0; o < body.length; o += chunk) {
    const n = Math.min(chunk, body.length - o);
    parts.push(enc.encode(n.toString(16) + "\r\n"), body.subarray(o, o + n), enc.encode("\r\n"));
  }
  parts.push(enc.encode(`0\r\n${name}:${value}\r\n\r\n`));
  const out = new Uint8Array(parts.reduce((a, p) => a + p.length, 0));
  let w = 0;
  for (const p of parts) {
    out.set(p, w);
    w += p.length;
  }
  return out;
}

function stream(bytes, piece = 65536) {
  let o = 0;
  return new ReadableStream({
    pull(c) {
      if (o >= bytes.length) return c.close();
      c.enqueue(bytes.slice(o, o + piece));
      o += piece;
    },
  });
}

function streamingPut(url, framed, extra = {}) {
  return new Request(url, {
    method: "PUT",
    duplex: "half",
    body: stream(framed),
    headers: {
      authorization: sig,
      "x-amz-content-sha256": "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
      "content-encoding": "aws-chunked",
      "x-amz-decoded-content-length": "11",
      "x-amz-trailer": "x-amz-checksum-crc64nvme",
      ...extra,
    },
  });
}

test("classify: aws-cli's default upload (unsigned payload, a checksum trailer, no Content-Length) is the edge's; signed chunks are not", () => {
  const framed = enc.encode("b\r\nhello world\r\n0\r\nx-amz-checksum-crc64nvme:jSnVw/bqjr4=\r\n\r\n");
  assert.equal(classify(streamingPut("https://s3.stored.cloud/b/k.bin", framed)).kind, "put");
  assert.equal(classify(streamingPut("https://s3.stored.cloud/b/k.bin?partNumber=2&uploadId=u", framed)).kind, "part");
  assert.deepEqual(classify(streamingPut("https://s3.stored.cloud/b/k.bin", framed, { "x-amz-content-sha256": "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" })), { kind: "pass", reason: "aws_chunked" },
    "chunk signatures are made with the client's key: never forwarded");
  const noDecoded = streamingPut("https://s3.stored.cloud/b/k.bin", framed);
  noDecoded.headers.delete("x-amz-decoded-content-length");
  assert.deepEqual(classify(noDecoded), { kind: "pass", reason: "no_length" });
});

test("the mover forwards an aws-chunked body untouched under the admit's header-signed request, chunked, and commits the backend's answer", async () => {
  const framed = enc.encode("b\r\nhello world\r\n0\r\nx-amz-checksum-crc64nvme:jSnVw/bqjr4=\r\n\r\n");
  const req = streamingPut("https://s3.stored.cloud/b/k.bin", framed);
  const signed = {
    authorization: "AWS4-HMAC-SHA256 Credential=BACKEND/20261027/us-west-1/s3/aws4_request, SignedHeaders=content-encoding;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-trailer, Signature=ab",
    "x-amz-date": "20261027T120000Z", "x-amz-content-sha256": "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "content-encoding": "aws-chunked",
    "x-amz-decoded-content-length": "11", "x-amz-trailer": "x-amz-checksum-crc64nvme", host: "s3.us-west-1.backend.test",
  };
  const calls = [];
  const commits = [];
  globalThis.fetch = async (input, init) => {
    const url = String(input instanceof Request ? input.url : input);
    if (url.endsWith("/api/v1/edge/admit")) {
      return new Response(JSON.stringify({ edge: true, admit_id: "a-1", url: "https://s3.us-west-1.backend.test/stored/t-x/k@1", method: "PUT", headers: signed, streaming_trailer: "x-amz-checksum-crc64nvme", content_length: 11 }), { status: 200 });
    }
    if (url.endsWith("/api/v1/edge/commit")) {
      commits.push(JSON.parse(init.body));
      return new Response(JSON.stringify({ status: 200, headers: { ETag: '"5eb63bbbe01eeed093cb22bb8f5acdc3"' }, body: "" }), { status: 200 });
    }
    calls.push({ url, init });
    return new Response(null, { status: 200, headers: { etag: '"5eb63bbbe01eeed093cb22bb8f5acdc3"' } });
  };
  const resp = await worker.fetch(req, env, { waitUntil: () => {} });
  assert.equal(resp.status, 200);
  assert.equal(calls.length, 1);
  const { url, init } = calls[0];
  assert.equal(url, "https://s3.us-west-1.backend.test/stored/t-x/k@1");
  assert.equal(init.method, "PUT");
  assert.equal(init.body, req.body, "the request's own body stream — no JavaScript reads it");
  assert.equal(init.duplex, "half");
  const h = new Headers(init.headers);
  for (const [k, v] of Object.entries(signed)) if (k !== "host") assert.equal(h.get(k), v, k);
  assert.equal(h.get("host"), null, "the host is the URL's");
  assert.equal(h.get("transfer-encoding"), "chunked", "the client sent no Content-Length");
  assert.equal(commits.length, 1);
  assert.equal(commits[0].outcome, "ok");
  assert.equal(commits[0].backend_status, 200);
  assert.equal(commits[0].backend_etag, '"5eb63bbbe01eeed093cb22bb8f5acdc3"');
});

test("CRC64NVME and CRC32 tables: the standard check values, aws-sdk-go-v2's vector, uneven pieces", () => {
  const pat = (n) => {
    const b = new Uint8Array(n);
    for (let i = 0; i < n; i++) b[i] = (i * 7 + n) & 255;
    return b;
  };
  for (const [name, b, w64, w32] of [
    ["empty", new Uint8Array(0), "AAAAAAAAAAA=", "AAAAAA=="],
    ["123456789", enc.encode("123456789"), "rosUhgp5mIg=", "y/Q5Jg=="], // CRC-64/NVME check 0xae8b14860a799888
    ["hello world", enc.encode("hello world"), "jSnVw/bqjr4=", "DUoRhQ=="], // aws-sdk-go-v2 service/internal/checksum
    ["pattern 1000003", pat(1000003), "E+we8FCnTdU=", "jL+cEg=="], // Go hash/crc64, table 0x9a6c9329ac4bc9b5
    ["pattern 3 MiB", pat(3 << 20), "cKqytnsA+ps=", "CWo1Dw=="],
  ]) {
    const c = new CRC64NVME();
    const d = new CRC32();
    for (let o = 0; o < b.length; o += 37) {
      c.update(b.subarray(o, o + 37));
      d.update(b.subarray(o, o + 37));
    }
    assert.equal(base64(c.bytes()), w64, `crc64nvme ${name}`);
    assert.equal(base64(d.bytes()), w32, `crc32 ${name}`);
  }
});

async function decode(framed, opts, piece) {
  const d = awsChunkedDecoder(opts);
  const out = [];
  await stream(framed, piece).pipeThrough(d.stream).pipeTo(new WritableStream({ write: (p) => { out.push(p.slice()); } }));
  return { bytes: new Uint8Array(Buffer.concat(out)), result: d.result };
}

test("the decoder: the bench's captured shape (1 MiB chunks, CRC64NVME) at every piece size, the decoded bytes out, the trailer verified", async () => {
  const body = new Uint8Array(3 * (1 << 20) + 12345).map((_, i) => (i * 31 + 7) & 255);
  const want = base64(new CRC64NVME().update(body).bytes());
  const framed = frame(body, 1 << 20, "x-amz-checksum-crc64nvme", want);
  assert.equal(new TextDecoder().decode(framed.subarray(0, 8)), "100000\r\n", "aws-cli's first size line");
  for (const piece of [1, 7, 4096, 1 << 20, framed.length]) {
    if (piece === 1 && framed.length > 1 << 16) {
      const { bytes } = await decode(frame(body.subarray(0, 50000), 1 << 14, "x-amz-checksum-crc64nvme", base64(new CRC64NVME().update(body.subarray(0, 50000)).bytes())), { decodedLength: 50000, trailer: "x-amz-checksum-crc64nvme" }, 1);
      assert.equal(bytes.length, 50000);
      continue;
    }
    const { bytes, result } = await decode(framed, { decodedLength: body.length, trailer: "x-amz-checksum-crc64nvme" }, piece);
    assert.equal(bytes.length, body.length, `piece ${piece}`);
    assert.equal(createHash("md5").update(bytes).digest("hex"), createHash("md5").update(body).digest("hex"));
    assert.equal(result.computed, want);
  }
  // CRC32 (older aws-cli).
  const c32 = base64(new CRC32().update(body).bytes());
  const r32 = await decode(frame(body, 1 << 20, "x-amz-checksum-crc32", c32), { decodedLength: body.length, trailer: "x-amz-checksum-crc32" }, 65536);
  assert.equal(r32.result.computed, c32);
});

test("the decoder refuses a wrong trailer (checksum_mismatch) and bad framing (aws_chunked_framing)", async () => {
  const body = enc.encode("hello world");
  const bad = frame(body, 11, "x-amz-checksum-crc64nvme", "AAAAAAAAAAA=");
  await assert.rejects(decode(bad, { decodedLength: 11, trailer: "x-amz-checksum-crc64nvme" }), (e) => e.code === "checksum_mismatch");
  const good = "b\r\nhello world\r\n0\r\nx-amz-checksum-crc64nvme:jSnVw/bqjr4=\r\n\r\n";
  for (const [why, text, len] of [
    ["no trailer", "b\r\nhello world\r\n0\r\n\r\n", 11],
    ["short", good.slice(0, 20), 11],
    ["a bad size line", "zz\r\nhello world\r\n0\r\n\r\n", 11],
    ["no CRLF after the data", "b\r\nhello worldXX0\r\n\r\n", 11],
    ["another decoded length", good, 12],
    ["bytes after the end", good + "x", 11],
  ]) {
    await assert.rejects(decode(enc.encode(text), { decodedLength: len, trailer: "x-amz-checksum-crc64nvme" }), (e) => e.code === "aws_chunked_framing", why);
  }
});

test("runCodec decodes an aws-chunked body (the chunker and the MD5 see decoded bytes); a wrong trailer commits checksum_mismatch and relays the 400", { skip: noWasm }, async () => {
  const chunker = await loadGo("chunker.wasm", "storedChunker");
  const e2ee = await loadGo("e2ee.wasm", "storedE2EE");
  const body = chunker.goldenBody(4631, 3 << 20).value;
  const pub = e2ee.publicKey(new Uint8Array(32).fill(7)).value;
  const admit = { edge: true, admit_id: "a-2", streaming_trailer: "x-amz-checksum-crc64nvme", content_length: body.length, codec: { version: 1, kem_public_key: Buffer.from(pub).toString("base64") } };
  const run = async (trailerValue) => {
    const commits = [];
    let puts = 0;
    const cp = async (path, b) => {
      if (path.endsWith("/index")) {
        return new Response(JSON.stringify({ hits: [], misses: b.chunks.map((c) => ({ hash: c.hash, url: "https://backend.test/x/" + c.hash, send_headers: {}, sealed_length: 1186 + c.size + 16 * Math.max(1, Math.ceil(c.size / (1 << 20))) })) }), { status: 200 });
      }
      commits.push(b);
      if (b.outcome === "checksum_mismatch") {
        return new Response(JSON.stringify({ status: 400, headers: { "Content-Type": "application/xml" }, body: "<Error><Code>XAmzContentChecksumMismatch</Code></Error>" }), { status: 200 });
      }
      return new Response(JSON.stringify({ status: 200, headers: {}, body: "" }), { status: 200 });
    };
    const fakeFetch = async (url, init) => {
      puts++;
      const r = init.body.getReader();
      for (;;) if ((await r.read()).done) break;
      return new Response(null, { status: 200, headers: { etag: '"' + "0".repeat(32) + '"' } });
    };
    const req = { url: "https://s3.stored.cloud/b/k.bin", headers: new Headers({ "x-amz-decoded-content-length": String(body.length) }), body: stream(frame(body, 1 << 20, "x-amz-checksum-crc64nvme", trailerValue)) };
    const relay = await runCodec(req, admit, {}, {}, { chunker, e2ee, controlPlane: cp, fetch: fakeFetch, requestId: "r" });
    return { relay, commits, puts };
  };
  const good = await run(base64(new CRC64NVME().update(body).bytes()));
  assert.equal(good.relay.status, 200);
  assert.equal(good.commits.length, 1);
  assert.equal(good.commits[0].outcome, "ok");
  assert.equal(good.commits[0].body_md5, createHash("md5").update(body).digest("hex"), "the MD5 of the DECODED body");
  assert.equal(good.commits[0].chunks.reduce((a, c) => a + c.size, 0), body.length);

  const bad = await run("AAAAAAAAAAA=");
  assert.equal(bad.relay.status, 400, "the control plane's answer, relayed");
  assert.match(bad.relay.body, /XAmzContentChecksumMismatch/);
  assert.deepEqual(bad.commits.map((c) => c.outcome), ["checksum_mismatch"]);
});
