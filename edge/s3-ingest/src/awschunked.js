// aws-chunked decoding at the edge (WP-EDGE-2 Part C, D-EDGE-31): the
// framing aws-cli ≥ 2.23 sends by default (x-amz-content-sha256:
// STREAMING-UNSIGNED-PAYLOAD-TRAILER, Content-Encoding: aws-chunked) taken
// apart in the codec's byte path — the chunk-size lines, the data, the
// terminal chunk, the trailer — and the trailer's checksum VERIFIED over the
// decoded bytes (CRC64NVME, aws-cli's default since 2.23, or CRC32). The
// codec already touches every byte (it chunks them), so the decode costs the
// table CRC only; a trailer that does not match is the client's 400
// XAmzContentChecksumMismatch, relayed from the control plane. The edge
// verifies what the origin never did (RE-6). The MOVER path never decodes:
// it forwards the framing to the backend untouched under a header-signed
// request (D-EDGE-30).
//
// The framing (the SPEED bench § 1, aws-sdk-go-v2's own test vector):
//
//   <hex size>\r\n<size bytes>\r\n   … repeated (aws-cli: 100000\r\n + 1 MiB)
//   0\r\n
//   x-amz-checksum-crc64nvme:<base64 of the big-endian CRC>\r\n
//   \r\n
//
// A chunk-size line may carry ";…" extensions (ignored: the unsigned
// variant carries no chunk signatures). Bounds: a size line ≤ 4 KiB, the
// trailer ≤ 16 KiB; anything else is an aws_chunked framing error.

// --- CRC64NVME (CRC-64/NVME: poly 0xAD93D23594C93659, reflected
// 0x9A6C9329AC4BC9B5, init and xorout all ones — aws-sdk-go-v2's
// crc64.MakeTable(0x9a6c9329ac4bc9b5)). The 64-bit state is two uint32
// halves; the table is built once at module load, slicing by 8. ---

const POLY_LO = 0xac4bc9b5;
const POLY_HI = 0x9a6c9329;

function makeTables64() {
  const lo = new Uint32Array(8 * 256);
  const hi = new Uint32Array(8 * 256);
  for (let n = 0; n < 256; n++) {
    let l = n;
    let h = 0;
    for (let k = 0; k < 8; k++) {
      const bit = l & 1;
      l = ((l >>> 1) | (h << 31)) >>> 0;
      h = h >>> 1;
      if (bit) {
        l = (l ^ POLY_LO) >>> 0;
        h = (h ^ POLY_HI) >>> 0;
      }
    }
    lo[n] = l;
    hi[n] = h;
  }
  for (let t = 1; t < 8; t++) {
    for (let n = 0; n < 256; n++) {
      const pl = lo[(t - 1) * 256 + n];
      const ph = hi[(t - 1) * 256 + n];
      const i = pl & 0xff;
      lo[t * 256 + n] = (((pl >>> 8) | (ph << 24)) ^ lo[i]) >>> 0;
      hi[t * 256 + n] = ((ph >>> 8) ^ hi[i]) >>> 0;
    }
  }
  return { lo, hi };
}

const T64 = makeTables64();

/** CRC64NVME is an incremental CRC-64/NVME. */
export class CRC64NVME {
  constructor() {
    this.lo = 0xffffffff;
    this.hi = 0xffffffff;
  }
  update(b) {
    const TL = T64.lo;
    const TH = T64.hi;
    let lo = this.lo;
    let hi = this.hi;
    let i = 0;
    const n = b.length;
    for (; i + 8 <= n; i += 8) {
      const a0 = (lo ^ (b[i] | (b[i + 1] << 8) | (b[i + 2] << 16) | (b[i + 3] << 24))) >>> 0;
      const a1 = (hi ^ (b[i + 4] | (b[i + 5] << 8) | (b[i + 6] << 16) | (b[i + 7] << 24))) >>> 0;
      const k7 = a0 & 0xff, k6 = (a0 >>> 8) & 0xff, k5 = (a0 >>> 16) & 0xff, k4 = a0 >>> 24;
      const k3 = a1 & 0xff, k2 = (a1 >>> 8) & 0xff, k1 = (a1 >>> 16) & 0xff, k0 = a1 >>> 24;
      lo = TL[7 * 256 + k7] ^ TL[6 * 256 + k6] ^ TL[5 * 256 + k5] ^ TL[4 * 256 + k4] ^ TL[3 * 256 + k3] ^ TL[2 * 256 + k2] ^ TL[256 + k1] ^ TL[k0];
      hi = TH[7 * 256 + k7] ^ TH[6 * 256 + k6] ^ TH[5 * 256 + k5] ^ TH[4 * 256 + k4] ^ TH[3 * 256 + k3] ^ TH[2 * 256 + k2] ^ TH[256 + k1] ^ TH[k0];
    }
    for (; i < n; i++) {
      const k = (lo ^ b[i]) & 0xff;
      lo = (((lo >>> 8) | (hi << 24)) ^ TL[k]) >>> 0;
      hi = ((hi >>> 8) ^ TH[k]) >>> 0;
    }
    this.lo = lo >>> 0;
    this.hi = hi >>> 0;
    return this;
  }
  /** bytes is the final CRC, big-endian (the trailer's encoding). */
  bytes() {
    const lo = (this.lo ^ 0xffffffff) >>> 0;
    const hi = (this.hi ^ 0xffffffff) >>> 0;
    return new Uint8Array([hi >>> 24, (hi >>> 16) & 255, (hi >>> 8) & 255, hi & 255, lo >>> 24, (lo >>> 16) & 255, (lo >>> 8) & 255, lo & 255]);
  }
}

