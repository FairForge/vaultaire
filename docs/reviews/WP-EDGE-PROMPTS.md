# WP-EDGE — the 0×-SLC ingest plane: worker prompts

*Plan section 40.10 (`docs/IMPLEMENTATION_PLAN.md`, `grep -n "### 40.10"`). Written 2026-10-26 by the owner session that set 40.10's order ("do it now"). Each prompt is copy-pasted into a fresh Claude Code session; it carries everything that session needs. Line numbers below are from main at `e9cb4241` (2026-10-26) — re-grep before editing, they drift.*

---

## Stage 1a Prompt 44: WP-EDGE-1 Part A — the mover (PutObject + UploadPart through a Worker on `s3.stored.cloud`)

**Baseline measured after this prompt was written — read it before Step 0 (`bench-results/EDGE-RELAY-2026-10-26.md`).** It overrides four lines of the prompt below: (1) **the Worker never touches bytes on the write path** — `fetch(url, {method: "PUT", headers: send_headers, body: request.body})`, no `tee`, no `DigestStream`, no `FixedLengthStream` (any JS in the byte path halves throughput to ~25 MB/s per stream; pass-through runs at 42–54 MB/s per stream from SLC and 150 MB/s at 8 streams; the backends accepted the pass-through body with `Content-Length`, no 411); so the commit carries no `md5_hex`/`sha256_hex` and the "backend ETag against the Worker's MD5" check is dropped — integrity is the backend's own plus the client's `Content-MD5` when it sends one (signed into the link, enforced by both backends); a client `x-amz-content-sha256` hex claim is NOT verified at the edge (note it in the docs: the origin path verifies it, the edge path relies on `Content-MD5`); (2) **`aws-chunked` bodies pass through to the origin** (`{edge:false}` when `x-amz-content-sha256` starts with `STREAMING-`), and the docs tell aws-cli v2 users to set `request_checksum_calculation = when_required` in their profile; no decoder in the Worker in Part A; (3) **`EDGE_MIN_BYTES`** (default `4MiB`, a size or `off`): an admit below it answers `{edge:false}` with outcome `pass_through_small` — the origin is faster for small bodies and their bytes do not matter; (4) Step 0.3 shrinks to: a pass-through PUT of 64 MiB reaches the backend with `Content-Length` (it did) and the 100 MB body limit holds on `workers.dev` too (it did: HTML 413). Everything else in the prompt stands. Document the per-customer expectation (40–55 MB/s per stream through the edge, ~150 MB/s with 8 parts in flight; SLC stays a host for more) in docs/API.md with the rclone snippet.

