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

## Stage 1a Prompt 46b: WP-EDGE-2 Part B — the Worker 0.3.0 pipeline, the lab proof, dedup-measure (Part 0: RP68-1…6)

*Refreshed 2026-10-27 by the RP68 review (`docs/reviews/WP-EDGE-2A-REVIEW.md`):
Part 0 is RP68's list, and RP68-1 changes WHAT the pipeline admits — every
edge-eligible write of a codec tenant, not only the origin's chunked
placements. Two editions follow: this one (the chain's), and an Opus edition
below with the same scope as numbered steps. Paste ONE of them.*

Continue from WP-EDGE-2 Part A (2026-10-27: #994 Part 0 + the Worker 0.2.1,
#995 the codec, #997 the control plane + migration 111; note
docs/reviews/WP-EDGE-2A.md — read § Decisions D-EDGE-16…21 and § Residuals
RB2-1…5 + RB-9) and from its post-merge review RP68
(docs/reviews/WP-EDGE-2A-REVIEW.md — its findings are THIS prompt's Part 0,
in the order its last section gives). Most recent merged PR and open PRs:
`git log origin/main`, `gh pr list`.

Read first: docs/reviews/WP-EDGE-2A.md (the harness
internal/api/edge_codec_harness_test.go is THE ORACLE: the Worker must
produce the same rows — the harness's admit → index → seal → PUT → commit
sequence is exactly the Worker's pipeline, and its tests say what each
answer looks like); internal/crypto/edge_codec.go (SealWrappedChunk is the
Go reference of what the Worker must produce byte-compatibly: the 0x06
header, HKDF(chunk key, "stored/edge-codec/stream-v1" ‖ header) → AES-256-GCM
1 MiB segments with the 0x04 nonce/last-flag layout, the chunk key wrapped
with sdk/wasm's sealTo under info "stored/edge-codec/chunk-key/v1");
internal/crypto/stream.go (`streamAEAD`, `streamAAD`, `newStreamSealer` —
the exact nonce and AAD rules; never guess them); sdk/chunkerwasm (the
chunker's WASM API: create/push/finish/free); edge/s3-ingest/src/index.js
and README; bench-results/EDGE-RELAY-2026-10-26.md (JS in the byte path
halves a stream — slice 2 puts WASM in it ON PURPOSE; this prompt measures
what it costs); contract lines 4, 5, 6 (cite in the PR body).

Lane: Engine. No migration expected (next: 112 — claim it only if Part 0
forces one). PRs: (0) RP68's fixes — the control plane first, the Worker is
written against the corrected admit; (A) the Worker 0.3.0 + its node tests;
(B) cmd/tools/dedup-measure; (C) the docs PR (WP-EDGE-2B.md, STATUS, plan
40.10 + the three § 40.5 sentences RP68 C-3/C-4/C-5, CLAUDE.md rows, Prompt
47's text refreshed in this file).

What to build
0. PART 0, in RP68's order: **RP68-1** the edge eligibility rule — a codec
   tenant's whole PUT ≥ EDGE_MIN_BYTES and EVERY part of a multipart upload
   is codec when the bucket allows chunking (`!versioned`, `!byTier`,
   `!byRegion`, not SSE-C, the `chunking` flag) — `placementRequest.EdgeCodec`
   makes `decidePlacement` treat the size as over the threshold so every
   other rule stays the one decision; the grant's `chunk_threshold` =
   EDGE_MIN_BYTES; the harness gains a 12 MiB whole PUT and an 8 MiB-part
   upload (chunked rows at the edge where the origin stores whole — the note
   says so). **RP68-2** the commit waits on a young claim (chunk_claim.go's
   backoff, ≤ 60 s on the commit's clock) before `errEdgeChunkVanished`;
   **RP68-6** the hit's `size_bytes` check; **RP68-3** the HEAD sample at
   commit (the first stored chunk + 1 in 32; a mismatch refuses the commit,
   `head_mismatch`), `routing_truth`'s chunked sample reads codec chunks
   through `openEdgeCodecChunk`; **RP68-5** `chunk_move` verifies AES256-EW
   by MD5; **RP68-4** the metric names (and the endpoint presence test
   #1000 added — RB2-6: the families were never registered before it).
   Each with its red test. Start after #1000 is on main.
1. THE WORKER 0.3.0 (edge/s3-ingest): a codec admit (admit.codec set) runs
   the pipeline instead of moveBody — the request body through a
   TransformStream into the WASM chunker (chunker.wasm + wasm_exec.js
   shipped as additional modules beside index.js; `no_bundle` keeps them
   separate files — Step 0.2 decides how a multi-module Worker's
   client_build rows are computed: one row per module file is the default
   answer), sha256 per chunk (WebCrypto digest), the index call per 64
   chunks (one /index call per batch; the LAST batch may be smaller), per
   miss: seal (WebCrypto AES-GCM per 1 MiB segment under a random key from
   crypto.getRandomValues; HKDF via WebCrypto deriveBits with the info =
   "stored/edge-codec/stream-v1" ‖ header; the nonce layout stream.go's;
   the wrap via sdk/wasm's sealTo — load e2ee.wasm too, or extract the
   HPKE seal into a smaller module — measure both, Step 0.3) and PUT to the
   link with Content-Length = sealed_length; back-pressure: at most N
   chunks buffered (the Workers memory limit is 128 MB — N ≤ 4 of 16 MiB
   worst-case, measure), the body read paced by the PUTs; then the commit
   with the ordered chunk list + body_md5 (MD5 in WASM or JS — the ETag;
   measure its cost). A pipeline failure ANYWHERE: the admit is failed via
   commit (outcome edge_error) and the client answered 503 SlowDown +
   Retry-After — a codec admit CANNOT fall back to fetch(request) once a
   byte is read (the body is consumed); say so in the README and the note.
2. THE LAB PROOF through wrangler dev (the D-EDGE-7 topology: aws-cli →
   wrangler dev as HTTP proxy → the lab origin with
   ENCRYPTION_MASTER_KEY_FILE and EDGE_TRUST_ANY_PEER): a 100 MiB `aws s3
   cp` admitted codec, chunked by the WASM chunker at the GOLDEN
   boundaries, sealed, stored, committed; the origin reads it back
   MD5-equal; a re-upload = 100 % hits and 0 chunk PUTs; the rows equal the
   HARNESS's for the same body (the oracle — compare manifests row by row
   in SQL); an aws-cli multipart upload with DEFAULT 8 MiB parts (RP68-1)
   AND one with ≥ 64 MiB parts; a body with a repeated 4 MiB region across
   parts (RP68-2: parallel parts sharing chunks, no 503); a mid-body cut
   (kill aws-cli) → the claims dropped by the commit/reaper; a TAMPERED
   host (a test build that reports a wrong ETag; one that seals under the
   wrong key) → the commit refused (RP68-3); the per-stream RATE measured
   against Part A's pass-through baseline and the isolate's memory watched
   (`wrangler dev` reports it); the index call's latency per 64-chunk batch
   (RP68-7's number).
3. CMD/TOOLS/DEDUP-MEASURE (F-3): the number the owner decides F-37 with.
   A read-only tool against a lab corpus (a directory tree, or a bucket via
   the S3 API with a provided key): chunk every file with the frozen
   chunker, hash, and report the dedup fraction WITHIN one tenant and
   ACROSS N simulated tenants (the corpus split by top-level directory =
   one "tenant" each): total bytes, unique bytes, the cross-tenant-only
   fraction (what a global index would save beyond per-tenant dedup — the
   number contract line 5's refusal is weighed against). Table in
   cmd/tools/README.md. Never writes; never touches prod.
4. Worker 0.3.0's client_build row(s) + wrangler deploy on the production
   account — WITH RB-9's capture armed FIRST (Step 0.1): this is the next
   CONTENT-changing deploy, the one that reproduces the first-request 403.

Step 0 — probes, answers into WP-EDGE-2B.md: 0.1 RB-9's capture — arm
tcpdump on the box (`tcpdump -i lo -s0 -w /tmp/rb9.pcap tcp port <active>`;
sudo via the Keychain) BEFORE the 0.3.0 deploy, fire the signed
pass-through probe (the Part A recipe: If-None-Match + Range, the bench
key, a nonexistent bucket) within 15 s, diff the failing request's headers
against a succeeding one in the pcap; the probe writes nothing. 0.2 the
multi-module no_bundle deploy: does `wrangler deploy` with extra modules
keep each file byte-identical and downloadable (`…/content/v2` parts)? One
manifest row per module file unless the probe forbids it. 0.3 WebCrypto
AES-GCM segment rate in workerd (`wrangler dev`) and the e2ee.wasm load
cost per isolate; 0.4 the chunker's rate in workerd (node was 123 MB/s);
0.5 `encodeBody: "manual"` + an un-decoded subrequest for RP66-1's residual
(both backends): would stored-Content-Encoding objects be servable at the
edge? Probe only; the fix is a later part.

Tests (red first): the pipeline's node tests with a FAKE control plane and
backend (the index/commit shapes from the harness's JSON); the golden
boundaries THROUGH the Worker's TransformStream path; a sealed chunk from
the Worker OPENS with crypto.OpenWrappedChunk (export a test vector from
the node test, open it in a Go test — the cross-implementation pin, as
TestE2EE_WASMMatchesNative does); the 503 on a mid-pipeline failure (never
a silent fallback with a consumed body); the batch split at 64 chunks; the
back-pressure bound; Part 0's six. Structural: the client manifest rows;
`make test-edge` carries everything.

Architecture notes
- Contract lines 4, 5, 6 cited in every PR body. The Worker holds the
  public key and per-chunk keys for the request only; sha256(chunk) goes
  only to the control plane over the service token; nothing new is stored.
- On prod: deployed DARK (edge_codec OFF everywhere; without a master key
  the admit answers the whole PUT anyway). The flag stays OFF; the owner's
  post-F-14 session flips it (the chain's last bullet).
- Closeout: write Prompt 47's text refreshed (WP-EDGE-3, the data node —
  the drafts below + current line numbers) into this file; WP-EDGE-2B.md
  with Step 0 / decisions / residuals; STATUS; plan 40.10; the ledger row.
  The post-merge review is RP70.
- Commit: feat(edge,tools): WP-EDGE-2 Part B — … [WP-EDGE-2]; merge with
  `gh pr merge --auto` (merge queue; never --admin).

ultrathink

## About the Opus editions (read once)

The same scope as the chain's prompt above it, written for a session that
runs on Claude Opus rather than Fable: every decision is made in the text
(nothing to "decide — say so"), the work is numbered steps each ending in a
command that proves it, the files and functions are named, and the session
is told what to do when a step does not go as written. Rules for an Opus
session, in every edition:

- No `ultrathink` line. Read every file the prompt names BEFORE the first
  edit; read `CLAUDE.md`, `internal/api/CLAUDE.md`, `internal/crypto/CLAUDE.md`
  and `docs/reviews/WP-EDGE-PROMPTS.md`'s chain section first.
- TDD: every step's test is written and seen RED before the code. A step
  that cannot be made red-then-green as written is a STOP: write what you
  found into the closeout note's "open questions" and go on to the next
  independent step; never improvise a design.
- Never change a format byte, a derivation label, a chunker constant, a
  migration already on main, or a decision D-EDGE-n. Never run the
  vaultaire binary to "check it" (it boots on :8000). Never `pkill` by
  pattern on SLC. Secrets never printed.
- Every PR body cites contract lines 4, 5, 6; commits end with the
  reminder's attribution lines; merge with `gh pr merge --auto`, never
  `--admin`; the shared checkout is never `git checkout`-ed when another
  session may be on it — work in `git worktree add ../<name> origin/main`.
- The closeout note has the same sections as the chain's prompt says (Step
  0 answers, decisions, residuals, open questions), and the ledger row in
  memory is updated, never a new line added.

## Stage 1a Prompt 46b — Opus edition

You are building WP-EDGE-2 Part B in the Vaultaire repository
(`/Users/viera/fairforge/vaultaire`). Part A (merged: #994, #995, #997;
note `docs/reviews/WP-EDGE-2A.md`) built the codec in Go and the control
plane; the data-plane host in Part A is a Go test harness. Part B makes
the Cloudflare Worker `stored-s3-edge` (edge/s3-ingest, 0.2.1 today) that
host, version 0.3.0. Its post-merge review `docs/reviews/WP-EDGE-2A-REVIEW.md`
(RP68) lists fixes that come FIRST. Work in a worktree from `origin/main`.

Read before editing (in this order): docs/reviews/WP-EDGE-2A.md;
docs/reviews/WP-EDGE-2A-REVIEW.md; internal/api/edge_codec_harness_test.go
(the oracle: `uploadCodec`, `index`, `storeMisses`, `commit` show every
request and answer shape); internal/api/edge_codec_control.go;
internal/api/edge_codec_multipart.go; internal/api/edge_ingest.go (the
admit/commit types at lines 120–300, `edgeAdmitWrite` ~503, `settleEdgeAdmit`
~1124); internal/api/placement.go (`decidePlacement` ~159); internal/crypto/
edge_codec.go and stream.go (`streamAEAD`, `streamAAD`, `newStreamSealer`,
`StreamHeaderWrapped`, `StreamWrapLen`); sdk/chunkerwasm/main.go;
sdk/wasm/main.go (`sealTo`); edge/s3-ingest/src/index.js, README.md,
test/*.test.js, wrangler.toml; Makefile targets `test-edge`,
`edge-chunker-wasm`, `sdk-wasm`; scripts/build-client.sh;
internal/translog/clientbuilds/manifest.json.

The wire shapes (from the harness; do not invent fields):
- admit → `{edge:true, admit_id, codec:{version, kem_public_key (base64,
  1216 bytes), chunk_threshold, segment_size}, expires_at}` and NO `url`.
- index: `POST /api/v1/edge/index` `{admit_id, chunks:[{hash: sha256 hex,
  size}]}` → `{hits:[hash…], misses:[{hash, url, send_headers, sealed_length}]}`.
- commit: `POST /api/v1/edge/commit` `{admit_id, outcome:"ok", chunks:
  [{hash, size, sealed_size, backend_etag, status:"stored"|"hit"} in body
  order], body_md5: hex}` → the relayed S3 answer. A failure: `{admit_id,
  outcome:"edge_error"}`.
- Every call carries `X-Vaultaire-Edge-Token` as today.

PR 0 — the RP68 fixes (control plane, Go). Branch `stage1a-p46b-part0`.
1. RP68-1. Test first in edge_codec_harness_test.go: (a) a 12 MiB whole PUT
   of a codec tenant is admitted codec (`resp.Codec != nil`, no `url`);
   (b) a multipart upload whose first part is 8 MiB is admitted codec and
   its Complete reads back; (c) a 2 MiB PUT (under EDGE_MIN_BYTES) is
   `pass_through_small` as before. Run `go test ./internal/api/ -run
   TestEdgeCodec -v` — red. Then: add `EdgeCodec bool` to
   `placementRequest` (placement.go); in `decidePlacement` compute
   `overThreshold := req.Size > chunkThreshold || req.EdgeCodec` and use it
   where `req.Size > chunkThreshold` is used today (two places); in
   `edgeAdmitWrite` (edge_ingest.go ~612) call the codec gate for a PUT
   when the tenant has the `edge_codec` flag by re-deciding placement with
   `EdgeCodec: true` (one extra call only when the flag is on); in
   `edgeAdmitCodecPart` (edge_codec_multipart.go ~66) replace the first
   part's "own size places chunked" with the same `EdgeCodec: true`
   decision. The grant's `ChunkThreshold` (edge_codec_control.go ~112) =
   the server's EDGE_MIN_BYTES value. Green. Add one sentence to
   docs/ENCRYPTION.md § the edge codec: "at the edge every write ≥ 4 MiB of
   a codec tenant is a chunked row; the origin would store 4–64 MiB whole".
2. RP68-2. Test: two part admits (parts 1 and 2 of one upload) whose bodies
   share one chunk; part 2's index answers the shared chunk as a hit; part
   2 commits FIRST in a goroutine while part 1 has not committed; assert
   part 2's commit returns 200 after part 1's commit lands (use a channel
   to release part 1 after 200 ms), never 503. Red. Then in
   `referenceEdgeChunk` (edge_codec_control.go ~616): when the UPDATE hits
   0 rows, SELECT the row's `state, pending_since`; if `pending` and
   younger than `edgeCodecTakeover`, sleep with chunk_claim.go's backoff
   (25 ms doubling to 1 s) and retry the UPDATE until 60 s have passed on
   a `context.WithTimeout` of the commit's context, then return
   `errEdgeChunkVanished`; if the row is gone, return `errEdgeChunkVanished`
   at once. Green.
3. RP68-6. Test: a commit whose hit entry carries a size different from the
   row's `size_bytes` is refused (400 `InvalidRequest`). Red. Then
   `referenceEdgeChunk` RETURNING `size_bytes` too; `settleEdgeCodecChunks`
   compares it to `c.Size` and fails. Green.
4. RP68-3. Test: the fake backend of the fixture gains a `HeadObject`
   counter; a commit of 40 stored chunks makes exactly 2 HEADs (the first
   and the 33rd); a commit whose first stored chunk's reported ETag differs
   from what the fake backend holds is refused and gives everything back
   (refs, claims, reservation; `edgeCodecChunks{outcome="head_mismatch"}`
   = 1). Red. Then in `settleEdgeCodecChunks`'s `stored` branch: for the
   first stored chunk and every 32nd after it, HEAD the blob through the
   chunk store (`chunkStore.head` — add it beside `exists`, returning ETag
   and size, on `engine.ChunkContext`) and compare the ETag (unquoted,
   lower-case) and the size to `c.BackendETag` / `c.SealedSize`; a
   mismatch → `fail(errEdgeChunkHeadMismatch)` → the commit answers 400
   `InvalidRequest` with a suggestion naming the mismatch. Then in
   routing_truth.go's chunked sample: a chunk row with `encryption_algo =
   'AES256-EW'` is read through `openEdgeCodecChunk` (the read probe's
   `unreadable` verdict). Test: a codec object whose chunk blob is
   overwritten with other bytes of the same length is `unreadable` in the
   next run. Green.
5. RP68-5. Test: chunk_move's dry run over one `AES256-EW` row reports it
   `at_address` (not missing/damaged). Red. Then the verify branch: when
   the row's `encryption_algo` is `AES256-EW`, verify by MD5 of the blob
   against `ciphertext_hash`. Green.
6. RP68-4. Rename the metric outcomes: index-time `hit` / `miss`;
   commit-time `referenced` / `stored` / `head_mismatch` / `failed`; the
   reaper's codec expiry counts `claims_dropped` instead of `retired`.
   Update the `init()` pre-creation lists and the Help strings. Tests
   asserting labels updated — including the presence assertions #1000
   added to `TestMetricsEndpoint_KeepsLegacySeries`
   (internal/api/prom_metrics_test.go) for the new outcome labels; keep
   that test reading the ENDPOINT, not the vec (RB2-6's lesson: the three
   families were declared, pre-created and never registered until #1000).
7. `make lint && make test` green; `make test-db` then `go test -race
   ./internal/api/ -run 'TestEdgeCodec|TestIndexHolds|TestChunkMove|TestRoutingTruth'`.
   PR title: `fix(api): WP-EDGE-2 Part B Part 0 — the RP68 fixes [WP-EDGE-2]`.
   Body: the six items with their test names. `gh pr merge --auto`.

PR A — the Worker 0.3.0. Branch `stage1a-p46b-worker`, after PR 0 merged.
8. Step 0 probes, each answered in a scratchpad file you will paste into
   the note: 0.2 `wrangler deploy --dry-run --outdir` of a Worker with
   index.js + chunker.wasm + wasm_exec.js + e2ee.wasm as modules: list the
   output files and their sha256; decision: one manifest row per module
   file (`edge-ingest` for index.js, `edge-chunker` for chunker.wasm,
   `sdk-wasm` for e2ee.wasm when its bytes equal the sdk's build, a new
   closed-list client `edge-wasm-exec` for wasm_exec.js — add it to
   internal/translog/entries.go's list). 0.3 in `wrangler dev`: time
   WebCrypto AES-GCM over 64 × 1 MiB segments and the e2ee.wasm
   instantiate; 0.4 the chunker's rate on 100 MiB in `wrangler dev`.
   Write the numbers down before step 9.
9. Node test first (edge/s3-ingest/test/codec.test.js) with a fake control
   plane (an HTTP server answering admit/index/commit in the harness's
   shapes) and a fake backend (an HTTP server storing PUT bodies and
   answering an MD5 ETag): a 20 MiB body through the Worker's pipeline
   function makes index calls of ≤ 64 chunks, one PUT per miss with
   `content-length` = sealed_length, and a commit whose chunk list matches
   the fake backend's ETags; the golden body's boundaries through the
   TransformStream equal `internal/crypto/testdata/chunker_golden.json`.
   Red (the function does not exist). Then write `src/codec.js`:
   `async function runCodec(request, admit, env, ctx)` — the body through
   a TransformStream whose transform feeds `storedChunker.push`, each
   completed chunk length cutting a Uint8Array from the buffer; per chunk:
   `crypto.subtle.digest("SHA-256", chunk)`; chunks queued until 64 (or
   the end) → one index call; per miss: `sealChunk(pub, version, chunk)`
   (step 10) → `fetch(url, {method:"PUT", headers: send_headers, body:
   sealed})`, the ETag from the response; hits recorded; at most 4 chunks
   between hash and PUT at once (a counting semaphore; the reader awaits
   it — this is the back-pressure); after the last PUT: the commit with
   the ordered list and the body's MD5 (compute MD5 in JS over the stream
   — a small pure-JS MD5 is acceptable here; measure it). Any throw →
   commit `{outcome:"edge_error"}` and respond 503 with `Retry-After: 30`
   and the S3 `SlowDown` XML; never `fetch(request)` after the body was
   read. In index.js: when `admit.codec` is set, call `runCodec` instead
   of `moveBody`. Green.
10. The sealer, test first: `test/seal.test.js` seals a 3 MiB chunk under a
    public key exported by the Go test
    `TestEdgeCodec_ExportVectorForNode` (write it: it derives a lab key
    pair with a fixed seed and writes `testdata/edge_codec_node.json`
    {seed hex, public key base64}); the sealed bytes are written to a file
    the Go test `TestEdgeCodec_NodeSealedChunkOpens` reads and opens with
    `crypto.OpenWrappedChunk` under the same seed, comparing the plaintext.
    Run the Go side after the node side; both must pass. Then write
    `src/seal.js`: chunkKey = 32 random bytes; wrap =
    `storedE2EE.sealTo(pub, chunkKey, utf8("stored/edge-codec/chunk-key/v1"),
    null)` (1169 bytes — assert); header = [0x06, 0x03] ‖ version u32 BE ‖
    wrap ‖ 1048576 u32 BE ‖ 7 random bytes; DEK = HKDF-SHA256(chunkKey,
    salt = empty, info = utf8("stored/edge-codec/stream-v1") ‖ header, 32
    bytes) through `crypto.subtle.deriveBits`; for segment i of ≤ 1 MiB:
    nonce = prefix(7) ‖ i u32 BE ‖ (last ? 1 : 0) — read `stream.go`'s
    nonce function and copy its byte order exactly; AES-256-GCM with
    additionalData = header; output = header ‖ ct_0 ‖ … ‖ ct_{n-1}; assert
    the total equals `sealed_length` from the index answer.
11. Back-pressure and memory: a node test that feeds a 200 MiB body through
    the pipeline with a fake backend that delays every PUT by 50 ms asserts
    the peak number of chunks held ≤ 4 (instrument the semaphore). Then in
    `wrangler dev`, upload 1 GiB and read the isolate memory it reports;
    if it exceeds 100 MB, lower the bound to 3 and re-measure. Write both
    numbers into the note.
12. Lab proof (write a script `edge/s3-ingest/lab/codec-proof.sh` that does
    it; keep it in the repo). Origin: a local vaultaire with
    `ENCRYPTION_MASTER_KEY_FILE`, `EDGE_SERVICE_TOKEN`, `EDGE_TRUST_ANY_PEER=true`,
    flags `edge_ingest`, `edge_codec`, `chunking` ON for the lab tenant (via
    the admin API). Worker: `npx -y wrangler@4.151.3 dev` with the origin
    as upstream; aws-cli with the lab key through it as an HTTP proxy
    (`HTTPS_PROXY`/`--endpoint-url`, as Part A's lab did — read
    docs/reviews/WP-EDGE-1A.md § Step 0 for the exact topology).
    Run and record: (a) `aws s3 cp` of a 100 MiB file: admitted codec,
    chunk count = the golden count for that body if you use
    `crypto.GoldenBody` bytes (write them with a tiny Go program), read back
    MD5-equal; (b) the same file again: 0 PUTs on the backend, 100 % hits;
    (c) the SQL comparison: `tenant_chunk_refs` of the Worker's object vs
    the harness's for the same body (run the harness upload against the
    same lab DB with `go test -run TestEdgeCodecHarness_EndToEnd` pointed
    at it, or replay its SQL) — same count, offsets, sizes; (d) an aws-cli
    multipart with default 8 MiB parts and one with `--cli-write-timeout`
    irrelevant, `multipart_chunksize 64MB`; (e) a 300 MiB body made of a
    4 MiB block repeated (parallel parts sharing chunks) — no 503 in the
    aws-cli debug log; (f) kill aws-cli at 50 %: within 20 min + 5 the
    reaper's run shows `claims_dropped`; (g) two tampered Worker builds
    (`wrangler dev` with `CODEC_TAMPER=etag` / `=key` env read only in a
    test branch of seal.js — never deployable: assert at module load that
    `CODEC_TAMPER` is unset when `env.ENVIRONMENT === "production"`): the
    commit is refused 400, nothing stored for the object; (h) throughput:
    1 GiB through 0.3.0 codec vs the same through 0.2.1 pass-through, MB/s
    both, and the index call's p50/p95 from the Worker's log lines.
13. README: a § "0.3.0 — the codec pipeline" (what runs, the 503 rule, the
    memory bound, the module files and their manifest rows). Manifest rows
    for every module file (sha256 of the deploy artefacts from step 8's
    recipe; the Deploy repro step must rebuild them — extend
    scripts/repro-check-clients.sh for the new clients). `make test-edge`
    runs every node test. PR title `feat(edge): WP-EDGE-2 Part B — the
    Worker 0.3.0 codec pipeline [WP-EDGE-2]`; body cites contract 4, 5, 6,
    the Step 0 numbers and the lab proof's (a)–(h).

PR B — `cmd/tools/dedup-measure`. Branch `stage1a-p46b-dedup-measure`.
14. Test first (`cmd/tools/dedup-measure/main_test.go`): a temp tree with
    three top-level directories holding files whose bytes overlap in known
    amounts (write them from `crypto.GoldenBody` slices) yields the
    expected per-tenant and cross-tenant unique byte totals. Red. Then the
    tool: flags `-dir <path>` or `-bucket <name> -endpoint <url>` (reads
    `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` from the environment,
    never a flag), `-tenants-by topdir`; chunk every file with
    `crypto.DefaultChunker()`, sha256 per chunk; output a table: tenants,
    files, total bytes, unique bytes within each tenant (sum), unique bytes
    across all, and `cross_tenant_only_fraction = (sum of per-tenant unique
    − global unique) / total`. Never writes. Row in cmd/tools/README.md.
    Confirm `go list -deps ./cmd/vaultaire` does not include it. PR title
    `feat(tools): dedup-measure — the F-3 number [WP-EDGE-2]`.

PR C — docs. Branch `stage1a-p46b-docs`.
15. docs/reviews/WP-EDGE-2B.md: Step 0 answers (0.1–0.5 with numbers),
    what was built (PR 0, A, B), the lab proof table (a)–(h), decisions
    (number them D-EDGE-22…), residuals (number them RB3-n), open
    questions. docs/STATUS.md header line. docs/IMPLEMENTATION_PLAN.md:
    one status bullet under § 40.10 after the Part A one; the three § 40.5
    sentences from RP68 C-3/C-4/C-5 (one line each: "a request through the
    dispatcher is billed as one, whatever it serves"; "no Cron Triggers /
    Queue consumers on user Workers is inferred from the API surface and
    builder reports, not documented"; Dynamic Workers "GA in practice since
    2026-10-22, billed since 2026-05-26, Workers Paid only"). CLAUDE.md:
    the `edge_admits` bullet's codec sentence gains "every edge write ≥ 4
    MiB of a codec tenant"; edge/s3-ingest/README. In this file
    (WP-EDGE-PROMPTS.md): refresh Prompt 47's two editions with the line
    numbers as they are after your PRs, and mark 46b DONE in the chain.
    Memory: update the ledger row 46B (never a new line).
16. Deploy, LAST, with RB-9's capture armed: on the box, `sudo tcpdump -i lo
    -s0 -w /tmp/rb9.pcap tcp port <ACTIVE_PORT>` (the port from
    /opt/vaultaire/ACTIVE_PORT; sudo password in the Keychain
    `vaultaire-slc-sudo`); then from edge/s3-ingest `source
    ~/fairforge/.cloudflare-creds.env; CLOUDFLARE_API_TOKEN=$CLOUDFLARE_ZONE_TOKEN
    CLOUDFLARE_ACCOUNT_ID=$CLOUDFLARE_PROD_ACCOUNT_ID npx -y wrangler@4.151.3
    deploy`; within 15 s fire the signed pass-through probe (a GET with
    If-None-Match + Range of a nonexistent bucket using `.env.bench`'s
    `VAULTAIRE_BENCH_*` pair against s3.stored.cloud — 404 = auth passed,
    403 = the bug); stop tcpdump; `tcpdump -r` the pcap for the two
    requests, diff their headers, write the diff into WP-EDGE-2B.md § RB-9,
    delete the pcap. The flag `edge_codec` stays OFF on prod. Verify
    `curl -s https://stored.ge/health` and the Worker's version in the
    Cloudflare dashboard's deployments list.

Done means: PR 0, A, B, C merged through the queue; the lab proof table
filled; the deploy's manifest rows in the log (`translog-verify -client`
on each module file); the ledger row updated; Prompt 47 refreshed here.

## Stage 1a Prompt 47: WP-EDGE-3 — the data node (`VAULTAIRE_ROLE=node`), `data_plane_hosts`, `host=slc` (Part 0: RP70's findings)

*Draft written 2026-10-27 by RP68 ahead of its turn; Prompt 46b's closeout
refreshes it with the line numbers as they are then. Two editions; paste one.*

Continue from WP-EDGE-2 Part B (note docs/reviews/WP-EDGE-2B.md — its
§ Decisions, § Residuals RB3-n, RB-9's capture result) and from its
post-merge review RP70 (docs/reviews/WP-EDGE-2B-REVIEW.md — its findings
are THIS prompt's Part 0; if RP70 has not run, run it first as its own
session). `git log origin/main`, `gh pr list`.

Read first: plan § 40.10 (the principle: the control plane picks a host
per request; "the first hosts are the client and SLC itself, which exist";
"unmetered data nodes running this binary stateless"), contract lines 4,
10 (the attested data plane is NOT this — a node claims nothing of it; say
so in the note), 13 (the tenant is the unit of disclosure — a tenant-scoped
node admits one tenant); docs/reviews/WP-EDGE-1A.md RE-9 (the Cloudflare
peer rule is the Worker's; a node is another peer); the RP68 review's
RP68-3 (a BYO node's reports are never trusted for integrity: the HEAD
sample becomes 100 % for a tenant-scoped host) and the 2026-10-26 COGS
thread's rule (a customer-run host's reports are never trusted for
METERING: a tenant-scoped host's GET is metered at the admitted span, not
at its report); internal/api/edge_codec_harness_test.go (the Go pipeline
that becomes the node's); internal/clientip (`PeerIsCloudflare`);
cmd/vaultaire/main.go (the boot); deploy/ (the unit files' shape).

Lane: Engine. Migration 112 `data_plane_hosts`. PRs: (0) RP70's fixes; (A)
`internal/dataplane` — the pipeline as a package + the node role + the
registry + per-host tokens; (B) `host=slc` + the registry-driven admit +
the steering flag; (C) docs (WP-EDGE-3.md, DEPLOY.md § data nodes,
deploy/node/, STATUS, plan 40.10, CLAUDE.md rows, Prompt 48 refreshed here).

What to build
1. `internal/dataplane`: the Worker's logic in Go, lifted from the harness —
   `Mover` (admit → presigned PUT/GET → commit; the Part A/B shapes) and
   `Codec` (admit → chunk with `crypto.DefaultChunker` → sha256 → index per
   64 → `crypto.SealWrappedChunk` per miss → PUT → commit; the Part B
   shapes), a `ControlPlane` interface with two implementations: HTTP (the
   node: the edge calls over the service channel) and in-process (the
   origin: the same functions called directly — `host=slc`). The harness
   becomes a consumer of this package (its tests keep passing unchanged:
   that is the proof the lift lost nothing).
2. The node role: `VAULTAIRE_ROLE=node` boots `cmd/vaultaire` with NO
   database, NO engine, NO jobs and only the data-plane routes (PutObject,
   UploadPart, GetObject on its own hostname, `/health`, `/metrics`); env
   `NODE_CONTROL_PLANE_URL`, `NODE_TOKEN` (credential → `_FILE` → env, the
   master key's rules), `NODE_PUBLIC_HOST`; a heartbeat every 30 s to
   `POST /api/v1/edge/heartbeat` (version, build sha, load). Everything
   else the binary does is refused at boot in this role (a `STORAGE_MODE`,
   a `DB_HOST` set = a Warn naming what is ignored). `deploy/node/`
   carries the systemd unit and a one-line install (`curl | sh` is NOT it:
   a script that downloads the signed release, verifies it with
   `release-sign verify`, installs the unit).
3. Migration 112 `data_plane_hosts`: `host` (PK, the public hostname),
   `kind` (`edge` | `node` | `slc`), `address` (the peer IP the control
   plane accepts the token from; NULL for `edge` = the Cloudflare rule),
   `token_hash` (NULL for `edge` = the service token), `tenant_id` (NULL =
   a platform host; set = a tenant-scoped host that admits ONLY that
   tenant's requests and whose reports are never trusted — RP68-3 at
   100 %, GET metered at the admitted span), `weight`, `health`
   (`up` | `down` | `draining`), `last_seen`, `version`, `created_at`.
   Seeded with the two hosts that exist: `s3.stored.cloud` (`edge`) and
   `slc` (`slc`, weight 0). Admin routes `GET/PUT/DELETE
   /api/v1/admin/data-plane-hosts/{host}` (admin JWT, audited
   `data_plane.host_set`); the token is minted once and shown once.
4. The edge calls accept a node: `edgeCallAuthorized` becomes "the service
   token from a Cloudflare peer (the Worker) OR a host's token from that
   host's registered address" (constant-time compare against
   `token_hash`; `EDGE_TRUST_ANY_PEER` stays lab-only). Every admit and
   commit records `host` on the `edge_admits` row (migration 112 adds the
   column) and the metric families already labelled `host` carry it.
5. `host=slc`: the origin's own PutObject / UploadPart / GetObject for an
   edge-eligible request (flag `edge_ingest`, ≥ EDGE_MIN_BYTES, the
   pass-through matrix's cases excluded) go through `internal/dataplane`
   in-process — admit and commit as function calls, the bytes through the
   same mover/codec on the same box — so every byte is labelled by host
   and the rows are the ONE shape. Behind flag `data_plane_slc` (per
   tenant, default OFF): OFF = the handlers as today. Proven equal rows
   (the harness oracle) before the flag goes on for tenant zero on prod.
6. Steering: Step 0.1 decides. If aws-cli, rclone and the SDKs follow a
   307 on PutObject/GetObject to another host with a fresh signature, the
   origin answers 307 to the healthiest node of highest `weight` for
   edge-eligible requests behind flag `edge_redirect` (per tenant, default
   OFF); if not, a node is reached by the hostname the customer configures
   and the registry drives health, metrics and the admit's refusal of a
   `down` host only. Either way the control plane never proxies bytes.
7. Health: `vaultaire_data_plane_host_up{host,kind}` from `last_seen`
   (a host unseen 90 s is `down`), rule `DataPlaneHostDown` (warn; page
   when the only `up` host of a tenant-scoped set is down), the System page
   card.

Step 0 — probes into WP-EDGE-3.md: 0.1 the 307 behaviour of aws-cli v2,
rclone, aws-sdk-go-v2 and boto3 on PutObject and GetObject to another
hostname (a lab origin answering 307 Location to a second lab origin);
0.2 the node's throughput on a VPS-class box (the Hostbrr test box in
memory, or the Mac): a 1 GiB codec PUT through the node to Lyve, MB/s and
CPU; 0.3 what a node leaks in its logs (nothing but admit ids and sizes —
grep the node's log for hashes, keys, hostnames of other tenants).

Tests (red first): the lifted package passes the harness unchanged; the
node role refuses every non-data-plane route (a `chi.Walk` matrix); a
node's token from another address is 404 (the edge routes' refusal), the
right address 200; a tenant-scoped node's admit for another tenant is 403
`AccessDenied` in the relayed XML; a tenant-scoped node's stored chunks
are HEADed 100 %; its GET commit meters the admitted span whatever it
reports; `host=slc` rows equal the direct handlers' rows for the same body
(PUT, 8 MiB-part multipart, GET egress); the heartbeat flips `health`; the
307 path (only if 0.1 says yes) re-signs; the admin routes' audit rows.
Lab proof: two processes — the control plane on :8000 with the lab master
key, the node on :8100 with its token — a 100 MiB `aws s3 cp` through the
node → codec rows; a GET through the node; the control plane's log holds
no byte path. Prod: `host=slc` at weight 0 → 1 for tenant zero (the
Lightroom zip again: rows equal, HAProxy sees the body as today — it IS
the origin).

Architecture notes
- A node holds the service channel's token and the request's keys only;
  it never holds the master key or a tenant key version (contract 4); it
  is not the attested data plane (contract 10 — say so).
- BYO (a customer's own VPS as a tenant-scoped node) is the product shape
  from the COGS thread: their bandwidth, our control plane; nothing it
  reports is trusted for integrity or metering.
- Closeout: WP-EDGE-3.md (Step 0, D-EDGE-n, residuals RB4-n), STATUS, plan
  40.10, DEPLOY.md § data nodes, CLAUDE.md rows, the ledger row; refresh
  Prompt 48's two editions here. The post-merge review is RP72.
- Commit: feat(dataplane,api,database): WP-EDGE-3 — … [WP-EDGE-3]; merge
  with `gh pr merge --auto`.

ultrathink

## Stage 1a Prompt 47 — Opus edition

You are building WP-EDGE-3, the data node, in the Vaultaire repository.
The Worker (edge/s3-ingest, 0.3.0) is today's only data-plane host; this
prompt makes the Vaultaire binary itself a second kind of host (a stateless
node on a cheap VPS), re-expresses the origin's own write path as a host
(`host=slc`), and registers hosts in a table. Read the Opus rules in "About
the Opus editions" above. Work in a worktree from `origin/main`. Migration
112 is yours.

Read before editing: plan § 40.10 (docs/IMPLEMENTATION_PLAN.md ~2961–2990);
docs/ARCHITECTURE_CONTRACT.md lines 4, 10, 13; docs/reviews/WP-EDGE-2B.md
and WP-EDGE-2B-REVIEW.md (Part 0 = its findings, in its last section's
order); internal/api/edge_codec_harness_test.go in full (this IS the node's
pipeline, in test form); internal/api/edge_ingest.go (`edgeCallAuthorized`,
`handleEdgeAdmit`, `handleEdgeCommit`, `edgeAdmitPrelude`);
internal/api/edge_get.go; internal/clientip/clientip.go (`PeerIsCloudflare`);
cmd/vaultaire/main.go; internal/api/server.go (`NewServer`, the route
registration order, `s3CredentialGate`); internal/api/admin_flags.go (the
shape of an audited admin route); docs/DEPLOY.md; deploy/*.service.

PR 0 — RP70's fixes, each as its fix column says, red test first. Branch
`stage1a-p47-part0`. Merge before PR A.

PR A — the package, the role, the registry. Branch `stage1a-p47-node`.
1. `internal/dataplane/`: create `controlplane.go` with
   `type ControlPlane interface { Admit(ctx, AdmitRequest) (AdmitResponse,
   error); Index(ctx, IndexRequest) (IndexResponse, error); Commit(ctx,
   CommitRequest) (CommitResponse, error) }` whose request/response types
   are MOVED from internal/api (`edgeAdmitRequest`, `edgeAdmitResponse`,
   `edgeIndexRequest`, `edgeIndexResponse`, `edgeCommitRequest`,
   `edgeCommitChunk`) into this package with exported names; internal/api
   aliases them (`type edgeAdmitRequest = dataplane.AdmitRequest`) so no
   handler changes. Test: `go build ./...` and the whole api package's
   tests pass unchanged.
2. `internal/dataplane/mover.go` + `codec.go`: `func (h *Host) Write(ctx,
   req *http.Request, body io.Reader, size int64) (*Result, error)` and
   `Read(ctx, req) (*http.Response-like, error)`: the exact sequence the
   harness's `uploadCodec` performs (admit; if `Codec`: chunk with
   `crypto.DefaultChunker().Chunk`, sha256, index per 64, seal misses with
   `crypto.SealWrappedChunk`, PUT with the signed headers, commit; else the
   Part A move: PUT the body to the presigned link with the signed
   headers, commit with the backend's answer; a GET: admit, presigned GET,
   stream, commit at the headers). Test first: move the harness's helper
   bodies (`uploadCodec`, `storeMisses`) to call `Host.Write` with an
   in-process `ControlPlane` built over the Server's handlers (an
   `httptest`-free adapter calling `s.edgeAdmit`-style functions — add
   `func (s *Server) DataPlane() dataplane.ControlPlane`). Every harness
   test must pass unchanged — that is the whole proof of step 2.
3. The HTTP `ControlPlane` (`internal/dataplane/httpcp.go`): the three
   calls over HTTPS with `X-Vaultaire-Edge-Token`, a 10 s timeout per call,
   the commit on its own clock (`context.WithoutCancel` + 60 s). Test with
   an `httptest` server replaying the harness's JSON.
4. The node role, test first (`cmd/vaultaire/node_test.go`): with
   `VAULTAIRE_ROLE=node`, `NODE_CONTROL_PLANE_URL=http://127.0.0.1:1`,
   `NODE_TOKEN=x`×32, the process builds a router whose only routes are
   `PUT /{bucket}/{key}`, `GET /{bucket}/{key}`, `/health`, `/metrics`
   (a `chi.Walk` list, asserted exactly); `/api/v1/*`, `/dashboard`,
   `/translog/*` are 404. Then `internal/node/` with `Serve(cfg)`: the
   S3 request's bucket/key/query/headers go into the admit request as the
   Worker builds them (read edge/s3-ingest/src/index.js's `admitRequest`
   for the fields; the client IP is the node's peer), the body streams
   through `Host.Write`, the relayed answer is written back. A heartbeat
   goroutine POSTs `/api/v1/edge/heartbeat` every 30 s. In
   cmd/vaultaire/main.go: `if os.Getenv("VAULTAIRE_ROLE") == "node" { node.
   Main(); return }` before any driver or database is opened.
5. Migration 112 `internal/database/migrations/112_data_plane_hosts.sql`:
   the table as the chain prompt lists it (every column with a DEFAULT or
   NULLable; `host TEXT PRIMARY KEY`; `kind TEXT NOT NULL CHECK (kind IN
   ('edge','node','slc'))`; `health TEXT NOT NULL DEFAULT 'down'`), plus
   `ALTER TABLE edge_admits ADD COLUMN IF NOT EXISTS host TEXT`. Two
   seed rows with `INSERT … ON CONFLICT DO NOTHING`: `('s3.stored.cloud',
   'edge', NULL, NULL, NULL, 1, 'up')` and `('slc', 'slc', NULL, NULL,
   NULL, 0, 'up')`. Run `make test-db`; `TestMigrations_Reapply` and
   `TestSQLLiteralsMatchSchema` green. The table is infrastructure: add it
   to the export's excluded list with the reason and to
   `TestTranslogTablesAreInfrastructure`-style guards where the schema
   walk demands a row (run the api tests to find out which).
6. The registry (`internal/api/data_plane_hosts.go`): `GET/PUT/DELETE
   /api/v1/admin/data-plane-hosts/{host}` behind `requireJWT` +
   `requireAdminMiddleware`, classified as admin WRITES in
   `apiAdminWrites` (read internal/api/translog_read.go's lists; the
   classifier test fails until you add them). PUT body `{kind, address,
   tenant_id, weight, health}`; a `node` kind mints a 32-byte token on
   first PUT, stores its sha256 and returns the token ONCE
   (`token: "<hex>"` in that response only). Audit `data_plane.host_set`
   via `audit.Record`. Tests: the three routes, the token shown once, the
   audit row, 400 on a bad kind.
7. `edgeCallAuthorized` (edge_ingest.go): today = service token +
   Cloudflare peer. New: first try the service-token path as today; else
   look the presented token's sha256 up in `data_plane_hosts` (a 15 s
   cache, keyed by hash) and require the request's trusted peer IP
   (`clientip`'s rule for the direct peer) to equal the row's `address`;
   on success, put the host row in the request context
   (`dataplane.WithHost`). Tests: a node token from the wrong address →
   the edge 404; the right address → 200; a `down` host → 503 with
   `Retry-After: 30` on admit (never on commit — a commit must always be
   accepted).
8. The tenant-scoped host: in `edgeAdmitPrelude`, if the context's host
   has `tenant_id` set and the verified tenant differs → the relayed 403
   `AccessDenied`, counted `vaultaire_data_plane_admits_total{outcome=
   "refused"}`. In `settleEdgeCodecChunks`'s HEAD sample: `every := 32;
   if host.TenantID != "" { every = 1 }`. In `settleEdgeGet`: for a
   tenant-scoped host, `n = row.ContentLength` whatever
   `BackendContentLength` says (a comment citing the COGS rule). Tests for
   all three.
9. `host` on the admit row: `insertEdgeAdmit` writes the context's host
   name (`edge` for the Worker's service token, the row's `host` for a
   node, `slc` for in-process); the metrics already labelled `host`
   (`vaultaire_data_plane_admits_total`, `_bytes_total`) take it from the
   row instead of the constant `edgeHost`. Test: an admit through a node
   token carries its host.
10. `deploy/node/vaultaire-node.service` (a copy of the app unit minus the
    database and credentials, `Environment=VAULTAIRE_ROLE=node`,
    `LoadCredential=node_token:/etc/vaultaire/node_token`) and
    `deploy/node/install.sh` (downloads the release named on the command
    line, `release-sign verify -vkey` with the public key in
    docs/RELEASES.md, installs the binary and the unit, prints the
    heartbeat URL). `shellcheck` clean. PR A title `feat(dataplane,node,
    api,database): WP-EDGE-3 — the data node role, data_plane_hosts (112),
    per-host tokens [WP-EDGE-3]`.

PR B — `host=slc` and steering. Branch `stage1a-p47-slc`, after PR A.
11. Test first: with flag `data_plane_slc` ON for a tenant, a 12 MiB PUT
    through `HandlePut` produces rows identical to the same PUT through a
    node (the harness fixture's comparison helper: head row columns, the
    version row, `tenant_chunk_refs` count/offsets, the bill); the
    `edge_admits` row says `host = 'slc'`; a GET's egress rows equal the
    direct path's. Red. Then in `HandlePut` / `handleUploadPart` /
    `serveGetObject`: when the flag is on and the request is
    edge-eligible (reuse `edgeAdmitPrelude`'s matrix by calling the
    in-process `ControlPlane.Admit` and acting on `edge:false` as
    "the handler as today"), hand the request to `Host.Write` / `Read`
    with the in-process control plane. The flag OFF = no code path
    change (assert with a test that the handlers' row-writing functions
    are not called twice).
12. Step 0.1's answer decides steering. Write the probe first: a lab
    origin that answers `307` with `Location: http://127.0.0.1:8100/…` to
    PutObject and GetObject; aws-cli v2, rclone, aws-sdk-go-v2 (a 20-line
    Go program) and boto3 against it; record for each: followed? re-signed
    for the new Host? Write the table into WP-EDGE-3.md. If ALL FOUR
    follow with a fresh signature: implement `edge_redirect` (flag, per
    tenant, default OFF): in the same three handlers, before the
    `host=slc` branch, if a `node` host of the tenant's scope (or a
    platform node) is `up`, answer 307 to
    `https://<host>/<bucket>/<key>?<query>` and count
    `vaultaire_data_plane_redirects_total{host}`; else write "steering is
    by configuration" as a decision and skip the flag.
13. Health: a collector reading `data_plane_hosts` (15 s cache) exports
    `vaultaire_data_plane_host_up{host,kind}` (1 when `health='up'` and
    `last_seen` within 90 s); the heartbeat handler updates `last_seen`,
    `version`, `health='up'`; a sweep in the `edge_admit_reaper` run marks
    hosts unseen 90 s `down`. Rules in a new
    `deploy/monitoring/vaultaire-dataplane.yml` (`DataPlaneHostDown`
    warn after 5 min; a promtool test in `tests/`); add the file to the
    rule-file test lists (grep for `vaultaire-egress.yml` to find them).
    The System page gets a "Data-plane hosts" card (dashboard/handlers/
    admin_system.go, the capacity card's pattern).

Lab proof (a script `deploy/node/lab-proof.sh`): the control plane on
:8000 (lab master key, EDGE_SERVICE_TOKEN, flags), a node on :8100
registered via the admin route with address 127.0.0.1; `aws s3 cp` of a
100 MiB file to `http://127.0.0.1:8100` → codec rows (compare with the
harness), read back through the node MD5-equal, the control plane's access
log shows no body bytes; the same through `host=slc` with the flag on;
kill the node → `host_up` 0 within 90 s → an admit through its stale token
503. Write the results into WP-EDGE-3.md.

PR C — docs. WP-EDGE-3.md (Step 0 with the 307 table, decisions D-EDGE-n
continuing Part B's numbers, residuals RB4-n, the lab proof), DEPLOY.md
§ Data nodes (install, token mint, the unit, what a node never holds),
STATUS, plan § 40.10 status bullet, CLAUDE.md rows (the env table:
`VAULTAIRE_ROLE`, `NODE_*`; the `data_plane_hosts` table bullet; the flags),
cmd/tools/README untouched. Refresh Prompt 48's two editions here with
current line numbers; mark 47 DONE in the chain; the ledger row.

Prod: PR A/B deploy through the queue with everything OFF; then for
tenant zero only: `data_plane_slc` ON via the admin API (owner's TOTP is
required for an admin JWT — if no owner, set the flag row by psql as the
Part A operator items did and record it), the Lightroom zip through
stored.ge once, the rows compared, the flag left ON. No node is bought
(an owner purchase); the `slc` host row's weight stays 0.

Done means: PR 0, A, B, C merged; the lab proof written; `host=slc`
proven on prod for tenant zero; the ledger row; Prompt 48 refreshed.

## Stage 1a Prompt 48: slice 3 — the second copy at the edge (mirror, RS dark), `standard_sealed_default` (dark), the 10.3 catalogue filter (Part 0: RP72's findings)

*Draft written 2026-10-27 by RP68; Prompt 47's closeout refreshes it. Two
editions; paste one.*

Continue from WP-EDGE-3 (docs/reviews/WP-EDGE-3.md, § Residuals RB4-n) and
its review RP72 (docs/reviews/WP-EDGE-3-REVIEW.md — Part 0; run it first
if it has not). `git log origin/main`, `gh pr list`.

Read first: plan § 40.10 "Slice 3" and "The invariant and the order" (the
trap: sealing Standard by default sends every Standard byte through SLC
unless slice 2 exists — it does now; RP68-1 made every edge write of a
codec tenant codec); plan § 10.3 "The public-catalogue filter"; contract
lines 3 (`modes.json` is the one source — the sentence this flag makes
true), 4, 5 (cross-tenant dedup ONLY in the public-artefact namespace),
8; internal/api/vault_parity_codec.go (the WP-VAULT-1 RS codec: 4+4,
1 MiB per shard per stripe); internal/api/placement.go; internal/api/
flags_wiring.go (`GlobalOnly`, `flagCredentialPlaintextDrop`'s 409 guard
in admin_flags.go — the pattern for a flag refused without a master key);
internal/api/landing/modes.json; internal/modes; docs/BACKEND_ENCRYPTION.md;
tools/edge-ec-worker (the Rust fallback, read only); the RP66 and RP68
reviews' D-EDGE and RB numbering.

Lane: Engine. Migration 113 `blob_copies`. PRs: (0) RP72's fixes; (A) the
mirror at the edge + `blob_copies` + the read fallback (RS dark behind
`edge_rs`); (B) `standard_sealed_default`; (C) the catalogue filter + the
namespace tables (migration 114 if a table is needed — claim it then);
(D) docs.

What to build
1. The second copy, (a) MIRROR for objects under 8 MiB and — until a third
   fast leg exists — for every size on prod: migration 113 `blob_copies`
   (tenant, bucket, storage_key, leg, kind `mirror` | `shard`, `index`,
   `state` `pending` | `ready` | `stale`, `etag`, `size`, `created_at`;
   PK (tenant, storage_key, leg, index)); the write admit (Worker and
   node) of a mirror tenant (flag `edge_mirror`, per tenant, default OFF)
   answers TWO presigned PUTs — the placement's backend and the mirror
   leg from `MIRROR_LEGS` (default `lyve,idrive`, the first registered leg
   that is not the placement's) — the Worker tees the body
   (`ReadableStream.tee()`; the node `io.MultiWriter`-style) into two
   fetches; the commit records both (a mirror leg that failed = the
   object committed on one, a `pending` copy row the new `blob_copies`
   job completes from the primary within the hour, never a failed PUT for
   the client); a codec write mirrors its STORED chunks the same way (two
   links per miss; `global_content_index` gains nothing — the copy row
   names the chunk address); the read admit answers the primary's link
   and, on `fallback` with a 404/5xx from it, the Worker retries the
   mirror's link (a second presigned GET in the admit answer: `urls:[…]`);
   DELETE, the erasure sweep, `retired_blobs`, `routing_truth` and the
   backend capacity job all learn the copies (each a bounded change; the
   schema-walk tests will name them). (b) RS for ≥ 8 MiB: the WP-VAULT-1
   codec compiled to WASM (`sdk/ecwasm`, golden vectors from
   vault_parity_codec_test.go), k+m stripes per 8 MiB, data shards on fast
   legs, parity on slow, no leg holding more than m, a read touching data
   shards only — BUILT and proven in the lab behind `edge_rs` (per tenant,
   default OFF) and left dark on prod (one fast leg).
2. `standard_sealed_default` (global only, default OFF; turning it on
   without a master key or with any tenant lacking a key version is 409
   `master_key_absent` — the `credential_plaintext_drop` guard's shape): ON
   = every Standard-mode write is placed sealed — at the edge the codec
   (every edge write ≥ 4 MiB of every tenant: the flag implies
   `edge_codec` for all), at the origin `placeWholeSealed` / chunk
   encryption for the pass-through cases and sub-4 MiB bodies; a write
   that cannot be sealed (no key version mid-flight) is 503, never
   plaintext. `modes.json`'s Standard sentence becomes true on every
   backend (docs/BACKEND_ENCRYPTION.md's table updated by a test that
   reads the flag's default). Dark on prod; flipped after F-14.
3. The 10.3 catalogue filter: tables `public_artefacts` (id, name, source
   URL, licence, added_by, sha256 of the whole) and `public_chunks`
   (artefact id, chunk index, PLAIN sha256 — public by construction,
   size) seeded in the lab with one artefact set (an Ubuntu cloud image's
   chunks, cut by the frozen chunker; a tool `cmd/tools/catalogue-seed`
   reads a file and writes the rows, operator-run); a daily job
   `catalogue_filter` builds a versioned Bloom filter (1 % FP; a small
   pure-Go implementation with golden vectors, the same code compiled to
   WASM for the Worker and the SDK) and publishes it to R2 at
   `catalogue/<version>.bloom` + `catalogue/latest`; the Worker (and the
   SDK) fetches it once per isolate, checks each chunk's plain hash
   locally and marks matches in the index call (`public: true`); the
   control plane verifies a `public` claim against `public_chunks` (a
   false positive = refused reference, the chunk uploads as usual) and
   references the ONE public copy (`dedup_scope = '_public'`, stored
   plaintext — public content; a public chunk is never sealed). No
   per-chunk round trip is added; the server learns only what was
   referenced.

Step 0 — probes into WP-EDGE-4.md: 0.1 `ReadableStream.tee()` in workerd
with two 64 MiB PUT consumers of unequal speed: memory (the slow branch
buffers) and whether it fits the 128 MB isolate — if not, the mirror is
sequential (PUT then PUT from a buffer of ≤ 16 MiB chunks) and that is
the decision; 0.2 the RS codec's rate in WASM vs native (vault_parity's
bench numbers for native); 0.3 the Bloom filter's size for 10 M chunks at
1 % and its WASM check rate.

Tests (red first): a mirrored PUT lands on both legs with equal ETags and
one commit; a mirror leg down → the object committed, the copy row
`pending`, the job completes it; a read whose primary answers 404 is served
from the mirror and counted; DELETE removes both; the erasure sweep finds
copies; RS: a 64 MiB object's shards rebuild from any k; sealed-default:
with the flag on, a plain `aws s3 cp` of 2 MiB, 12 MiB and 200 MiB into a
plain bucket yields `encryption_algorithm` set on every row and
plaintext on no backend (the fixture's fake backend asserts it);
the flag refused without a master key; the filter: a seeded chunk is
referenced without an upload, a false positive is refused and uploaded,
`_public` chunks never sealed; the modes.json/BACKEND_ENCRYPTION test.
Lab proof: tenant zero's buckets re-uploaded with `edge_mirror` ON
(Lyve + iDrive); the owner's single copy on Lyve ends here — the
`blob_copies` job backfills existing objects at ≤ 50 GB per run.

Architecture notes
- Contract 5: `_public` is the ONLY cross-tenant scope; it holds public
  artefacts the operator seeded; nothing a tenant uploads enters it except
  by matching a seeded hash.
- The two switches that wait for F-14 are `edge_codec` ON (Prompt 46) and
  `standard_sealed_default` ON (this prompt); both are built dark.
- Closeout: WP-EDGE-4.md, STATUS, plan 40.10 + 10.3, CLAUDE.md rows,
  BACKEND_ENCRYPTION.md, the ledger row; write "After the ceremony" as a
  checklist in this file (the chain's last bullet). The post-merge review
  is RP74.
- Commit: feat(api,edge,database): WP-EDGE-4 — … [WP-EDGE-4]; merge with
  `gh pr merge --auto`.

ultrathink

## Stage 1a Prompt 48 — Opus edition

You are building WP-EDGE-4 (slice 3 of plan § 40.10) in the Vaultaire
repository: the second copy at the edge, the dark seal-by-default flag,
and the public-catalogue filter. Read the Opus rules above. Worktree from
`origin/main`. Migrations 113 (and 114 only for the catalogue tables).

Read before editing: plan § 40.10 "Slice 3", "The invariant and the
order"; plan § 10.3; docs/ARCHITECTURE_CONTRACT.md lines 3, 4, 5, 8;
docs/reviews/WP-EDGE-3.md + WP-EDGE-3-REVIEW.md; internal/api/
vault_parity_codec.go + its test; internal/api/vault_parity.go (how legs
are chosen: `VAULT_PARITY_LEGS`); internal/api/placement.go;
internal/api/blob_address.go (`blobNamedBy` — every "is this blob still
named" check); internal/api/retired_blobs.go; internal/api/
erasure_sweep.go; internal/api/routing_truth.go; internal/api/
admin_flags.go (`flagCredentialPlaintextDrop`'s 409 guard); internal/api/
landing/modes.json + internal/modes; docs/BACKEND_ENCRYPTION.md;
internal/dataplane (Prompt 47's package); edge/s3-ingest/src/codec.js.

PR 0 — RP72's fixes. Branch `stage1a-p48-part0`.

PR A — the mirror. Branch `stage1a-p48-mirror`.
1. Migration 113 `113_blob_copies.sql`: the table as the chain prompt
   lists it, every column defaulted; index on `(state, created_at)` for
   the job; `make test-db`, the reapply test. Add it to the erasure walk
   (`internal/account/erase.go`: tenant data, erased with the account),
   the export's excluded list ("copies of the customer's own objects"),
   and the sweep's bucket sources if the schema walk asks.
2. `MIRROR_LEGS` env (default `lyve,idrive`; validated like
   `VAULT_PARITY_LEGS`; Warn-and-default) and flag `edge_mirror` (per
   tenant, default OFF) in flags_wiring.go. `mirrorLegFor(placementBackend)
   string` = the first registered `EdgeWriter` in the list that is not
   the placement's backend and whose breaker is closed, else "" (no
   mirror this write — never a refusal).
3. Test first (harness): with `edge_mirror` ON, a 12 MiB codec PUT's index
   answer carries TWO urls per miss (`urls: [primary, mirror]`, `url`
   kept = urls[0] for the 0.3.0 Worker), and the commit with two ETags
   per stored chunk (`backend_etags: [a, b]`) writes one `blob_copies`
   row per chunk with `state='ready'`; a Part A whole PUT's admit carries
   two presigned PUTs the same way and the commit writes one copy row.
   Red. Then the control plane: `edgeIndexMiss.URLs`, `edgeCommitChunk.
   BackendETags`, `edgeAdmitResponse.URLs`; `settleEdgeCodecChunks` and
   `commitEdgePut` write the copy rows in the commit's transaction
   (`blobWrite.Upsert`). A commit reporting only one ETag (the mirror PUT
   failed) writes the row `pending`. Green.
4. The Worker: in codec.js and index.js's `moveBody`, when `urls.length ==
   2`: `const [a, b] = body.tee()` and two fetches awaited together
   (`Promise.allSettled`); a rejected mirror → its ETag omitted, the
   commit says so. Node test: both PUTs receive identical bytes; a failing
   mirror still commits. Step 0.1 first: if `tee()` with a slow branch
   exceeds 100 MB in `wrangler dev` on a 1 GiB body, use the sequential
   form (PUT primary; then re-PUT the mirror from the chunk buffer per
   chunk — codec writes hold the chunk anyway; a whole-PUT mirror then
   becomes the `blob_copies` job's, not the Worker's). Write the decision
   as D-EDGE-n.
5. The node (`internal/dataplane`): the same with `io.Pipe` ×2.
6. The `blob_copies` job (interval 10 min, `BootDelay` 6 min): `pending`
   rows older than 5 min → the primary's blob streamed to the mirror leg
   through the engine (never SLC… it IS SLC: say in the note that the
   repair copy is the 2× lane, rare by construction); `stale` rows
   (written by the DELETE aftermath and `commitBlobWrite`'s displaced-blob
   path) → deleted on the leg, then the row. `vaultaire_blob_copies_total
   {outcome}`, a gauge of pending rows from the table.
7. The read: the GET admit answers `urls: [primary, mirror]` when a
   `ready` copy exists; the Worker's `serveRead`, on a backend answer that
   is not `expect_status` from urls[0], tries urls[1] before the origin
   fallback; the commit's `backend` names which served. `routing_truth`
   samples copies like head rows (`kind="copy"` results). Tests for
   each.
8. Lab proof script `deploy/edge/mirror-proof.sh` + a backfill run on
   tenant zero's buckets in the LAB only. PR A title `feat(api,edge,
   database): WP-EDGE-4 Part A — the mirror at the edge, blob_copies
   (113) [WP-EDGE-4]`.

PR B — RS dark. Branch `stage1a-p48-rs`.
9. `sdk/ecwasm`: `internal/api/vault_parity_codec.go`'s encoder/decoder
   compiled to WASM (`make ec-wasm`, client `edge-ec`, a manifest row),
   golden vectors from the native test asserted in node (`test/ec.test.js`).
   Flag `edge_rs` (per tenant, default OFF): the admit of a ≥ 8 MiB write
   answers k+m presigned PUTs (`shards: [{leg, url}]` — data legs first)
   when ≥ k+m registered EdgeWriters exist with no leg over m, else the
   mirror; the Worker stripes per 8 MiB through the WASM codec and PUTs
   each shard; the commit writes `kind='shard'` rows; the read admit
   answers the k data shards' links and the Worker concatenates. Lab only
   (two fake fast legs in the fixture); prod: the flag off, one fast leg.
   Tests: any k of k+m rebuild; a stripe boundary mid-object.

PR C — `standard_sealed_default`. Branch `stage1a-p48-sealed`.
10. Test first: `PUT /api/v1/admin/flags/standard_sealed_default` with the
    master key absent → 409 `master_key_absent`; with it present and a
    tenant without a key version → 409 `tenant_keys_missing` naming the
    count; a tenant row → 400 `flag_global_only`. Then: register the flag
    `GlobalOnly`; the guard in admin_flags.go beside the plaintext-drop
    guard; the dashboard's flags page reuses the guard (it calls the same
    service). Then `decidePlacement`: when the flag is on and the request
    is Standard mode (not SSE-C, not Public), `encryptionRequired = true`
    (the existing variable) — so sub-threshold bodies become
    `placeWholeSealed` and chunked ones chunk-encrypt; the edge admit:
    the flag ON implies `edge_codec` for every tenant (one `||` in the
    gate). Tests: 2 MiB / 12 MiB / 200 MiB plain PUTs into a plain bucket
    with the flag on → every row sealed, the fake backend saw no plaintext
    (compare the first 4 bytes: a 0x04/0x06 format byte, or AES256-CE
    ciphertext) ; a write with no key version mid-flight → 503, nothing
    stored. `docs/BACKEND_ENCRYPTION.md`'s table: add a `TestBackendEncryption
    DocMatchesFlagDefault` that reads the flag's default and the doc's
    "sealed by default: yes/no" cell.

PR D — the catalogue filter. Branch `stage1a-p48-catalogue`.
11. Migration 114 `public_artefacts` + `public_chunks` (infrastructure: no
    tenant column; excluded from the export and the erasure walk with
    reasons). `cmd/tools/catalogue-seed -file <path> -name <n> -licence
    <l> -source <url>`: cuts with `crypto.DefaultChunker`, inserts rows;
    README row; operator-run, never linked into the product.
12. `internal/catalogue/bloom.go`: a double-hashing Bloom filter (FNV-1a
    64 split, k from the FP rate) with a versioned byte format
    `[0x01][version u64][m u64][k u8][bits]` and golden vectors in
    `testdata/bloom_golden.json`; the same package compiled in
    `sdk/wasm` (export `catalogueContains(filter, hash)`) and asserted
    in node against the goldens.
13. The job `catalogue_filter` (daily 09:30 UTC): builds the filter from
    `public_chunks` and writes `catalogue/<version>.bloom` and
    `catalogue/latest` (the version number) to the public R2 bucket
    through the `r2` driver (tenant `_catalogue`, a reserved id like
    `_probe`); `vaultaire_catalogue_filter_version`, `_chunks`.
14. The Worker: fetch `catalogue/latest` then the filter once per isolate
    (cached in a module variable; re-fetched when older than 1 h); in
    codec.js, a chunk whose plain hash the filter contains is sent with
    `public: true`; the control plane's `edgeIndex`: a `public` chunk is
    looked up in `public_chunks` by plain hash — found → a hit on the
    `_public` scope row (`global_content_index` row with
    `dedup_scope='_public'`, plaintext, created by the seed tool with the
    chunk's blob stored at `_chunks/_public/<hash>` by the seed tool
    through the chunk store) — not found → a miss as usual (the false
    positive). The manifest ref carries `dedup_scope='_public'`; the
    chunked GET already resolves scope per ref. Tests: a seeded chunk
    referenced with 0 PUTs; a false positive uploads; `_public` rows never
    sealed; a tenant cannot INSERT into `_public` (the index call never
    claims there).

PR E — docs: WP-EDGE-4.md (Step 0, D-EDGE-n, RB5-n, the lab proofs),
STATUS, plan § 40.10 + § 10.3 status lines, CLAUDE.md rows (env
`MIRROR_LEGS`; tables `blob_copies`, `public_artefacts`, `public_chunks`;
flags `edge_mirror`, `edge_rs`, `standard_sealed_default`; the job rows),
BACKEND_ENCRYPTION.md, the ledger row; and in this file the "After the
ceremony" checklist: (1) F-14 ceremony (owner); (2) `tenant_key_versions`
exist for every tenant (the `key_reencrypt` run creates on first use —
verify); (3) `edge_codec` ON for tenant zero, the sealed proofs (a PUT
through s3.stored.cloud → AES256-EW rows, a GET MD5-equal); (4)
`standard_sealed_default` ON; (5) `credential_plaintext_drop` ON (Prompt
43 B3's order); (6) the key-dependent residuals of Prompts 46/48.

Prod: everything deploys dark; `edge_mirror` ON for tenant zero only
after the lab proof; the `blob_copies` backfill of tenant zero's buckets
is the first production use (the owner's Lightroom backups get their
second copy on iDrive) — run it, verify copy rows = head rows for those
buckets, write the count into the note.

Done means: PR 0, A, B, C, D, E merged; the lab proofs written; tenant
zero mirrored on prod; the ceremony checklist written here.

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
- **Prompt 46b — WP-EDGE-2 Part B** (ISSUED 2026-10-27, REFRESHED the same day by the RP68 review (`docs/reviews/WP-EDGE-2A-REVIEW.md`): Part 0 = RP68-1…6 in the review's order, and RP68-1 changes what the pipeline admits — every edge write ≥ 4 MiB and every multipart part of a codec tenant, not only the origin's chunked placements; the full text is the `## Stage 1a Prompt 46b` section above in TWO editions, the chain's and an Opus edition; summary): the Worker 0.3.0 pipeline (the WASM chunker, sha256, the index call per 64 chunks, WebCrypto AES-GCM segments + the HPKE wrap from sdk/wasm, a PUT per miss, the commit; back-pressure; the per-stream rate measured — the baseline says JavaScript in the byte path halves a stream), the lab proof through wrangler dev against the harness's rows (default 8 MiB parts, parallel parts sharing chunks, a tampered host), `cmd/tools/dedup-measure` (F-3: the cross-tenant fraction of a lab corpus, the number the owner decides F-37 with), `encodeBody: "manual"` probed for RP66-1's residual, the three plan § 40.5 sentences (RP68 C-3/4/5). On prod the two switches that wait for F-14 stay: `edge_codec` ON and Prompt 48's `standard_sealed_default`. The post-merge review is RP70.
- **Prompt 47 — WP-EDGE-3, the data node** (DRAFTED 2026-10-27 by RP68 in two editions above — `## Stage 1a Prompt 47` and its Opus edition; Prompt 46b's closeout refreshes the line numbers; summary): `internal/dataplane` = the harness's pipeline as a package with an HTTP and an in-process control plane; `VAULTAIRE_ROLE=node` boots the binary with no DB, no engine, the data-plane routes only, a heartbeat, `deploy/node/`; migration 112 `data_plane_hosts` (host, kind edge|node|slc, address, token_hash, tenant_id — a tenant-scoped BYO host admits one tenant and nothing it reports is trusted for integrity (RP68-3 at 100 %) or metering (its GET metered at the admitted span) —, weight, health, last_seen) with audited admin routes and per-host tokens accepted from the host's registered address; `host=slc` = the origin's own edge-eligible writes and reads through the same package in-process behind `data_plane_slc` (rows proven equal); steering by 307 behind `edge_redirect` only if Step 0.1 shows aws-cli, rclone and the SDKs re-sign on a 307, else by configuration; `vaultaire_data_plane_host_up`, `DataPlaneHostDown`. Proven with two processes in the lab and on prod as `host=slc` for tenant zero. A real node is an owner purchase. The post-merge review is RP72.
- **Prompt 48 — slice 3 + the dark seal-by-default flag + the catalogue filter** (DRAFTED 2026-10-27 by RP68 in two editions above — `## Stage 1a Prompt 48` and its Opus edition; Prompt 47's closeout refreshes it; summary): (a) the second copy at the edge — migration 113 `blob_copies`; `edge_mirror` (per tenant): the admit answers two presigned PUTs (the placement's backend + the first registered leg of `MIRROR_LEGS` that is not it), the Worker tees (or PUTs sequentially — Step 0.1 decides on the isolate's memory), the commit records both, a failed mirror leg becomes a `pending` row the `blob_copies` job completes, reads fall back to the mirror before the origin, DELETE / the sweep / `retired_blobs` / `routing_truth` / capacity learn the copies; RS k+m with the WP-VAULT-1 codec in WASM (`sdk/ecwasm`) built and proven in the lab behind `edge_rs`, dark on prod (one fast leg); mirror first on prod — tenant zero's backups get their second copy on iDrive. (b) `standard_sealed_default` (global, default OFF, 409 without a master key or a tenant without a key version): ON = every Standard write placed sealed — the codec at the edge for every tenant, `placeWholeSealed` / chunk encryption at the origin — the switch that makes `modes.json`'s sentence true; built dark, flipped after F-14. (c) 10.3's catalogue filter: `public_artefacts` + `public_chunks` (migration 114) seeded in the lab by `cmd/tools/catalogue-seed`, a daily `catalogue_filter` job publishing a versioned Bloom filter to R2, the Worker and the SDK checking locally and marking `public: true`, the control plane referencing the one `_public` copy — the only cross-tenant scope (contract 5). The post-merge review is RP74.
- **After the ceremony (one session, owner present only for the ceremony
  itself):** `edge_codec` ON for tenant zero → the sealed-object proofs →
  `standard_sealed_default` ON → `credential_plaintext_drop` (Prompt 43 B3) →
  the key-dependent residuals of Prompts 46 and 48.