// --- CRC32 (zlib's: IEEE 802.3, reflected 0xEDB88320), slicing by 8. ---

function makeTables32() {
  const t = new Uint32Array(8 * 256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? (0xedb88320 ^ (c >>> 1)) >>> 0 : c >>> 1;
    t[n] = c >>> 0;
  }
  for (let s = 1; s < 8; s++) {
    for (let n = 0; n < 256; n++) {
      const p = t[(s - 1) * 256 + n];
      t[s * 256 + n] = ((p >>> 8) ^ t[p & 0xff]) >>> 0;
    }
  }
  return t;
}

const T32 = makeTables32();

/** CRC32 is an incremental zlib CRC-32. */
export class CRC32 {
  constructor() {
    this.c = 0xffffffff;
  }
  update(b) {
    const T = T32;
    let c = this.c;
    let i = 0;
    const n = b.length;
    for (; i + 8 <= n; i += 8) {
      const a = (c ^ (b[i] | (b[i + 1] << 8) | (b[i + 2] << 16) | (b[i + 3] << 24))) >>> 0;
      c = T[7 * 256 + (a & 0xff)] ^ T[6 * 256 + ((a >>> 8) & 0xff)] ^ T[5 * 256 + ((a >>> 16) & 0xff)] ^ T[4 * 256 + (a >>> 24)] ^
        T[3 * 256 + b[i + 4]] ^ T[2 * 256 + b[i + 5]] ^ T[256 + b[i + 6]] ^ T[b[i + 7]];
    }
    for (; i < n; i++) c = (T[(c ^ b[i]) & 0xff] ^ (c >>> 8)) >>> 0;
    this.c = c >>> 0;
    return this;
  }
  bytes() {
    const c = (this.c ^ 0xffffffff) >>> 0;
    return new Uint8Array([c >>> 24, (c >>> 16) & 255, (c >>> 8) & 255, c & 255]);
  }
}

export function base64(bytes) {
  let s = "";
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
  return btoa(s);
}

/** checksumFor is the CRC a trailer names, or null when the edge does not verify that algorithm. */
export function checksumFor(trailerName) {
  switch ((trailerName || "").toLowerCase()) {
    case "x-amz-checksum-crc64nvme":
      return new CRC64NVME();
    case "x-amz-checksum-crc32":
      return new CRC32();
    default:
      return null;
  }
}

/** The two trailers the edge verifies; any other is the origin's (the control plane's rule too). */
export const VERIFIED_TRAILERS = ["x-amz-checksum-crc64nvme", "x-amz-checksum-crc32"];

/** AWSChunkedError carries the commit outcome a decode failure maps to:
 * "checksum_mismatch" (the client's 400 XAmzContentChecksumMismatch) or
 * "aws_chunked_framing" (an edge_error: the body is not what it said). */
export class AWSChunkedError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

const CR = 13;
const LF = 10;
const MAX_SIZE_LINE = 4096;
const MAX_TRAILER = 16384;