```
Stage 1a Prompt 44: WP-EDGE-1 Part A — the mover: PutObject and UploadPart leave SLC

Continue from WP-P42 (the S3 secrets wrapped under the tenant key, migration 108,
#969/#970, deployed; the Prompt 43 owner-attended items B1–B5 run beside you and
do not collide) and from the docs PRs #976 + the 2026-10-26 one (plan 40.10, its
order, F-37, 10.3's catalogue filter; docs-only, no deploy).
Most recent merged PR: run `gh pr list --state merged --limit 5` — it moved.
Open PRs that matter: none in internal/api's write path at hand-out time; the
dependabot bumps (#941 …) do not collide. Re-check with `gh pr list --state open`.

Read first:
- docs/IMPLEMENTATION_PLAN.md § 40.10 (grep "### 40.10"): "The principle", "Proven
  2026-10-25", "Slice 1 — the mover", "What the move changes elsewhere", "The
  invariant and the order (2026-10-26)". You build slice 1's WRITE half only.
- docs/ARCHITECTURE_CONTRACT.md lines 1 (mode is a property of the surface — the
  Worker moves Standard plaintext and Private ciphertext alike, it never decides),
  4 (keys only through the one hierarchy — NO key at the edge in this part), 7
  (any act that could widen access is in the transparency log — the Worker's build
  is a client_build row), 8 ("can't read" is client-side only — the Worker is not
  a privacy feature and the docs must not call it one), 11 (one domain: the
  Worker lives on s3.stored.cloud). Cite 1, 4, 7 and 11 in the PR body.
- docs/surfaces/ENGINE.md § The first ten (this is engine work: the data plane
  gains a second host; the Developer surface inherits an endpoint, nothing else).
- CLAUDE.md non-negotiables that bite: Architecture Decision 7 (client IP only
  through internal/clientip — the Worker reports the client's IP and the admit
  trusts it only from a Cloudflare peer with the service token); Decision 8 (a
  request carrying S3 credentials is the S3 API's before any route — the admit
  and commit calls carry the client's Authorization INSIDE a JSON body, never as
  their own header, so the gate never captures them; pin this with a test);
  Decision 4 (never buffer — the Worker streams, the control plane never sees a
  body byte); "a write that fails queues its address on every backend that was
  sent the body" (placeObject, object_write_shared.go:205) — the same rule for a
  Worker upload whose commit fails.
- docs/CLOUDFLARE.md § Accounts and zones (both zones Free plan → 100 MB per
  request, returned by Cloudflare before any Worker runs), § What the edge did,
  § Rules learned. docs/DEPLOY.md § the two slots (the flag stays OFF until the
  new build is the active slot — see Architecture notes).
- internal/api/direct_upload.go: the WHOLE file (47–300). It is the public-bucket
  shape of exactly this flow: PresignPut → the client PUTs → complete verifies
  with Stat, checks the lock, reserves quota, commitBlobWrite, retireOrphanWrite
  on failure. Part A generalises it to every unsealed whole-object write on the
  private backends, with the Worker in the client's place.
- docs/reviews/WP-P40.md, WP-P41.md, WP-P42.md "Decisions" and "Residuals"
  (RP57…RP62) for the conventions of the last three prompts (the ONE reader
  rule, the gate, the drill) — none is a dependency.

Lane: Engine (with one Developer-surface sentence: the docs' rclone snippet).
Next migration: 109 (latest is 108_credential_secrets_wrapped.sql) — claim it in
your PR title; check `ls internal/database/migrations | tail -1` and open PRs
first. Three PRs: (A1) the control plane — migration 109, admit/commit, the
presigning drivers, the reaper, the translog row; (A2) the Worker under
edge/s3-ingest/ + its node tests + docs/CLOUDFLARE.md; (A3) the docs PR — plan
40.10 status, STATUS.md, CLAUDE.md rows (env, flag, routes, job, table), the
rclone snippet, /legal/sub-processors sentence, your docs/reviews/WP-EDGE-1A.md
note. A1 and A2 can go to the merge queue together; A3 after the live proof.

Scope: a Cloudflare Worker on s3.stored.cloud takes every S3 request. PutObject
and UploadPart of a tenant with the `edge_ingest` flag, whose placement is an
UNSEALED WHOLE write, are admitted by the control plane (SigV4 verified on SLC,
quota reserved, a blob address minted, a presigned backend URL signing
content-length + the client's checksum), streamed by the Worker straight to the
backend (iDrive primary, idrive-<region>, lyve — never R2, never a sealed tier),
and committed by the control plane exactly as a PUT through SLC would be (head
row, version row, lock, bill, access log). Every other request — GET, HEAD,
listings, deletes, Initiate/Complete/Abort, the management API, a tenant without
the flag, a sealed or chunked placement — passes through to the origin untouched.
Closes 40.10 slice 1 (writes); Part B (reads) is the next prompt. Prod has no
master key, so on prod EVERY whole-object write is eligible today; the sealed and
chunked ones keep routing to SLC through placement.go (40.10's "trap" bullet:
nothing seals Standard by default until slice 2 exists — do not add that here).

Step 0 — probes before any code (write the answers into WP-EDGE-1A.md):
0.1 aws-sdk-go-v2 presign: does s3.NewPresignClient(...).PresignPutObject with
    ContentLength and ContentMD5 set put `content-length` and `content-md5` in
    X-Amz-SignedHeaders? Print the URL's query from a unit test against a fake
    endpoint. If not, presign with aws/signer/v4's Signer.PresignHTTP over a
    hand-built *http.Request carrying those headers (that is what the owner's
    2026-10-25 stdlib probe did and both backends enforced it). Same for
    PresignUploadPart.
0.2 Lyve us-west-1 and iDrive us-central-1: the ETag of (a) a single presigned
    PUT and (b) one UploadPart equals the body's MD5 (hex, quoted). Use the lab
    pairs (.private/idrive-keys-2026-10-20.env, the Mac's `lyve-admin` profile
    with endpoint https://s3.us-west-1.global.lyve.seagate.com) under a
    t-_lab/ prefix and delete what you wrote. Never print a key.
0.3 Workers runtime: `crypto.DigestStream("MD5")` and ("SHA-256") exist and
    stream (wrangler dev, one script); `FixedLengthStream` exists; a fetch() to
    an https backend with a ReadableStream body and a known Content-Length is
    sent with Content-Length (not chunked) — read the backend's access answer
    (a 411 means it was chunked).
0.4 The stored.cloud zone: SSL mode (Full vs Full strict) and whether the origin
    certificate HAProxy serves covers s3.stored.cloud (`openssl s_client
    -servername s3.stored.cloud -connect <SLC IP>:443`). DNS for s3.stored.cloud
    does not exist yet (CLOUDFLARE.md § DNS (stored.cloud) — target state). YOU
    create it (the operator items at the end): the zone-scoped user token in
    ~/fairforge/.cloudflare-creds.env (CLOUDFLARE_ZONE_TOKEN, the zone ids, the
    prod account id) covers DNS, Workers routes and Workers scripts on the
    production account. Prove the code on a lab origin FIRST, then do the prod
    items.
0.5 Does auth.(*Auth).ValidateRequest (internal/auth/handlers.go:105) hash the
    body when x-amz-content-sha256 is a hex digest? If yes, the described-request
    verifier below must accept the digest as a CLAIM and the commit must check
    it (the Worker computes the SHA-256 as it streams).
0.6 wrangler: a single-file ES-module Worker with `no_bundle = true` deploys as
    the file itself, so sha256(src/index.js) is the deployed script's hash and
    `wrangler` can download it back (the translog row's proof). Confirm on the
    bench account (isaacv17@gmail.com; token in ~/fairforge/.cloudflare-creds.env,
    never in the repo, never printed).

What to build

1. Migration 109_edge_ingest.sql (additive; the previous build ignores it):
   - edge_admits (id TEXT PK — 26 base32 like a blob address; tenant_id, bucket,
     key, op TEXT 'put'|'part', upload_id TEXT NULL, part_number INT NULL,
     backend TEXT, storage_key TEXT (the minted blob address; for a part: the
     upload's recorded storage_key), content_length BIGINT, content_md5 TEXT NULL
     (base64, as signed), content_sha256 TEXT NULL (the client's claim, hex),
     content_type TEXT, attrs JSONB (x-amz-meta-*, cache-control, disposition,
     encoding, language, expires, redirect — what handlePutObject stores),
     storage_class TEXT, floor TEXT, reserved_bytes BIGINT, reserved_floor TEXT,
     client_ip TEXT, request_id TEXT, state TEXT 'pending'|'committed'|'failed'|
     'expired', backend_status INT NULL, backend_etag TEXT NULL, created_at,
     expires_at, finished_at). Index (state, expires_at); (tenant_id). Tenant
     data: erased with the account (TestErasedTablesCoverSchema), excluded from
     the export with the reason "transient admission rows" (TestExportSections
     CoverSchema), a sweep bucket source is NOT needed (the address is in
     retired_blobs on failure).
   - multipart_uploads: backend_upload_id TEXT NULL (set once, under the upload's
     lock, by the FIRST edge part's admit), edge BOOLEAN NOT NULL DEFAULT false.
   - multipart_parts: backend_etag TEXT NULL — a part with it is an edge part
     (no staged file). Read 024/068/091/096 for today's columns first.

2. Service token (cmd/vaultaire/main.go + internal/api): EDGE_SERVICE_TOKEN read
   like the master key — credential $CREDENTIALS_DIRECTORY/edge_service_token,
   then EDGE_SERVICE_TOKEN_FILE, then the variable; set but unusable = fatal;
   one Info line `edge service token: credential|file|env` (never the value).
   Unset = the edge routes are not mounted (one Info line) and the flag does
   nothing. EDGE_TRUST_ANY_PEER=true (labs only, a Warn line at boot) lifts the
   Cloudflare-peer requirement below.

3. The described-request verifier (internal/auth): add
   `func (a *Auth) ValidateDescribed(ctx context.Context, d RequestDescriptor,
   clientIP string) (accessKey string, scope *KeyScope, err error)` where
   RequestDescriptor = {Method, Host, Path (raw, as received), RawQuery,
   Headers http.Header (every header the client sent, lower-cased by the Worker;
   the verifier picks the ones SignedHeaders names)}. ValidateRequest becomes a
   thin wrapper that builds the descriptor from the live request and
   clientip.FromRequest(r): ONE verifier, two entrances. The key's IP allow-list
   (review R1-01) is applied to clientIP in both. Test: the same verdict from
   both entrances for the SigV4 header, SigV4A header (403 unsupported), SigV2,
   presigned-query and a tampered-Host shape (reuse wp_p40_s3_gate_test.go's
   shapes or a subset); a descriptor whose Host differs from the signed one
   fails. If 0.5 said the verifier hashes the body: the descriptor carries the
   digest as a claim and the verifier does not read a body.

4. The presigning drivers (internal/engine + internal/drivers): a new interface
   `engine.EdgeWriter` — PresignPut(ctx, container, artifact string, p
   EdgePresign) (url string, signedHeaders []string, err); CreateMultipart(ctx,
   container, artifact, contentType string) (uploadID string, err);
   PresignUploadPart(ctx, container, artifact, uploadID string, partNumber int,
   p EdgePresign) (url, signedHeaders, err); CompleteMultipart(ctx, container,
   artifact, uploadID string, parts []EdgePart) (etag string, err);
   AbortMultipart(ctx, container, artifact, uploadID string) error; where
   EdgePresign = {ContentLength int64, ContentMD5 string (base64 or ""),
   ContentType string, TTL time.Duration} and EdgePart = {Number int, ETag
   string}. Implement ONCE as a helper over the aws-sdk-go-v2 client and attach
   it to the fixed-bucket driver (idrive.go:119 NewFixedBucketS3Driver — so
   idrive, idrive-<region>, wasabi) and to LyveDriver (lyve.go:26). Keys through
   each driver's own key() (the t-<tenant>/<container>/<artifact> rule;
   drivers.ErrNoTenant when the context names none). R2 keeps DirectUploader
   (direct_upload.go:47) — do not unify in this part. Geyser, permafrost, sync,
   blomp, sia, local: not EdgeWriters (placement never offers them).
   `TestEdgePresign_SignsLengthAndChecksum`: every URL's X-Amz-SignedHeaders
   contains content-length and, when ContentMD5 is set, content-md5; a
   host-only presign (build one in the test) FAILS the same checker — the
   checker (`edgePresignSigns(url, headers...) error`) runs on every URL the
   admit hands out, so the owner's 2026-10-25 finding can never regress.

5. Admit — POST /api/v1/edge/admit (internal/api/edge_ingest.go, mounted in
   server.go next to the management routes only when the token is loaded;
   NOT an admin route — TestAdminReadRoutes_AllClassified is untouched):
   - Auth of the call: header X-Vaultaire-Edge-Token compared constant-time, and
     the peer is Cloudflare (export a `clientip.PeerIsCloudflare(r) bool` over
     trustedPeer, clientip.go:92; EDGE_TRUST_ANY_PEER lifts it). Otherwise 404
     (not 401: the route does not exist to anyone else).
   - Body: {op: "put"|"part", method, host, path, query, headers: {name: value}
     (lower-cased, every client header minus hop-by-hop; `content-length`
     present; aws-chunked bodies carry x-amz-decoded-content-length and
     x-amz-trailer — the admit uses the DECODED length as the size and the
     backend never sees aws-chunked), client_ip, request_id}.
   - Steps, in order, each a typed refusal that the Worker RELAYS to the client
     as the S3 XML the origin would have sent (status + body + the usual
     headers): ValidateDescribed → the scope check handlePutObject does
     (s3.go:1143 ff.) → the tenant's `edge_ingest` flag (flags_wiring.go:31
     pattern; internal/flags/service.go:155; the dashboard table
     dashboard/handlers/admin_flags.go:47 gets a row) — OFF = {edge:false} →
     bucket exists, reserved names (bucket_reserved.go), key length (857 bytes
     rule) → the ONE placement decision (placement.go ~180–240: build the same
     request handlePutObject builds with Size = decoded length; ANYTHING but
     placeWhole-unsealed = {edge:false}: chunked, sealed, SSE-C, sealed tier,
     a refusal — the origin then answers the same refusal itself) → the backend
     the placement names must be an EdgeWriter, else {edge:false} → object lock
     (s3_lock.go:476 checkObjectLock) → quota: reserveQuota (quota_accounting.go
     :54) for a PUT; for a part the upload's reservation rule
     (multipart_reservation.go — the same statement UploadPart runs, under the
     upload's lock) → mint storage_key = newBlobAddress(key) (blob_address.go:56)
     for a PUT; for a part: the upload row's storage_key (Complete reuses it),
     and if backend_upload_id is NULL create the backend multipart NOW under the
     upload's lock (one creator; a concurrent first part waits and reuses) and
     set edge = true → presign (TTL 15 min; content-md5 signed when the client
     sent Content-MD5; content-type when sent) → INSERT edge_admits pending,
     expires_at = now + 20 min → answer {edge:true, admit_id, url, method:"PUT",
     send_headers: {content-length, content-md5?, content-type?}, backend,
     storage_key, expect: {md5: bool, sha256_claim: hex|null}}.
   - A refusal AFTER the reservation releases it in the same function (defer
     pattern); nothing pending is left for a refused admit.
   - Metrics: vaultaire_data_plane_admits_total{host="edge",op,outcome=
     admitted|pass_through|refused|error}.

6. Commit — POST /api/v1/edge/commit (same auth): body {admit_id, outcome:
   "ok"|"backend_error"|"client_abort"|"edge_error", backend_status,
   backend_etag, bytes, md5_hex, sha256_hex, trailer_ok (bool|null),
   duration_ms}. Under the admit row's FOR UPDATE:
   - already committed/failed → the SAME answer again (idempotent; the Worker
     retries a lost answer), never a second row.
   - outcome ≠ ok, or bytes ≠ content_length → state failed, release the
     reservation, retireOrphanWrite(... storage_key, backend) (blob_address.go
     :358: the backend may have kept the body — placeObject's rule), answer the
     S3 error the origin gives for that failure (503 ServiceUnavailable XML for
     a backend 5xx with Retry-After; 400 IncompleteBody for a short body).
   - outcome ok: integrity — when content_md5 was signed the backend enforced it
     (0.2's finding); when it was not, backend_etag (trimmed) must equal md5_hex,
     else DELETE the blob on the backend, release, state failed, 400 BadDigest;
     when content_sha256 is a hex claim it must equal sha256_hex, else the same
     with 400 XAmzContentSHA256Mismatch; a trailer_ok=false never reaches here
     (the Worker refuses before commit) but is refused the same way if it does.
   - PUT: commitBlobWrite (blob_flip.go:132) with Upsert =
     upsertWholeObjectHeadRow (object_write_shared.go:143) on the admit's
     backend/floor/attrs, Version = the version row when versioned, Bill = the
     reservation — the flip's displaced-blob rule applies unchanged; the access
     log row the origin would have written (access_log.go; op PutObject, the
     admit's client_ip, bytes, status 200, request_id) — grep its columns, add
     nothing; answer {status:200, headers:{ETag, x-amz-version-id?,
     x-amz-request-id, x-amz-checksum-crc32 when the client sent one}}.
   - part: INSERT the multipart_parts row with backend_etag (and size), the
     reservation settled as UploadPart's is; answer {status:200, headers:{ETag}}.
   - errSSECRequiredAtCommit (a bucket made Private meanwhile, RP33-1 b): the
     direct_upload.go:280 handling — remove the blob, 400 ssec_required.
   - Metrics: vaultaire_data_plane_bytes_total{host="edge",op} += bytes on ok.

7. Multipart at the origin for an edge upload (s3_multipart.go): Complete
   (completeMultipartUpload:760) with backend_upload_id set calls the
   EdgeWriter's CompleteMultipart with the parts' backend ETags in the client's
   order (the small call SLC may make), ETag = the backend's `-N` value (it is
   the md5-of-md5s on both backends — assert in 0.2), body_md5 NULL (094's rule:
   "an R2 direct multipart — served size-checked"), then the same commit as
   today (the upload's storage_key, lock, bill). A STAGED part on an edge upload
   (handleUploadPart:301 reached directly) is 400 InvalidRequest "this upload is
   fed through the edge; send the part through s3.stored.cloud" — state in the
   note why (one upload is one path). Abort (1336) and the multipart_reaper
   abort the backend upload too (AbortMultipart; a failed abort is a note, the
   reaper retries on its next pass; log it). ListParts of an edge upload lists
   the edge parts (backend_etag) like staged ones.

8. The admit reaper: job `edge_admit_reaper`, every 5 min through the one
   scheduler (jobs.go): pending rows past expires_at → release the reservation,
   retireOrphanWrite for a PUT's address on its backend (the body may have
   landed with no commit), state expired; terminal rows older than 7 days
   deleted. job_runs result {expired, released_bytes, retired, deleted}.
   vaultaire_edge_admit_reaper_total{outcome}.

9. The transparency-log row (internal/translog/clientbuilds/manifest.json): the
   client list grows by `edge-ingest` (the closed list in clientbuilds — extend
   it and its test); the row = version, source commit, sha256(edge/s3-ingest/
   src/index.js), toolchain "wrangler <pinned>", recipe "no_bundle: the file is
   the script". scripts/repro-check-clients.sh rebuilds it = hashes the file at
   the source commit; `translog-verify -client <downloaded script>` works on the
   file wrangler downloads. The row is appended at boot like the SDKs' (K4c-3b).
   40.10 said "edge_build, a new kind": it is NOT — the kind list stays closed
   (plan 40.10 "The invariant and the order" bullet says so).

10. The Worker (edge/s3-ingest/: wrangler.toml, src/index.js, test/*.test.js
    run by `node --test`, README.md): name `stored-s3-edge`, route
    `s3.stored.cloud/*` on the stored.cloud zone of the PRODUCTION account,
    `no_bundle = true`, observability on, vars VAULTAIRE_ORIGIN
    (https://stored.ge — where admit/commit go; the SLC host whose certificate
    is valid), secret VAULTAIRE_EDGE_TOKEN. Logic:
    - Not (PUT with an object key, no `?uploads`, no `?uploadId` for Initiate)
      → `return fetch(request)` (the route's origin = SLC, Host intact, the
      SigV4 signature still valid). Also pass through: no Authorization /
      presigned query (the origin answers 403 itself), OPTIONS, HEAD, GET.
    - PUT object / PUT ?partNumber&uploadId: build the descriptor (headers
      lower-cased, hop-by-hop dropped, client_ip = request.headers.get(
      "CF-Connecting-IP"), request_id = crypto.randomUUID()), POST admit with
      X-Vaultaire-Edge-Token. {edge:false} → fetch(request). A refusal → relay
      status/headers/body verbatim. An admit that does not answer within 10 s or
      5xx → fetch(request) (the origin is the fallback, never a dropped upload;
      count it in a Worker log line).
    - Admitted: body = request.body; if x-amz-content-sha256 starts with
      STREAMING- (aws-chunked): pipe through the decoder (test/chunked.test.js:
      the `<hex size>;chunk-signature=…\r\n<bytes>\r\n` framing, the final
      0-size chunk, the trailer `x-amz-checksum-crc32:<base64>\r\n`, CRC32
      verified over the decoded bytes — IEEE, as aws-cli sends; a bad trailer
      aborts the backend PUT (cancel the fetch) and commits outcome
      "edge_error" so the client gets 400 BadDigest). Tee into
      crypto.DigestStream("MD5") and ("SHA-256"). Send with new
      FixedLengthStream(content_length) to `url` with send_headers only (never
      the client's Authorization or x-amz-* headers — the presign signed exactly
      send_headers + host). Count bytes.
    - Backend answered → POST commit {…}; relay commit's status/headers/body to
      the client. A commit that does not answer: retry once after 2 s with the
      same admit_id (idempotent); then answer the client 503 SlowDown
      Retry-After 30 (the reaper collects the admit).
    - Never log a body, a signature or the token; log one line per admitted
      request: op, tenant-less request_id, bytes, backend status, ms.
    - Memory: one isolate may hold several bodies; never read a body into
      memory (no arrayBuffer()).
    - test/: the chunked decoder (framing, trailer, CRC32, a truncated body),
      the descriptor builder (lower-case, hop-by-hop dropped, Authorization
      kept in the JSON), the error relay (S3 XML unchanged). Pure functions
      exported from src/index.js for the tests.

11. Docs (A3): CLAUDE.md rows (EDGE_SERVICE_TOKEN, EDGE_TRUST_ANY_PEER, the
    `edge_ingest` flag in the feature_flags row, edge_admits in the tables
    list, the job in job_runs' list, the two routes in Architecture Decision 8's
    exceptions — "the edge calls carry the client's Authorization inside a JSON
    body"); docs/CLOUDFLARE.md § What the edge does (the product Worker, the
    route, the secret, the deploy command); docs/API.md the s3.stored.cloud
    endpoint + the rclone snippet `--s3-upload-cutoff 64M --s3-chunk-size 64M`
    with the one sentence why (the 100 MB zone limit; rclone's default 200 MiB
    cutoff gets an HTML 413); /legal/sub-processors (43.8) gains the sentence
    that Cloudflare carries bytes in transit for the S3 endpoint (it proxies
    stored.ge already — a sentence, not a new processor; its test, if any);
    plan 40.10 status line; STATUS.md; docs/reviews/WP-EDGE-1A.md (Step 0
    answers, decisions D-EDGE-1…, residuals, the live-proof tables).

Tests (red first, at the handlers, own test DB; the structural ones must stay
green: TestSQLLiteralsMatchSchema, TestErasedTablesCoverSchema,
TestExportSectionsCoverSchema, TestAdminReadRoutes_AllClassified,
TestS3Gate_EveryHeaderTheVerifierAttemptsIsTheGates, the clientbuilds manifest
test, TestBackendEncryptionDoc_EveryDriverHasARow)
- TestEdgeRoutes_NotMountedWithoutToken: no token → 404 on both routes, boot
  Info line; with a token: 404 to a wrong token and to a non-Cloudflare peer.
- TestEdgeRoutes_NeverCapturedByTheS3Gate: a POST to /api/v1/edge/admit with
  the client's SigV4 Authorization INSIDE the JSON and none on the request
  reaches the handler; one WITH an AWS4 header on the request is the gate's
  (Decision 8) — pin both.
- TestValidateDescribed_SameVerdictAsValidateRequest: the shapes of step 3.
- TestEdgeAdmit_FlagOffPassesThrough: {edge:false}, no row, nothing reserved.
- TestEdgeAdmit_OnlyUnsealedWholeWritesQualify: SSE bucket, SSE-C request,
  sealed-only tier, a chunking-eligible size with the flag on, an R2 (public)
  bucket, a backend that is no EdgeWriter → {edge:false}; an ssec_required
  bucket without a key → {edge:false} (the origin's 400 follows).
- TestEdgeAdmit_ReservesQuotaAndMintsAddress: row pending with storage_key an
  address of the key (isBlobAddressOf, blob_address.go:90), reservation held;
  past the quota → the S3 QuotaExceeded XML relayed, nothing pending.
- TestEdgeAdmit_ObjectLockRefused.
- TestEdgeAdmit_RefusalAfterReservationReleases (inject a presign failure).
- TestEdgeCommit_WholeObjectCommitsLikeAPut: head row (backend, floor, attrs,
  content-type), version row when versioned, bill settled, access-log row with
  the client IP and bytes, the response headers; compare against a PUT through
  the origin of the same bytes — same rows but storage_key.
- TestEdgeCommit_ETagMismatchDeletesAndRefuses: no Content-MD5 signed, etag ≠
  md5 → backend Delete observed (fake EdgeWriter), released, 400 BadDigest;
  sha256 claim mismatch → 400 XAmzContentSHA256Mismatch.
- TestEdgeCommit_BackendErrorReleasesAndQueuesOrphan: retired_blobs row
  reason orphan_write on THAT backend, 503 XML with Retry-After.
- TestEdgeCommit_Idempotent: the second commit of one admit answers the same,
  no second row/version; an unknown admit_id 404; an expired one 410.
- TestEdgeCommit_BucketMadePrivateMeanwhile: errSSECRequiredAtCommit path.
- TestEdgeMultipart_FirstPartCreatesBackendUploadOnce: two concurrent first
  parts → one CreateMultipart, both admitted; edge = true.
- TestEdgeMultipart_PartCommitRecordsBackendETag + the reservation on the
  upload row equals UploadPart's for the same size.
- TestEdgeMultipart_CompleteUsesBackendComplete: parts in order, ETag the
  backend's -N, body_md5 NULL, head row flipped, bill settled; a staged part
  on an edge upload → 400 InvalidRequest; Abort and the reaper call
  AbortMultipart.
- TestEdgeAdmitReaper_ExpiredPendingReleasesAndRetires + terminal rows past
  7 days deleted + job_runs result shape.
- TestEdgePresign_SignsLengthAndChecksum (step 4) on the fixed-bucket driver
  and LyveDriver against a fake endpoint; the host-only URL fails the checker.
- TestEdgeAdmits_ErasedWithTheAccount (account.EraseRows walks it).
- Worker: node --test on edge/s3-ingest/test (step 10).

Key files
- internal/api/direct_upload.go:47 DirectUploader, :124 create, :188 complete
  — the shape to generalise (Stat → the commit's integrity check; the
  removeUpload closure → the backend Delete on a refused commit).
- internal/api/placement.go:71 the placement request fields (SSEC,
  SSERequested), :188 bucketEncryptionSettings, :215–232 sealedTier /
  encryptionRequired, :49 placeWholeSealed — the ONE decision the admit calls.
- internal/api/s3.go:1143 handlePutObject (attrs parsing, the scope check, the
  response headers), :605/:622/:624 the dispatch.
- internal/api/s3_multipart.go:93 Initiate, :301 UploadPart, :760
  completeMultipartUpload, :1336 Abort; multipart_reservation.go (the per-part
  reservation under the upload's lock); multipart_reaper.go (gains the backend
  abort); multipart_staged.go (what a staged part looks like — an edge part is
  its sibling without a file).
- internal/api/blob_flip.go:42 blobWrite, :132 commitBlobWrite;
  blob_address.go:56 newBlobAddress, :90 isBlobAddressOf, :133 the retire
  reasons, :358 retireOrphanWrite; object_write_shared.go:143
  upsertWholeObjectHeadRow, :205–222 placeObject's orphan rule.
- internal/api/quota_accounting.go:54 reserveQuota; s3_lock.go:476
  checkObjectLock; access_log.go (the row's columns).
- internal/auth/handlers.go:105 ValidateRequest, :208 LookupCredential.
- internal/clientip/clientip.go:54 IsCloudflare, :69 FromRequest, :92
  trustedPeer (export a predicate; never read CF-Connecting-IP elsewhere).
- internal/api/flags_wiring.go:31 (flag constants + comments), internal/flags/
  service.go:155 Enabled, internal/dashboard/handlers/admin_flags.go:47 (the
  dashboard's flag table).
- internal/drivers/idrive.go:119 NewFixedBucketS3Driver, r2.go:294 PresignPut
  (the SDK presign pattern), lyve.go:26 LyveDriver; internal/drivers/CLAUDE.md
  (the request-bound invariant — a presign is not a request; CreateMultipart /
  Complete / Abort are small S3 calls and take s3OpTimeouts' class).
- internal/api/jobs.go (the one scheduler; add the job like multipart_reaper).
- internal/api/metrics.go (the two new families).
- internal/translog/entries.go:40 KindClientBuild, internal/translog/
  clientbuilds/ (manifest + its closed client list + test),
  scripts/repro-check-clients.sh.
- cmd/vaultaire/main.go (the master-key credential read to copy for the token).
- tools/edge-ec-worker/worker/wrangler.toml, tools/edge-rules-worker/ (the
  wrangler layout; those are benches — yours is product and lives under edge/).
- tests/load (the load gate: add a Worker-path target as an env-gated case,
  VAULTAIRE_LOAD_ENDPOINT = the Worker; not run in CI).
- docs/CLOUDFLARE.md, docs/API.md, docs/DEPLOY.md, internal/api/landing/
  (the sub-processors page source — grep "sub-processors").

Architecture notes
- The control plane never sees a body byte and never holds a key for this
  part: the Worker is a mover. Any "let the Worker encrypt" is slice 2 and
  needs the master key — refuse it here (contract line 4).
- Two-slot deploy: migration 109 is additive, but the PREVIOUS build cannot
  Complete an edge upload (it knows no backend_upload_id) — so the flag stays
  OFF until `vaultaire-switch deploy` has made the new build the active slot;
  only then `edge_ingest` on for tenant zero. Say this in DEPLOY.md.
- The 100 MB limit is the ZONE's (Free plan), answered by Cloudflare before the
  Worker runs: a single PUT over it gets an HTML 413 that no S3 client parses.
  The docs snippet and the sdk's uploader keep parts at 64 MiB. Do not try to
  work around it in the Worker.
- Pass-through is `fetch(request)`: the route's DNS record points at SLC, so the
  origin receives Host s3.stored.cloud and the signature verifies there. That
  is why admit/commit go to VAULTAIRE_ORIGIN (stored.ge) instead: calling
  s3.stored.cloud from the Worker would loop into itself.
- Egress, throttling and reads are Part B. The egress meter is untouched here.
- An admit's reservation + address + 20-minute expiry is the "part that landed
  but was never reported" answer (40.10): the reaper retires the address on the
  backend; the retired_blobs collector's blobNamedBy re-check protects an
  address a late commit DID name (it never commits after expiry: 410).
- The Worker's hash in the log makes the data path verifiable end to end
  (server_build + client_build rows); the deployed script is downloadable, so a
  reviewer can compare. Mention in the trust lane note, not in customer copy
  ("can't read" stays client-side only — contract line 8).
- Live proof, lab: a local Vaultaire from /verify on a private migrated DB
  (make test-db TEST_DB=vaultaire_edge) with STORAGE_MODE=idrive on the lab
  iDrive pair (or LYVE_*), EDGE_SERVICE_TOKEN + EDGE_TRUST_ANY_PEER=true;
  `wrangler dev` with VAULTAIRE_ORIGIN=http://127.0.0.1:8000 and the route's
  origin pointed at it (wrangler dev's --local proxy, or a Cloudflare tunnel —
  your choice, write it down); aws-cli with --endpoint-url the Worker: a 10 MiB
  PUT with Content-MD5, a 10 MiB PUT with aws-chunked CRC32 trailer (aws-cli
  v2.23+ default), a 3-part 64 MiB multipart, an abort, a corrupted-body case
  (a client that lies about Content-MD5 → 400), the flag off (pass-through).
  Prove with: the backend holds the object at the storage_key (list the lab
  prefix), the head row names it, the origin's access log shows bytes_in 0 for
  the admitted writes and the full body for the pass-through ones, the
  reservation settled, `s3api head-object` + a GET through the origin returns
  the bytes (MD5 equal). Report a red-before/green-after table and the proof
  table in WP-EDGE-1A.md. The full suite on a private DB via DATABASE_URL (the
  shared vaultaire_test collides with other sessions).
- Live proof, prod (after the owner's [YOU] 1–3 and the deploy): tenant zero,
  one Lightroom backup zip through s3.stored.cloud with rclone
  (--s3-upload-cutoff 64M --s3-chunk-size 64M), then
  vaultaire_data_plane_bytes_total vs HAProxy's bytes for the same minute.
- Operator items — YOURS, not the owner's (the owner's standing authorization of
  2026-10-26: "build everything now, I won't be back for a while"; plan 40.10
  "Build everything now"). Do them AFTER the lab proof and after A1 + A2 are
  on main and `vaultaire-switch deploy` has made the new build the active slot
  (DEPLOY.md). In order; every secret handled so that its value never appears
  in a command line, a log or the transcript:
  (1) DNS: an A (and AAAA if the box has one — read HAProxy's bind) record
      `s3.stored.cloud` → the SLC address, proxied, on the stored.cloud zone,
      through the Cloudflare API with CLOUDFLARE_ZONE_TOKEN (source the creds
      file with `set -a; . ~/fairforge/.cloudflare-creds.env; set +a` inside
      one `bash -c`; never cat it). Then the certificate: on the box, `sudo
      certbot certonly --expand` adding the SAN s3.stored.cloud to the cert
      that holds stored.cloud (HTTP-01 — the ACME path reaches acme_backend
      through HAProxy, DEPLOY.md:62; the deploy hook rebuilds
      /etc/haproxy/certs/*-le.pem, DEPLOY.md:83); verify with `openssl
      s_client -servername s3.stored.cloud`. If the zone's SSL mode is Full
      (not strict) the SAN is still added — the probe TLS_CERT_PROBE_TARGETS
      gains `s3.stored.cloud@127.0.0.1:443`.
  (2) The token: on the box `umask 077; openssl rand -hex 32 > /tmp/edge.tok`
      (never echo it); append `EDGE_SERVICE_TOKEN=$(cat /tmp/edge.tok)` to
      /opt/vaultaire/configs/.env with `sudo sh -c` after the usual backup
      `.env.bak-edge-token-<ts>` (the zsh pipe+heredoc rule: one `bash -c`, no
      heredoc into ssh); `sudo vaultaire-switch restart`; the boot line says
      `edge service token: env`. Copy the file to the Mac (scp, 0600), add it
      to the Keychain with `security add-generic-password -a vaultaire -s
      vaultaire-edge-token -w "$(cat …)"` inside a bash -c (the value is read
      from the file, never typed), then `wrangler secret put
      VAULTAIRE_EDGE_TOKEN < file` on the PRODUCTION account
      (CLOUDFLARE_API_TOKEN = the user token, CLOUDFLARE_ACCOUNT_ID =
      CLOUDFLARE_PROD_ACCOUNT_ID); shred both files.
  (3) `wrangler deploy` from edge/s3-ingest/ (the route `s3.stored.cloud/*`
      in wrangler.toml); confirm `GET https://s3.stored.cloud/health` answers
      the origin's body (pass-through) and the translog manifest row's sha256
      equals the deployed script's (download it with wrangler).
  (4) Flag `edge_ingest` ON for tenant zero through the admin API (a psql row
      skips the guard, RP62-13) — the admin JWT comes from the sign-in with the
      bench key route of docs/reviews (never minted from JWT_SECRET); then the
      Lightroom proof above; then the sub-processors sentence is in A3.
  If any token lacks a scope (a 403 from the API), stop there, say exactly which
  scope, and leave that item as the ONE [YOU] — the owner can mint a token from
  a phone. Nothing else in this prompt is the owner's.
- Closeout: write **Prompt 45 (WP-EDGE-1 Part B — reads)** into this file in
  the /next format (STATUS "Now" + plan 40.10 "Build everything now" are the
  order of record; your WP-EDGE-1A.md residuals are its Part 0), update STATUS,
  the plan's 40.10 status line and the Stage 1a ledger memory row 44, and name
  the post-merge review RP64 — the chain continues without the owner.
- Commit: feat(api,drivers,edge): WP-EDGE-1 Part A — the mover … [WP-EDGE-1];
  no AI attribution lines in commits; merge with `gh pr merge --auto` (merge
  queue; no --squash, no --delete-branch, never --admin).
```

---

## Stage 1a Prompt 45: WP-EDGE-1 Part B — reads (GetObject through the Worker on `s3.stored.cloud`)

*Ran 2026-10-26 (#984 Part 0, #985 the control plane + migration 110, #986 the Worker 0.2.0, the docs PR; note `docs/reviews/WP-EDGE-1B.md`). The prompt actually issued superseded the text below in four places, each with its reason in the note: the throttle decision is a PASS-THROUGH to the origin's pacing, never a 503 from admit (D-EDGE-13; the plan's two 40.10 sentences rewritten); egress is metered at the commit (at the backend's headers), nothing at admit (D-EDGE-14); an `If-Match` the row satisfies is admitted — aws-cli's download shape (D-EDGE-12); and the link's If-Match is signed only where the row's digest is provably the backend's ETag, with a Range always (D-EDGE-10/11 — the lab's 412s).*

```
Stage 1a Prompt 45: WP-EDGE-1 Part B — reads: GetObject leaves SLC

Continue from WP-EDGE-1 Part A (Stage 1a Prompt 44, 2026-10-26: #980 the
Worker, #981 the control plane + migration 109, #982 the docs; deployed;
the Worker live on s3.stored.cloud; flag `edge_ingest` ON for tenant zero;
note docs/reviews/WP-EDGE-1A.md — read its § Decisions D-EDGE-1…9 and
§ Residuals RE-1…: they are this prompt's Part 0) and from the baseline
bench-results/EDGE-RELAY-2026-10-26.md (reads through a Worker ran at the
backend's single-stream rate, 111 MB/s from SLC, with a better first byte
than direct: a presigned GET streamed back — `return fetch(url)` — is the
whole data path; no JavaScript touches the bytes, as on the write path).
Most recent merged PR: `gh pr list --state merged --limit 5`. Open PRs
that matter: re-check `gh pr list --state open`; the dependabot bumps do
not collide. Next migration: 110 (109 was Part A's) — claim it only if
Part B needs a column; it should not (the admit row is Part A's table with
op = 'get').

Read first:
- docs/IMPLEMENTATION_PLAN.md § 40.10 "Slice 1 — the mover" (the GET
  sentence), "What the move changes elsewhere" (egress is metered from
  admit/commit; the egress_throttle decision becomes a 503 SlowDown from
  admit — the Worker never paces), "The invariant and the order" (reads
  leave SLC too).
- docs/ARCHITECTURE_CONTRACT.md lines 1, 4, 7, 11 (cite them in the PR
  body); line 8 (the Worker is not a privacy feature).
- internal/api/edge_ingest.go (the admit/commit shape, the relay, the
  token auth, the metrics families), edge_multipart.go, edge_reaper.go;
  edge/s3-ingest/src/index.js (classify/describe/relay; moveBody);
  internal/api/egress_meter.go + egress_writer.go (the ONE egress counter,
  the allowance, the stream guard, the `egress_throttle` flag, decision
  D-25); internal/api/s3_engine_adapter.go HandleGet + serveGetObject (what
  a GET does: the head row, objectBlobAddress, WithRecordedBackend, the
  conditionals, Range, the integrity reader, the headers — object_headers.go
  — the access log, the egress span, `object.downloaded`); s3_large_get.go
  (parallel_get: a LATER part); internal/drivers/idrive.go GetRangeInfo /
  lyve.go (VersionedRangeGetter — the ranged read the presign must match).
- CLAUDE.md Architecture Decisions 2 (HEAD stays at the origin, cache-only
  — never through the Worker), 7 (client IP), 8 (the edge calls carry the
  client's Authorization inside the JSON).

Lane: Engine. Two PRs: (B1) the control plane — the GET admit/commit
(`op = 'get'` on edge_admits), the presigning drivers' PresignGet, the
egress metering from admit/commit, the throttle decision as 503 from admit;
(B2) the Worker — GET classification, the presigned GET streamed back, the
commit of bytes served (the Worker learns the byte count from the backend's
Content-Length / the Range's length, never from reading the body), docs.
One docs PR after the live proof (plan 40.10 status, STATUS, CLAUDE.md
rows, API.md, WP-EDGE-1B.md with Step 0 answers, decisions, residuals).

Scope: GetObject (plain and Range) of a tenant with the `edge_ingest` flag,
whose head row is a WHOLE UNSEALED object on an EdgeWriter backend (idrive,
idrive-<region>, wasabi, lyve), of at least EDGE_MIN_BYTES, is admitted —
SigV4 verified on SLC (ValidateDescribed), the key's scope (GetObject, the
bucket), the head row read (size, ETag, backend, storage_key, content type,
the attributes, the version), the conditionals (If-Match / If-None-Match /
If-Modified-Since → the origin's 304 / 412 relayed from admit), the Range
parsed as range.go does (a multi-range is the origin's; a 416 relayed), the
egress allowance checked: a tenant past its allowance with `egress_throttle`
ON answers 503 SlowDown + Retry-After FROM ADMIT (never paced by the
Worker; the stream guard's MAX_STREAMS rule applies to the origin's own
paced responses only — document); the object's bytes are metered on the
tenant's month counter AT ADMIT (`egressMeter`: the live counter + the
bandwidth_usage_daily row as the origin's GET does — the commit corrects
to what the backend served when it differs: a cut body); a presigned
ranged GET (TTL 15 min, `Range` passed through, `If-Match: <backend
ETag>` signed so a concurrent overwrite cuts the body — never splices;
the backend ETag is the row's ETag for a single PUT, the `-N` for a
multipart assembled by the backend) is handed to the Worker with the
response headers the origin would send (object_headers.go's set: ETag,
Last-Modified, Content-Type, Accept-Ranges, x-amz-storage-class via
engine.CustomerStorageClass, x-amz-version-id, x-amz-meta-*, the
disposition/encoding/language/cache headers, Content-Range + 206 for a
range). The Worker streams the backend's body back with THOSE headers (not
the backend's), then commits {admit_id, outcome, backend_status,
backend_content_length, duration_ms}; the commit writes the access-log row
(GetObject, the client's IP, bytes_sent), fires `object.downloaded` and
counts `vaultaire_data_plane_bytes_total{host="edge",op="get"}`. Pass
through to the origin: HEAD (Decision 2), a chunked / sealed (SSE-S3, SSE-C)
/ tape / `sia` / parity-rebuilt row, a `?versionId` of a non-current
version (its own row — a later part), a public bucket's object (R2 /cdn),
an object under EDGE_MIN_BYTES, a presigned GET URL (the origin verifies
presigned queries), a request with a flag-off tenant. HEAD stays at the
origin (cache-only, non-negotiable 2). The `parallel_get` whole-object
path at the edge is a later part (one presigned ranged GET per part would
be the shape).

Step 0 — probes (write the answers into WP-EDGE-1B.md):
0.1 PresignGetObject with Range and IfMatch in the input: are `range` and
    `if-match` in X-Amz-SignedHeaders (the Worker must send exactly the
    signed headers)? If the SDK does not sign them, build the presign as
    the write path does and check; both backends must honour a signed
    If-Match (412 on a mismatch — probe on the lab pairs with a 10 MiB
    object overwritten between presign and GET).
0.2 A Worker `return fetch(presignedURL, {headers: send_headers})` with the
    response's headers REPLACED by the origin's set (a new Response around
    the backend's body): does it stream (no buffering), and does
    `Content-Length` survive a `new Response(resp.body, {headers})`
    (wrangler dev, a 64 MiB object)? Does a client Range through the Worker
    get the backend's 206 bytes exactly?
0.3 The egress meter: how a GET through the origin lands on
    `bandwidth_usage_daily` and the live counter today (`egressSpan`,
    `recordResponse`) — what the admit must write so the month counter,
    the alerts ladder and the throttle see the same number; and what
    `EgressStatus` reads.
0.4 The lab topology of Part A (D-EDGE-7: wrangler dev --local-upstream +
    the HTTP-proxy trick) holds for GET; verify a GET through it returns
    the backend's bytes once.

Tests (red first; the Part A fixture `setupEdgeFixture` + `edgeFakeDriver`
gain PresignGet and a served-body record): TestEdgeAdmitGet_ChunkedSealed
TapePublicPassThrough, TestEdgeAdmitGet_MetersEgressAtAdmit (the egress
rows equal a GET through the origin), TestEdgeAdmitGet_ThrottledTenantIs
503FromAdmit (flag on; a tenant row with the flag off is the exemption),
TestEdgeAdmitGet_RangeAndConditionals (206 headers, 416, 304, 412 relayed
from admit), TestEdgeAdmitGet_HeadersAreTheOrigins (the header set equals
HEAD's from object_headers.go), TestEdgeCommitGet_AccessLogAndEvent,
TestEdgeCommitGet_LostCommitStillMeters (the reaper reads the admit's size:
metered at admit, nothing to undo), TestEdgePresignGet_SignsRangeAndIfMatch
on both drivers against a fake endpoint; Worker: classify (GET with/without
Range, HEAD never, presigned query never), the header replacement, the relay.

Live proof, lab: the Lightroom zip of Part A's prod proof shape in the lab
(a 192 MiB object): a GET through the Worker MD5-equal, a Range GET's
bytes, a GET of a throttled tenant 503 from admit, the origin's access log
bytes_out 0 for the admitted reads; bandwidth_usage_daily equal to a GET
through the origin. Prod: the Lightroom zip of Part A back through
s3.stored.cloud with rclone, MD5 equal, `vaultaire_data_plane_bytes_total
{op="get"}` vs the origin's egress for the same minute (the origin's
request log carries no bytes: use the metric and HAProxy's log).

Operator items (yours): none new — the Worker redeploys (`wrangler deploy`
+ the manifest row for the new file), the flag is already ON for tenant
zero. The Worker's new hash is a new client_build row (version 0.2.0).

Closeout: write Prompt 46 (WP-EDGE-2, slice 2 against the lab master key —
the chain list below) into this file in the /next format; update STATUS,
the plan's 40.10 status line and the Stage 1a ledger memory row 45; name
the post-merge review RP66.
Commit: feat(api,drivers,edge): WP-EDGE-1 Part B — reads … [WP-EDGE-1];
no AI attribution lines; merge with `gh pr merge --auto` (merge queue; no
--squash, no --delete-branch, never --admin).
```

---

## Stage 1a Prompt 46: WP-EDGE-2 Part A — slice 2 against the LAB master key: the codec and the control plane (Part 0: RP66-1/2/3/5/6/7)

```
Stage 1a Prompt 46: WP-EDGE-2 Part A — slice 2 against the LAB master key: the
codec and the control plane (Part 0: RP66-1/2/3/5/6/7)

Continue from WP-EDGE-1 Part B (2026-10-26: #984 Part 0, #985 the control plane +
migration 110, #986 the Worker 0.2.0, #992 docs; deployed; reads leave SLC for
tenant zero; note docs/reviews/WP-EDGE-1B.md — read § Decisions D-EDGE-10…15 and
§ Residuals RB-1…9) and from its post-merge review RP66
(docs/reviews/WP-EDGE-1B-REVIEW.md — HOLDS at P2; RP66-1 and RP66-2 must land
before `edge_ingest` widens, and they are THIS prompt's Part 0 with RP66-3/5/6/7).
Most recent merged PR and open PRs: `git log origin/main`, `gh pr list`.

THIS IS PART A OF TWO. The Prompt 46 text written on 2026-10-26 asked one session
for the codec, the control plane, the Worker 0.3.0, the lab proof and the F-3 tool
— more than WP-EDGE-1 Part A (4,400 lines). Part A here is the codec in Go with
its WASM build pinned by golden vectors, and the control plane's index/commit half
proven by a Go harness that plays the Worker; Part B (Prompt 46b, written by THIS
closeout) is the Worker 0.3.0 pipeline, the lab proof through wrangler dev and
cmd/tools/dedup-measure. Two corrections of that text, found by RP66 (RP66-8):
(1) a per-admit keyed hash cannot be mapped to the tenant's keyed hash — the Worker
sends sha256(chunk) over the service-token channel and the CONTROL PLANE applies
the tenant index key, in memory for the call, never stored or logged; (2) the
dual-key stream's format byte is NOT 0x04 (the CURRENT StreamFormat, every writer
since WP-R8-3 A.2; 0x03 is opened only) nor 0x05 (the SSE-C multipart header) —
it is 0x06; verify in internal/crypto/stream.go:93–101 before you type it.

Read first: docs/ARCHITECTURE_CONTRACT.md lines 4, 5, 6 (cite in the PR body:
keys only through the one hierarchy; no global convergent key; dedup by a keyed
index); docs/IMPLEMENTATION_PLAN.md § 40.10 "The invariant and the order" and
"Build everything now" (slice 2 is proven on a LAB master key, deployed DARK,
refused on prod without a master key — the same refusal placement.go gives
today); docs/surfaces/ENGINE.md; docs/ENCRYPTION.md (tenant key versions, the
ML-KEM-768 seed, the 0x04 streaming format — internal/crypto/stream.go, its
golden vectors in stream_test.go); docs/reviews/WP-R8-4.md (the frozen chunker:
crypto.ChunkerIdentity / DefaultChunkerIdentity, internal/crypto/chunker.go:47);
sdk/go/e2ee (e2ee.go: the HPKE wrap with X-Wing the sealer reuses; the WASM
build in sdk/wasm and scripts/build-client.sh); internal/api/chunk_store.go,
chunk_claim.go (claim → PUT → ready, the 3 min takeover window, the one address
`_chunks/<tenant>/<hash>.k<N>` for an encrypted-scope chunk), chunked_write.go
(storeChunkedObject: the rows a chunked write produces — the commit must produce
the SAME rows), placement.go (the one decision; `chunked`; the `chunking` flag);
CLAUDE.md Architecture Decisions 2, 7, 8; docs/reviews/WP-EDGE-1A.md § Decisions
(D-EDGE-3 one upload one path, D-EDGE-9 the JSON body); bench-results/
EDGE-RELAY-2026-10-26.md (JavaScript in the byte path halves a stream — slice 2
puts WASM in it on purpose; Part B measures it).

Lane: Engine. Next migration: 111 — `global_content_index.keyed_hash` (BYTEA/TEXT,
NULL on every row written before; a partial unique index with the scope) and
`edge_admits.codec BOOLEAN NOT NULL DEFAULT FALSE` (or `attrs.codec`; decide in
Step 0.4 and say so); additive. Four PRs: (0) the RP66 fixes — Worker 0.2.1 +
the control plane; (A) the codec: the 0x06 format in Go, the keyed hash, the WASM
chunker build + golden vectors; (B) the control plane: the admit's codec answer,
POST /api/v1/edge/index, the codec commit, the reaper, flag `edge_codec`, the
metrics, migration 111; then (C) the docs PR after the harness proof
(WP-EDGE-2A.md with Step 0 answers / decisions / residuals, STATUS, plan 40.10,
CLAUDE.md rows, ENCRYPTION.md, the Prompt 46b text into this file).

Scope: a PutObject / UploadPart admitted at the edge (Part A's gate) of a tenant
with the `edge_codec` flag (default OFF, per tenant; needs a tenant key version —
on prod there is none without a master key, so the admit answers Part A's whole
PUT, reason `no_key`, never a refusal and never a plaintext chunked write) is
chunked, hashed, sealed under a random per-chunk key wrapped to the tenant's
ML-KEM-768 public key, and uploaded chunk by chunk BY THE DATA-PLANE HOST; the
control plane answers the index (hit / miss per hash), signs each miss's link
into `_global`, and writes the manifest at commit exactly as storeChunkedObject
does. In Part A the data-plane host is a Go test harness; Part B makes it the
Worker. Reads of such an object are the origin's (Part B of WP-EDGE-1 passes a
chunked row through); the edge-side open with a released key is Prompt 47's or
later — say so.

What to build
1. PART 0 (own PR, first — each fix exactly as RP66's fix column):
   a. RP66-1 (P2): internal/api/edge_get.go edgeAdmitGet — a row with a non-empty
      `content_encoding` passes through (`passThrough("content_encoding")`; the
      Workers runtime re-encodes a body whose Response carries Content-Encoding,
      and the fixed-bucket driver never sent the encoding to the backend); a row
      in TestEdgeAdmitGet_PassThroughMatrix (red first).
   b. RP66-2 (P3) + RP66-7: the admit answers `content_length` (the admitted
      span); edge/s3-ingest/src/index.js readSucceeded = status === expect_status
      AND (no content-length header OR Number(content-length) === admit.content_
      length) — a mismatch is a `fallback` (the origin's integrity path decides;
      the backend body cancelled); settleEdgeGet counts every served read in
      vaultaire_object_integrity_unverified_total{path="edge",reason="unverified"}
      (object_integrity.go's counter — pre-create the pair) and a missing
      Content-Length is Debug, metered as admitted. Node test: the length
      mismatch → fallback. Worker 0.2.1: a new manifest row (sha256 of the
      deployed file), `wrangler deploy` on the production account, the deployed
      script downloaded back byte-identical.
   c. RP66-3 (P3): settleEdgeGet (served) runs the origin's detached
      `UPDATE object_head_cache SET last_accessed = NOW()`
      (s3_engine_adapter.go:515–520, context.Background()).
   d. RP66-5 (P4): the response's x-amz-request-id = req.RequestID (the Worker's;
      what the admit row and the access-log row carry), as commitEdgePart does.
   e. RP66-6 (P4): the reaper purges terminal `get` rows after 24 h; writes keep
      7 d (two DELETEs, or one with a CASE).
   f. Step 0.5 at the 0.2.1 deploy: RB-9's capture — a signed pass-through GET
      (If-None-Match with Range) through s3.stored.cloud every 5 s for two
      minutes after `wrangler deploy` returns, the origin's `auth` Debug on, the
      first failing request's headers kept; the answer (or "not reproduced, N
      requests") into WP-EDGE-2A.md and the RB-9 line of WP-EDGE-1B.md.
2. THE CODEC (Go first, one implementation):
   a. The chunker is frozen (WP-R8-4, crypto.ChunkerIdentity). Add its WASM
      build: the SAME package compiled with GOOS=js GOARCH=wasm into
      edge/s3-ingest/wasm/chunker.wasm by scripts/build-client.sh (a new client
      `edge-chunker` with its row in internal/translog/clientbuilds/manifest.json;
      scripts/repro-check-clients.sh rebuilds it; `make lint-wasm` covers it).
      A golden test in Go (TestChunker_GoldenBoundaries) and the SAME vectors in
      a node test running the wasm (edge/s3-ingest/test/chunker.test.js, under
      `make test-edge`): a 100 MiB pseudo-random body from a FIXED seed (never a
      file in the repo) → the exact boundary offsets, byte-identical between Go
      and WASM. Boundaries reset at part edges (each 64 MiB part is chunked
      alone; the manifest's chunk list is the parts' concatenation — cross-part
      dedup is by chunk content, not position; say so in the note).
   b. The 0x06 format (internal/crypto/stream.go): the 0x04 layout + a wrap kind
      byte + the KEM ciphertext (X-Wing, as sdk/go/e2ee's HPKE) + the tenant key
      VERSION. A random chunk key per chunk; the chunk sealed in the 0x04 layout's
      1 MiB segments under it; the chunk key wrapped with HPKE to the tenant's
      ML-KEM-768 public key of that version. Open: unwrap under the tenant private
      seed of the recorded version (tenant_key_versions, WP-R8-1), then the 0x04
      open. 0x03/0x04/0x05 blobs still open (IsStreamFormat grows by 0x06; every
      reader that switches on the format byte is found by grep and listed in the
      note). The golden vectors gain the 0x06 case with a FIXED test seed and a
      fixed KEM ephemeral (the HPKE test-vector trick) so the ciphertext is
      reproducible.
   c. The keyed hash: HMAC-SHA256(tenant_index_key, sha256(chunk plaintext)) where
      tenant_index_key = HKDF(the key version's chunk key, "stored/index/v1") —
      ONE hierarchy (contract line 4); the index stores the keyed hash ONLY
      (TestIndexHoldsNoPlainHash: no column, log line or metric of the new path
      carries sha256(chunk) — the test greps the schema, the zap fields and the
      metric labels); dedup is per tenant by construction (contract line 5): the
      cross-tenant forms are 10.3's filter and F-37's pool, neither here.
   d. D-EDGE-16 (rewritten): the data-plane host learns NO long-lived key. It
      sends sha256(chunk) in the index call (the service-token channel, TLS to
      the control plane); the control plane applies the tenant index key, in
      memory for the call. The host already holds the plaintext; the hash adds
      nothing to what it knows. Argue it in the note against the stored text's
      per-admit key (impossible: HMAC(K_tenant, h) is not computable from
      HMAC(K_admit, h) without h).
3. THE CONTROL PLANE:
   a. admit (op put/part, Part A's gate passed): with `edge_codec` ON for the
      tenant AND a tenant key version (crypto.TenantKeys) AND a `chunked`-eligible
      placement (placement.go's rule for storeChunkedObject: the chunking flag,
      above the chunk threshold, unversioned, unpinned, a plain bucket; a body
      under the threshold stays Part A's whole PUT), the admit answers
      {edge:true, codec:{version, kem_public_key (base64), chunk_threshold,
      segment_size}, admit_id, ...} and NO presigned PUT for the body; the row
      records `codec = true`. Without a key version: {edge:true} as Part A, the
      Debug line says `no_key`.
   b. POST /api/v1/edge/index (the token gate; D-EDGE-9's JSON shape):
      {admit_id, chunks:[{hash (hex sha256), size}]} → {hits:[hash…],
      misses:[{hash, url, send_headers, sealed_length}]}: per hash the control
      plane computes the keyed hash and looks the tenant's encrypted scope up
      (ready rows only); each miss is a presigned PUT into `_global` at the ONE
      address (`engine.ChunkContext`, chunk_store.go's rule; the key
      `_chunks/<tenant>/<keyed hash hex>.k<version>`) with a CLAIM taken as
      chunk_claim.go takes it (pending, pending_since = the admit id; the takeover
      window applies — a host that dies leaves a claim the next writer takes over
      after 3 min), the link signing content-length = the sealed length the host
      can compute from the plaintext size (D-EDGE-17: no Content-MD5 on a chunk
      link — the host would have to seal every chunk before asking; the backend's
      ETag of the sealed bytes is recorded at commit as the row's ciphertext_hash
      and every chunked GET verifies the stored blob against it, as today). A hit
      is referenced at COMMIT (IncrementRef), never at index time — a host that
      dies holds no reference. ≤ 10,000 hashes per call; one call per part.
   c. commit: {admit_id, chunks:[{hash, size, sealed_size, backend_etag,
      status}], body_md5, …}: every miss's backend answer ok → readyChunk (state
      ready, the backend, ciphertext_hash = the backend's MD5 ETag of the sealed
      bytes, encryption_key_version = the admit's, +1 ref), every hit →
      IncrementRef (0 rows = re-stored by the host? NO: a vanished hit is the
      commit's error — the host retries the part; say so); then the manifest
      (ReplaceObjectManifestTx) + the chunked head row (backend `chunked`,
      is_chunked, the encryption name of the 0x06 chunks — a new constant beside
      AES256-CE, the ETag = the host's whole-body MD5 for a PUT: the host reads
      every byte to chunk it, so the digest is free — this REVERSES Part A's
      "no JS in the byte path" for codec writes; say so) in ONE transaction
      through atomicHeadUpsert's rules for a chunked row; a failed commit drops
      the claims (dropChunkClaim) and the references taken. A multipart: each
      part's commit stores its chunk list on the part row (`multipart_parts.
      part_chunks` JSONB — Step 0.4) and takes its references; Complete
      concatenates the parts' lists into one manifest (the -N ETag as today) —
      the origin's completeMultipartUpload gains the codec branch beside the edge
      branch (one upload one path, D-EDGE-3: a codec part on an upload with a
      staged or an edge-whole part is 400 InvalidRequest).
   d. The reaper: an expired codec admit drops its pending claims; blobs the host
      reported landed and never committed are chunk_orphans' (the 24 h grace).
   e. Flag `edge_codec` (per tenant first; registerFlags). Metrics
      vaultaire_edge_codec_chunks_total{outcome=hit|miss|stored|failed},
      _bytes_total{kind=plaintext|sealed}, _index_calls_total — pre-created.
4. THE HARNESS (Part A's proof of 3): internal/api/edge_codec_harness_test.go —
   a Go "Worker": chunks a body with the Go chunker, hashes, calls /index over
   httptest, seals the misses with the Go 0x06 sealer under the admit's public
   key, PUTs them to the presigned links (edgeFakeDriver records the sealed
   bytes; one run against the bench iDrive pair under a build tag), commits;
   then the ORIGIN reads the object back (the lab master key file) MD5-equal.
   This harness is also Part B's oracle: the Worker must produce the same rows.
5. NOT HERE (Prompt 46b): the Worker 0.3.0 pipeline (TransformStream → the WASM
   chunker → sha256 → the index call per 64 chunks → WebCrypto AES-GCM per 1 MiB
   segment + the HPKE wrap from sdk/wasm → PUT per miss → commit; back-pressure;
   the per-stream rate), the lab proof through wrangler dev, cmd/tools/
   dedup-measure (F-3).

Step 0 — probes, answers into WP-EDGE-2A.md: 0.1 the WASM chunker's boundaries on
the 100 MiB seed body equal Go's (byte-identical) and its throughput in node
(workerd is Part B's); 0.2 the X-Wing KEM from sdk/wasm callable standalone in
node (bundle size, ms per wrap) — enough to know Part B is buildable; 0.3 the
claim/takeover window against a host that dies mid-part (chunk_claim.go's rule
holds: the next writer takes over after the window; the harness kills itself);
0.4 migration 111's shape (keyed_hash column + index; edge_admits.codec;
multipart_parts.part_chunks) — decide and say why; 0.5 RB-9's capture at the
0.2.1 deploy (Part 0 f).

Tests (red first): the golden boundaries (Go + node), the 0x06 vectors + every
older format still opens, the keyed hash (a fixed key version → a fixed value;
TestIndexHoldsNoPlainHash), the index call (hit / miss / claim / takeover / a
second host racing the same miss), the commit (manifest + refs + head row =
storeChunkedObject's rows for the same body, compared row by row), a failed
commit drops the claims, the reaper, the flag OFF / no key version → Part A's
whole PUT, the multipart concatenation + the one-path refusal, the harness end
to end (a second upload of the same bytes = 100 % hits and 0 PUTs; the origin
reads it back). Part 0: the matrix row, the length-mismatch fallback (node), the
last_accessed touch, the request id, the 24 h purge. Structural:
TestSQLLiteralsMatchSchema, TestEdgeRoutes_NeverCapturedByTheS3Gate (the index
route too), migration 111 in the walk.

Key files
- internal/api/edge_get.go:73 edgeAdmitGet (:91 the conditional list — RP66-1
  beside it), :283 the request id, :337 settleEdgeGet
- internal/api/edge_ingest.go:284 edgeAdmit, :304 edgePrelude, :396 edgeAdmitWrite
  (the codec branch goes after the placement decision), :791 edgeCommit, :921
  settleEdgeAdmit; edge_reaper.go:82 (the purge), :116 (the op switch)
- internal/api/chunk_claim.go, chunk_store.go, chunked_write.go, placement.go,
  s3_multipart.go (completeMultipartUpload's branches), edge_multipart.go
- internal/crypto/stream.go:93–101 (the format bytes), :307 newStreamHeaderFormat;
  chunker.go:47; stream_test.go (the golden vectors)
- sdk/go/e2ee/e2ee.go (the HPKE wrap), sdk/wasm, scripts/build-client.sh,
  scripts/repro-check-clients.sh, internal/translog/clientbuilds/manifest.json
- edge/s3-ingest/src/index.js:137 readSucceeded, :170 readCommit; test/read.test.js

Architecture notes
- Contract lines 4, 5, 6 cited in every PR body. The index stores no plain hash;
  the host holds no long-lived key (D-EDGE-16 rewritten); the control plane sees
  plaintext HASHES for the duration of a call and never a plaintext byte.
- On prod: deployed DARK. `edge_codec` ON for a tenant without a key version does
  nothing (Part A's whole PUT); with the F-14 ceremony the owner's session flips
  it and runs the sealed proofs. The lab proves everything with
  ENCRYPTION_MASTER_KEY_FILE.
- Two-slot deploy: migration 111 additive; a codec admit comes only from the new
  build, so no old-build Worker interaction exists; deploy the origin first.
- Operator items: the 0.2.1 Worker deploy (Part 0) with RB-9's capture; the
  `edge-chunker` manifest row; nothing else — the flag stays OFF.
- Closeout: write Prompt 46b (WP-EDGE-2 Part B: the Worker 0.3.0, the lab proof,
  dedup-measure; its Part 0 = RP68's findings) into this file per the chain;
  STATUS, plan 40.10, the Stage 1a ledger row 46; the post-merge review is RP68.
- Commit: feat(crypto,api,edge): WP-EDGE-2 Part A — … [WP-EDGE-2]; merge with
  `gh pr merge --auto` (merge queue; no --squash/--delete-branch, never --admin).

ultrathink
```

## Stage 1a Prompt 46b: WP-EDGE-2 Part B — the Worker 0.3.0 pipeline, the lab proof, dedup-measure (Part 0: RP68's findings)

Continue from WP-EDGE-2 Part A (2026-10-27: #994 Part 0 + the Worker 0.2.1,
#995 the codec, #997 the control plane + migration 111; note
docs/reviews/WP-EDGE-2A.md — read § Decisions D-EDGE-16…21 and § Residuals
RB2-1…5 + RB-9) and from its post-merge review RP68
(docs/reviews/WP-EDGE-2A-REVIEW.md once written — its findings are THIS
prompt's Part 0; if RP68 has not run yet, run it first as its own session).
Most recent merged PR and open PRs: `git log origin/main`, `gh pr list`.

Read first: docs/reviews/WP-EDGE-2A.md (the harness
internal/api/edge_codec_harness_test.go is THE ORACLE: the Worker must
produce the same rows — the harness's admit → index → seal → PUT → commit
sequence is exactly the Worker's pipeline, and its tests say what each
answer looks like); internal/crypto/edge_codec.go (SealWrappedChunk is the
Go reference of what the Worker must produce byte-compatibly: the 0x06
header, HKDF(chunk key, "stored/edge-codec/stream-v1" ‖ header) → AES-256-GCM
1 MiB segments with the 0x04 nonce/last-flag layout, the chunk key wrapped
with sdk/wasm's sealTo under info "stored/edge-codec/chunk-key/v1");
sdk/chunkerwasm (the chunker's WASM API: create/push/finish/free);
edge/s3-ingest/src/index.js and README; bench-results/EDGE-RELAY-2026-10-26.md
(JS in the byte path halves a stream — slice 2 puts WASM in it ON PURPOSE;
this prompt measures what it costs); contract lines 4, 5, 6 (cite in the PR
body).

Lane: Engine. No migration expected (next: 112 — claim it only if RP68
forces one). PRs: (0) RP68's fixes; (A) the Worker 0.3.0 + its node tests;
(B) cmd/tools/dedup-measure; (C) the docs PR (WP-EDGE-2B.md, STATUS, plan
40.10, CLAUDE.md rows, Prompt 47's text refreshed in this file).

What to build
1. PART 0: RP68's findings, each exactly as its fix column says.
2. THE WORKER 0.3.0 (edge/s3-ingest): a codec admit (admit.codec set) runs
   the pipeline instead of moveBody — the request body through a
   TransformStream into the WASM chunker (chunker.wasm + wasm_exec.js
   shipped as additional modules beside index.js; `no_bundle` keeps them
   separate files — probe in Step 0 how a multi-module Worker's
   client_build row is computed: the manifest row should cover index.js AND
   chunker.wasm, likely as two rows or a combined digest — decide and say
   so), sha256 per chunk (WebCrypto digest), the index call per 64 chunks
   (one /index call per batch; the LAST batch may be smaller), per miss:
   seal (WebCrypto AES-GCM per 1 MiB segment under a random key from
   crypto.getRandomValues; HKDF via WebCrypto deriveBits with the info =
   "stored/edge-codec/stream-v1" ‖ header; the nonce layout stream.go's;
   the wrap via sdk/wasm's sealTo — load e2ee.wasm too, or extract the
   HPKE seal into a smaller module — measure both, Step 0) and PUT to the
   link with Content-Length = sealed_length; back-pressure: at most N
   chunks buffered (the Workers memory limit is 128 MB — N ≤ 4 of 16 MiB
   worst-case, measure), the body read paced by the PUTs; then the commit
   with the ordered chunk list + body_md5 (MD5 in WASM or JS — the ETag;
   measure its cost). A pipeline failure ANYWHERE falls back: the admit is
   failed via commit (outcome edge_error) and the ORIGIN takes the request
   — but the body is partially consumed: a codec admit CANNOT fall back to
   fetch(request) once a byte is read; answer the client 503 SlowDown +
   Retry-After (the client retries; the commit releases everything) — say
   so in the README and the note.
3. THE LAB PROOF through wrangler dev (the D-EDGE-7 topology: aws-cli →
   wrangler dev as HTTP proxy → the lab origin with
   ENCRYPTION_MASTER_KEY_FILE): a 100 MiB `aws s3 cp` admitted codec,
   chunked by the WASM chunker at the GOLDEN boundaries, sealed, stored,
   committed; the origin reads it back MD5-equal; a re-upload = 100 % hits
   and 0 chunk PUTs; the rows equal the HARNESS's for the same body (the
   oracle — compare manifests row by row in SQL); a multipart upload with
   ≥ 64 MiB parts; a mid-body cut (kill aws-cli) → the claims dropped by
   the commit/reaper; the per-stream RATE measured against Part A's
   pass-through baseline (the whole point of the bench caveat) and the
   isolate's memory watched (`wrangler dev` reports it).
4. CMD/TOOLS/DEDUP-MEASURE (F-3): the number the owner decides F-37 with.
   A read-only tool against a lab corpus (a directory tree, or a bucket via
   the S3 API with a provided key): chunk every file with the frozen
   chunker, hash, and report the dedup fraction WITHIN one tenant and
   ACROSS N simulated tenants (the corpus split by top-level directory =
   one "tenant" each): total bytes, unique bytes, the cross-tenant-only
   fraction (what a global index would save beyond per-tenant dedup — the
   number contract line 5's refusal is weighed against). Table in
   cmd/tools/README.md. Never writes; never touches prod.
5. Worker 0.3.0's client_build row(s) + wrangler deploy on the production
   account — WITH RB-9's capture armed FIRST (Step 0.1): this is the next
   CONTENT-changing deploy, the one that reproduces the first-request 403.

Step 0 — probes, answers into WP-EDGE-2B.md: 0.1 RB-9's capture — arm
tcpdump on the box (`tcpdump -i lo -s0 -w /tmp/rb9.pcap tcp port <active>`;
sudo via the Keychain) BEFORE the 0.3.0 deploy, fire the signed
pass-through probe (the Part A recipe: If-None-Match + Range, the bench
key, a nonexistent bucket) within 15 s, diff the failing request's headers
against a succeeding one in the pcap; the probe writes nothing. 0.2 the
multi-module no_bundle deploy: does `wrangler deploy` with extra modules
keep each file byte-identical and downloadable (`…/content/v2` parts)? How
does the manifest row cover them — decide. 0.3 WebCrypto AES-GCM segment
rate in workerd (`wrangler dev`) and the e2ee.wasm load cost per isolate;
0.4 the chunker's rate in workerd (node was 123 MB/s); 0.5
`encodeBody: "manual"` + an un-decoded subrequest for RP66-1's residual
(both backends): would stored-Content-Encoding objects be servable at the
edge? Probe only; the fix is a later part.

Tests (red first): the pipeline's node tests with a FAKE control plane and
backend (the index/commit shapes from the harness's JSON); the golden
boundaries THROUGH the Worker's TransformStream path; a sealed chunk from
the Worker OPENS with crypto.OpenWrappedChunk (export a test vector from
the node test, open it in a Go test — the cross-implementation pin, as
TestE2EE_WASMMatchesNative does); the 503 on a mid-pipeline failure (never
a silent fallback with a consumed body); the batch split at 64 chunks; the
back-pressure bound. Structural: the client manifest rows; `make
test-edge` carries everything.

Architecture notes
- Contract lines 4, 5, 6 cited in every PR body. The Worker holds the
  public key and per-chunk keys for the request only; sha256(chunk) goes
  only to the control plane over the service token; nothing new is stored.
- On prod: deployed DARK (edge_codec OFF everywhere; without a master key
  the admit answers the whole PUT anyway). The flag stays OFF; the owner's
  post-F-14 session flips it (the chain's last bullet).
- Closeout: write Prompt 47's text refreshed (WP-EDGE-3, the data node —
  the chain's summary + current line numbers) into this file; WP-EDGE-2B.md
  with Step 0 / decisions / residuals; STATUS; plan 40.10; the ledger row.
  The post-merge review is RP70.
- Commit: feat(edge,tools): WP-EDGE-2 Part B — … [WP-EDGE-2]; merge with
  `gh pr merge --auto` (merge queue; never --admin).

ultrathink

## The chain (owner 2026-10-26: "build everything now") — each closeout writes the next prompt here

Serial, one deep code lane; each prompt's Part 0 is the previous prompt's
residuals; each post-merge review takes the next RP number. Nothing below needs
the owner; the two switches that wait for the F-14 ceremony are named in
Prompt 46. The scopes here are what the writer of each prompt expands with
current line numbers (the /next conventions).

- **Prompt 45 — WP-EDGE-1 Part B, reads — DONE 2026-10-26** (`docs/reviews/WP-EDGE-1B.md`; the post-merge review is RP66). As issued, with the four corrections the note records. The text below was Prompt 44's closeout. GetObject (and the Range form)
  through the Worker: admit (SigV4 via ValidateDescribed, scope, the head row —
  a whole unsealed object on an EdgeWriter backend; chunked, sealed, SSE-C,
  tape, parity-rebuild and `sia` rows pass through to the origin) → the egress
  meter counts the object's bytes at admit (`internal/api/egress_meter.go`;
  the live counter + `bandwidth_usage_daily` as the origin's GET does) → the
  `egress_throttle` decision answered as 503 SlowDown + Retry-After FROM ADMIT
  (the Worker never paces; the floor and MAX_STREAMS rules unchanged) → a
  presigned ranged GET (TTL 15 min, `Range` passed through, If-Match the
  backend ETag so a concurrent overwrite cuts the body, never splices) →
  stream; the commit reports bytes served (metering truth) and the response
  headers are the origin's (ETag, content-type, x-amz-* from the row, version
  id, storage class via engine.CustomerStorageClass). HEAD stays at the origin
  (Decision 2, cache-only). The `parallel_get` whole-object path at the edge
  is a later part. Tests: the egress rows equal a GET through the origin;
  the throttled tenant's 503 from admit; a Range GET's bytes; a sealed row
  passes through; the access-log row; a lost commit still meters (the reaper
  reads the admit's size). Live proof: the Lightroom zip back through
  s3.stored.cloud with MD5 equal, the origin's access log bytes_out 0.
- **Prompt 46 — WP-EDGE-2 Part A — DONE 2026-10-27** (`docs/reviews/WP-EDGE-2A.md`; #994 Part 0 + the Worker 0.2.1 deployed with RB-9 reproduced and narrowed, #995 the codec, #997 the control plane + migration 111; the post-merge review is RP68). As rewritten 2026-10-27 by the RP66 review; the 2026-10-26 text asked one session for Part A and Part B together and had two design errors — RP66-8). Built and proven with `ENCRYPTION_MASTER_KEY_FILE` in the lab; deployed to prod DARK (flag `edge_codec`, per tenant; without a master key the admit answers Part A's whole PUT). (a) The frozen chunker (WP-R8-4) compiled to WASM from the Go module (client `edge-chunker`), pinned to golden boundaries in Go and node. (b) The dual key: the tenant's ML-KEM-768 public key handed out by admit, a random key per chunk, the chunk sealed in the current (0x04) stream layout under it, the key wrapped with HPKE (X-Wing, sdk/go/e2ee) — a NEW format byte **0x06** (0x05 is the SSE-C multipart header), opened on the control plane under the tenant private seed. (c) The keyed index: the data-plane host sends sha256(chunk); the CONTROL PLANE applies the tenant index key (HKDF of the key version's chunk key, label `stored/index/v1`) in memory — the index stores the keyed hash only (`TestIndexHoldsNoPlainHash`), dedup per tenant by construction; misses get presigned PUTs into `_global` under a claim (chunk_claim.go's takeover window), the manifest is written at commit as storeChunkedObject writes it. Part A's data-plane host is a Go harness. Part 0 = RP66-1/2/3/5/6/7. Contract lines 4, 5, 6 cited.
- **Prompt 46b — WP-EDGE-2 Part B** (ISSUED — the full text is the `## Stage 1a Prompt 46b` section above; summary): the Worker 0.3.0 pipeline (the WASM chunker, sha256, the index call per 64 chunks, WebCrypto AES-GCM segments + the HPKE wrap from sdk/wasm, a PUT per miss, the commit; back-pressure; the per-stream rate measured — the baseline says JavaScript in the byte path halves a stream), the lab proof through wrangler dev against the harness's rows, `cmd/tools/dedup-measure` (F-3: the cross-tenant fraction of a lab corpus, the number the owner decides F-37 with), `encodeBody: "manual"` probed for RP66-1's residual. On prod the two switches that wait for F-14 stay: `edge_codec` ON and Prompt 48's `standard_sealed_default`.
- **Prompt 47 — WP-EDGE-3, the data node.** The binary in node mode
  (`VAULTAIRE_ROLE=node`: no DB, no routes but the mover's, the same admit/
  commit protocol over the service token, the Worker's chunked decoder and
  digests in Go) and `data_plane_hosts` (migration: host, kind
  edge|node|slc, weight, health, last_seen) with the control plane picking a
  host per admit by weight × health; `host=slc` is the origin's own write
  path re-expressed as a node of the protocol (the handlers call admit/commit
  in-process), so the metrics families carry every byte by host. Proven with
  two processes in the lab (control plane + node on another port) and on
  prod as `host=slc` beside the Worker at weight 0 → 1. A real unmetered node
  is an owner purchase later (a VPS from a phone; `deploy/node/` carries the
  unit and the one-line install).
- **Prompt 48 — slice 3 + the dark seal-by-default flag + the catalogue
  filter.** (a) The second-copy policy at the edge: an object under 8 MiB is
  mirrored — admit answers two presigned PUTs (the placement's backend + the
  mirror leg from the plan's leg order), the Worker tees one stream into two
  fetches, commit records both addresses (a `mirror` column on the head row
  or a `blob_copies` table — decide in the prompt, migration), the read path
  falls back to the mirror on a miss; 8 MiB and up: k+m RS stripes with the
  WP-VAULT-1 codec compiled to WASM (`tools/edge-ec-worker/codec`'s Rust is
  the fallback), data shards on fast legs, parity on slow, no vendor above m,
  a read touching data shards only; mirror first on prod (Lyve + iDrive;
  tenant zero's backups get their second copy — the owner's single copy on
  Lyve ends here). (b) `standard_sealed_default` (global, default OFF, 409
  without a master key like `credential_plaintext_drop`): ON = every Standard
  write is placed sealed (SSE-S3 at the edge via Prompt 46's codec, or at the
  origin for the pass-through cases) — the switch that makes modes.json's
  sentence true, built dark, flipped after F-14 and after Prompt 46 is ON.
  (c) 10.3's public-catalogue filter: a versioned Bloom filter of the
  public-artefact namespace's tags served from R2, the SDK and the Worker
  check locally, references only; the namespace itself (10.3/10.6's tables)
  seeded with one artefact set in the lab.
- **After the ceremony (one session, owner present only for the ceremony
  itself):** `edge_codec` ON for tenant zero → the sealed-object proofs →
  `standard_sealed_default` ON → `credential_plaintext_drop` (Prompt 43 B3) →
  the key-dependent residuals of Prompts 46 and 48.
