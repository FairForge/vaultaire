# stored-s3-edge — the mover (WP-EDGE-1 Part A + Part B)

The Cloudflare Worker on `s3.stored.cloud` (plan 40.10 slice 1). Every S3
request arrives here. A signed **PutObject** or **UploadPart** of a tenant
with the `edge_ingest` flag, whose placement is a whole unsealed write of
at least `EDGE_MIN_BYTES`, is **admitted** by the control plane (`POST
/api/v1/edge/admit` on `VAULTAIRE_ORIGIN`: SigV4 verified on SLC, quota
reserved, a blob address minted, a presigned backend link that signs
`content-length` and the client's `Content-MD5`) and its body is streamed
by this Worker straight to the backend — never through SLC — and reported
at **commit** (`POST /api/v1/edge/commit`: the backend's answer, nothing of
the bytes), where the control plane writes the head row, the version row
and the bill exactly as a PUT through SLC would. A signed **GetObject**
(Part B: plain, or one Range) of such a tenant whose head row is a whole
unsealed object on an edge backend of at least `EDGE_MIN_BYTES` is admitted
the same way and streamed FROM the backend on a presigned ranged GET: the
backend's body is the client's response body, under the ORIGIN's response
headers the admit answered, and the commit is made at the backend's
headers (the Worker never sees the end of a stream it does not touch) —
`served` meters the bytes once, as the origin would have; any other
backend answer (Lyve's HEAD 200 / GET 500 of 2026-10-21, a 412, a 403) is
`fallback` and the origin serves the request itself (`fetch(request)` — a
GET has no body to lose), with its failover path. Everything else — HEAD
(cache-only on SLC), listings, deletes, Initiate / Complete / Abort, the
management API, a tenant without the flag, a sealed, chunked, SSE-C, tape,
public-bucket, versioned-by-id, conditional or multi-range read, a copy or
sub-resource write, a small body or range, a tenant past its egress
allowance with the throttle on (the origin paces it) — passes through to
the origin untouched (`fetch(request)`: the route's origin is SLC, the Host
stays, the signature still verifies there).

What it is not: a privacy feature (it moves Standard plaintext and Private
ciphertext alike and holds no key — contract lines 1, 4, 8), a verifier
(the control plane verifies every signature — Architecture Decision 8), a
hasher or a buffer: **no JavaScript touches the bytes** — the subrequest's
body is the request's own (`bench-results/EDGE-RELAY-2026-10-26.md`: any JS
in the byte path halves a stream to ~25 MB/s; pass-through runs at 42–54
MB/s per stream and ~150 MB/s at eight). Integrity is the backend's own
plus the client's `Content-MD5` signed into the link.

## Files

- `src/index.js` — the whole Worker, one file, published as-is
  (`no_bundle = true`): `sha256(src/index.js)` is the transparency log's
  `client_build` row for client `edge-ingest`
  (`internal/translog/clientbuilds/manifest.json`), and the deployed script
  downloads back byte-identical (`GET /accounts/{id}/workers/scripts/stored-s3-edge/content/v2`,
  the `index.js` part). The pure parts (`classify`, `describe`,
  `relayResponse`, `s3ErrorXML`, `backendErrorCode`) are exported for the
  tests.
- `test/*.test.js` — `node --test` (`make test-edge`; CI runs it): the
  classifier (the SDK's `x-id=` query is not a sub-resource — rclone's
  library; an aws-chunked body is the origin's; a signed GET is the edge's,
  a HEAD or a presigned URL never), the descriptor (every header
  lower-cased, hop-by-hop dropped, the client's Authorization INSIDE the
  JSON), the error relay, the read half (`readResponse`: the backend's body
  under the origin's headers with the client's status; `readFetchInit`:
  exactly the signed headers, `cache: "no-store"`; `readSucceeded` /
  `readCommit`: served at `expect_status`, fallback on anything else).
- `wrangler.toml` — name `stored-s3-edge`, route `s3.stored.cloud/*` on the
  stored.cloud zone of the PRODUCTION account, observability on, var
  `VAULTAIRE_ORIGIN`.

## Configuration

| | |
|---|---|
| `VAULTAIRE_ORIGIN` (var) | where admit and commit go: `https://stored.ge` — the SLC host whose certificate is valid, NOT `s3.stored.cloud` (a call to the route's own host would loop into this Worker) |
| `VAULTAIRE_EDGE_TOKEN` (secret) | the `EDGE_SERVICE_TOKEN` of the box's `.env`, set with `wrangler secret put VAULTAIRE_EDGE_TOKEN < file` — never on a command line |

The control plane accepts the token only from a Cloudflare peer
(`clientip.PeerIsCloudflare`) unless `EDGE_TRUST_ANY_PEER=true` (labs).

## The wire

Admit body: `{op: "put"|"part", method, host, path (raw), query (raw),
headers: {lower-cased name: value}, client_ip (CF-Connecting-IP),
request_id}`. Answers (always HTTP 200): `{edge:false, reason}` →
`fetch(request)`; `{edge:false, refusal:{status, headers, body}}` → relayed
to the client verbatim (the S3 XML the origin would have sent);
`{edge:true, admit_id, url, method, send_headers, backend, storage_key,
expect:{md5}, expires_at}` → the move. An admit that
does not answer within 10 s, or answers 5xx, is a pass-through (the origin
is the fallback, never a dropped upload; one log line).

Commit body: `{admit_id, outcome: ok|backend_error|edge_error,
backend_status, backend_code (the backend's XML <Code>; the subrequest's
error on edge_error), backend_etag, duration_ms}` — the backend's answer,
nothing of the bytes. The answer is the client's: `{status, headers,
body}`. A lost answer is retried once after 2 s (idempotent on the admit
id); still none → the client gets 503 SlowDown + Retry-After 30 and the
control plane's reaper settles the admit.

A read (Part B): the admit answers `{edge:true, admit_id, url, method:
"GET", send_headers (exactly the signed headers: `range` always — the
exact byte span, the whole object's for a whole read — and `if-match`
where the row's digest is provably the backend's ETag), expect_status (206:
what the backend must answer), status (the client's: 200, or 206 for its
own Range), response_headers (the ORIGIN's set: ETag, Content-Type,
Content-Length, Content-Range, x-amz-storage-class, x-amz-meta-*, the
version id, the lock headers …)}`. The Worker fetches the link with
`cache: "no-store"` and, at the backend's headers, answers the client with
the backend's body under `response_headers` and `status`, and commits
`{admit_id, outcome: served|fallback|error, backend_status,
backend_content_length, duration_ms}` in the background
(`ctx.waitUntil`; one retry). A backend status other than `expect_status`,
or no answer, is a fallback: the client's request goes to the origin
unchanged. A mid-body backend failure is a cut body, as at the origin.

aws-chunked bodies (`x-amz-content-sha256: STREAMING-*`, what aws-cli v2
sends over TLS) pass through to the origin: decoding them would put
JavaScript in the byte path. `docs/API.md` tells aws-cli users to set
`request_checksum_calculation = when_required`.

## Deploy

```
cd edge/s3-ingest
npx -y wrangler@4.151.3 deploy          # CLOUDFLARE_API_TOKEN = the user token, CLOUDFLARE_ACCOUNT_ID = the production account
npx -y wrangler@4.151.3 secret put VAULTAIRE_EDGE_TOKEN < /path/to/token   # once, and after a rotation
```

Then the transparency-log row: `shasum -a 256 src/index.js` must equal the
manifest row's `sha256` for the deployed version (the Deploy job rebuilds
every row with `scripts/repro-check-clients.sh`; `translog-verify -client
<downloaded index.js>` verifies a download against the log).

## The lab (docs/reviews/WP-EDGE-1A.md, WP-EDGE-1B.md)

`wrangler dev --local --port 8787 --local-upstream 127.0.0.1:8099` against
a local origin on `:8099`, with `.dev.vars` holding `VAULTAIRE_ORIGIN` and
`VAULTAIRE_EDGE_TOKEN` (the origin booted with the same token and
`EDGE_TRUST_ANY_PEER=true`). Clients use the dev server as an **HTTP
proxy** (`HTTP_PROXY=http://127.0.0.1:8787`, endpoint
`http://127.0.0.1:8099`): the Host they sign is the origin's, the
pass-through reaches it, and the Worker sees the request as the edge would.
Go clients must set the proxy explicitly (`http.ProxyURL`) — Go never
proxies a loopback destination from the environment.

The 100 MB request limit is the zone plan's (Free), answered by Cloudflare
before this Worker runs: keep parts at 64 MiB (`docs/API.md`'s rclone
snippet). Nothing here works around it.