/**
 * awsChunkedDecoder is the codec's decoder of an aws-chunked body: a
 * TransformStream from the framed bytes to the DECODED bytes (each data
 * span is passed on as a view of the piece it arrived in — the cutter
 * copies), the trailer's CRC computed over them, and at the end the framing
 * closed, the decoded length equal to decodedLength and the trailer equal to
 * the CRC — or the stream errors with an AWSChunkedError. `result` holds
 * what was verified once the stream has flushed.
 */
export function awsChunkedDecoder({ decodedLength, trailer }) {
  const name = (trailer || "").toLowerCase();
  const crc = checksumFor(name);
  if (!crc) throw new AWSChunkedError("aws_chunked_framing", `edge codec: the trailer ${trailer || "(none)"} is not one the edge verifies`);
  const result = { algorithm: name, decoded: 0, expected: "", computed: "" };
  let state = "size"; // size | data | crlf | trailer | done
  let line = new Uint8Array(256);
  let lineLen = 0;
  let remaining = 0;
  let crlfSeen = 0;
  let trailerBytes = 0;
  const trailers = {};
  const fail = (msg) => {
    throw new AWSChunkedError("aws_chunked_framing", `edge codec: aws-chunked framing: ${msg}`);
  };
  const pushLineByte = (b, limit) => {
    if (lineLen >= limit) fail("a line is too long");
    if (lineLen === line.length) {
      const g = new Uint8Array(Math.min(limit, line.length * 2));
      g.set(line);
      line = g;
    }
    line[lineLen++] = b;
  };
  const lineText = () => {
    if (lineLen < 1 || line[lineLen - 1] !== CR) fail("a line does not end in CRLF");
    let s = "";
    for (let i = 0; i < lineLen - 1; i++) s += String.fromCharCode(line[i]);
    lineLen = 0;
    return s;
  };
  const stream = new TransformStream({
    transform(piece, ctl) {
      let i = 0;
      const n = piece.length;
      while (i < n) {
        switch (state) {
          case "size": {
            const b = piece[i++];
            if (b !== LF) {
              pushLineByte(b, MAX_SIZE_LINE);
              break;
            }
            const text = lineText();
            const hexPart = text.split(";")[0].trim();
            if (!/^[0-9a-fA-F]{1,16}$/.test(hexPart)) fail(`chunk size ${JSON.stringify(text.slice(0, 32))}`);
            const size = parseInt(hexPart, 16);
            if (!Number.isSafeInteger(size)) fail("chunk size out of range");
            if (size === 0) state = "trailer";
            else {
              remaining = size;
              state = "data";
            }
            break;
          }
          case "data": {
            const take = Math.min(remaining, n - i);
            const view = piece.subarray(i, i + take);
            crc.update(view);
            ctl.enqueue(view);
            result.decoded += take;
            if (decodedLength >= 0 && result.decoded > decodedLength) fail(`more than the ${decodedLength} decoded bytes declared`);
            remaining -= take;
            i += take;
            if (remaining === 0) {
              state = "crlf";
              crlfSeen = 0;
            }
            break;
          }
          case "crlf": {
            const b = piece[i++];
            if (b !== (crlfSeen === 0 ? CR : LF)) fail("a chunk's data is not followed by CRLF");
            if (++crlfSeen === 2) state = "size";
            break;
          }
          case "trailer": {
            const b = piece[i++];
            if (++trailerBytes > MAX_TRAILER) fail("the trailer is too long");
            if (b !== LF) {
              pushLineByte(b, MAX_TRAILER);
              break;
            }
            const text = lineText();
            if (text === "") {
              state = "done";
              break;
            }
            const colon = text.indexOf(":");
            if (colon < 1) fail("a trailer line has no name");
            trailers[text.slice(0, colon).trim().toLowerCase()] = text.slice(colon + 1).trim();
            break;
          }
          case "done":
            fail("bytes after the end of the trailer");
        }
      }
    },
    flush() {
      if (state !== "done") fail(`the body ended inside the ${state === "trailer" ? "trailer" : "framing"}`);
      if (decodedLength >= 0 && result.decoded !== decodedLength) fail(`${result.decoded} decoded bytes, x-amz-decoded-content-length ${decodedLength}`);
      result.expected = trailers[name] || "";
      result.computed = base64(crc.bytes());
      if (!result.expected) fail(`no ${name} trailer`);
      if (result.expected !== result.computed) {
        throw new AWSChunkedError("checksum_mismatch", `edge codec: the ${name} trailer is ${result.expected}, the body's is ${result.computed}`);
      }
    },
  });
  return { stream, result };
}
