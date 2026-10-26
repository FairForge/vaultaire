# WP-EDGE — the 0×-SLC ingest plane: worker prompts

*Plan section 40.10 (`docs/IMPLEMENTATION_PLAN.md`, `grep -n "### 40.10"`). Written 2026-10-26 by the owner session that set 40.10's order ("do it now"). Each prompt is copy-pasted into a fresh Claude Code session; it carries everything that session needs. Line numbers below are from main at `e9cb4241` (2026-10-26) — re-grep before editing, they drift.*

---

## Stage 1a Prompt 44: WP-EDGE-1 Part A — the mover (PutObject + UploadPart through a Worker on `s3.stored.cloud`)

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
    does not exist yet (CLOUDFLARE.md § DNS (stored.cloud) — target state). This
    is [YOU] item 1; you cannot create it. Build and prove on a lab origin.
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
- [YOU] (owner, in order): (1) DNS A/AAAA `s3.stored.cloud` → the SLC address,
  proxied, on the stored.cloud zone, and an origin certificate HAProxy serves
  that covers s3.stored.cloud (a Cloudflare Origin CA certificate is the
  simplest; or add the SAN to the LE cert — docs/DEPLOY.md's cert section);
  (2) `wrangler secret put VAULTAIRE_EDGE_TOKEN` on the production account and
  the same value as EDGE_SERVICE_TOKEN on the box (generate with `openssl rand
  -hex 32`, keep it in the Keychain as `vaultaire-edge-token`, never in the
  chat); (3) after the deploy: flag `edge_ingest` ON for tenant zero from
  /admin/flags, then the Lightroom proof above; (4) read the sub-processors
  sentence before A3 merges.
- Commit: feat(api,drivers,edge): WP-EDGE-1 Part A — the mover … [WP-EDGE-1];
  no AI attribution lines in commits; merge with `gh pr merge --auto` (merge
  queue; no --squash, no --delete-branch, never --admin).
```

---

## Next prompts in this series (not yet written)

- **WP-EDGE-1 Part B — reads:** GetObject through the Worker: admit → the
  egress meter counts and the `egress_throttle` decision is answered as 503
  SlowDown + Retry-After from admit (the Worker never paces) → a presigned ranged
  GET (`Range` passed through) → stream; HEAD stays at the origin (cache-only,
  Decision 2); the `parallel_get` flag's whole-object path at the edge later.
- **WP-EDGE-2 — slice 2, the codec at the edge** (after the F-14 ceremony; built
  against a lab master key first): the WASM chunker pinned to the golden
  boundaries, the random per-chunk key wrapped to the tenant's ML-KEM-768 public
  key (a new header version of the 0x03 format), the batched index call, F-3's
  measurement as its first run.
- **WP-EDGE-3 — the first data node:** this binary stateless as a data-plane
  host, `data_plane_hosts` (host, weight, health), the control plane picking a
  host per admit.
