# Changelog

What's new on Stored. Ship requests and bug reports are credited — tell us
what you need and it shows up here.

---

## 2026-10-19: Multipart objects are checked whole; a wrong SSE-C key is refused up front

- **A multipart upload's object is now verified on download like a single
  upload's.** Its ETag is `<md5-of-part-md5s>-N`, which says nothing about
  the whole body, so until now only the size was checked. The whole body's
  MD5 is recorded when the upload completes and every whole download is
  checked against it as it streams; a mismatch is cut before the last byte
  (your client retries). Objects completed before today keep the
  size-only check.
- **A wrong SSE-C key answers `403` on `HEAD` as well as `GET`**, from the
  object's own record (the key's MD5, as AWS keeps it — never the key),
  before any byte is read. `HEAD` used to answer 200 for a wrong key.
  Objects written before today keep the old answers.

## 2026-10-19: HEAD and ranged downloads tell the same story

- **HEAD and a ranged GET now carry every header a whole GET does**: the
  ETag, Last-Modified, the storage class, your `x-amz-meta-*` metadata,
  Cache-Control, Content-Disposition and the rest are the same on all three,
  so `aws s3 sync`, rclone and the SDKs see one object, not three.
  `HEAD` honours `Range` (206 / 416), as on AWS.
- **SSE-C requests without the key answer `400 InvalidRequest`**, as on AWS
  (it was 403), and say nothing about the object. A `GET` or `HEAD` with
  `?versionId=` of a delete marker answers `405 MethodNotAllowed` with
  `x-amz-delete-marker: true`, as on AWS (it was 404).
- Objects stored encrypted in chunks now say so on the upload's answer
  (`x-amz-server-side-encryption: AES256`), as HEAD always did.
- A bucket that was never versioned no longer answers
  `x-amz-version-id: null` on downloads.

## 2026-10-19: Downloads are checked against the object's checksum

- **A whole-object download is checked as it streams.** The bytes of a
  `GET` of a whole object (and of a public `/cdn/` link) are hashed on the
  way out and compared with the object's ETag — the MD5 of what you
  uploaded. If a storage backend ever hands back other bytes (a corrupted,
  truncated or misplaced copy), the download is **cut before its last
  byte**: your client sees a connection that ended early (aws-cli, rclone
  and the SDKs report it and retry) instead of a file that looks complete
  and is wrong. Every byte but the last has been sent by then — HTTP has no
  place for a checksum after the body that S3 clients read, so the cut is
  the signal. We are alerted on every one.
- What this check cannot cover: a ranged download (`Range:`) and an object
  uploaded in parts (an ETag ending in `-N`) are checked for their size
  only, as before. Large deduplicated objects already verify every piece
  they are made of.

## 2026-10-18: Server-side copies of large objects

- **`aws s3 cp` and `aws s3 mv` between keys work for large objects.**
  aws-cli, rclone and Cyberduck copy anything above their multipart
  threshold (8 MB for aws-cli) on the server, one part at a time, with
  `UploadPartCopy`. It answered `501 NotImplemented`, so a server-side copy
  or move of anything but a small file failed. It now copies the part from
  the source object: a byte range (`x-amz-copy-source-range`) or the whole
  object, from the current object or a version (`?versionId=`), encrypted
  or not, with the `x-amz-copy-source-if-*` conditions. Nothing is
  downloaded to your machine, and a server-side copy counts no download
  traffic.
- A part is up to 5 GiB, as on AWS. A copied part counts against your
  storage while the upload is in progress, as an uploaded part does.
  Objects encrypted with your own key (SSE-C) still cannot be copied
  server-side.

## 2026-10-13: Encryption at rest has no size limit

- **Any object can be encrypted at rest.** Server-side encryption
  (`x-amz-server-side-encryption: AES256`, or a bucket that encrypts by
  default) now seals an object as it streams, one megabyte at a time, so
  there is no size limit any more: uploads over 256 MiB into Vault, Object
  Lock, versioned or region-pinned buckets are encrypted instead of refused
  with `413 EntityTooLarge`.
- **Ranged downloads of encrypted objects read only what they need.** A
  `Range` request on an encrypted object fetches and decrypts just the
  megabytes the range covers.
- **Encrypted objects can be copied.** `CopyObject` (and `aws s3 mv`,
  rclone moves) of an encrypted object used to answer `501 NotImplemented`;
  it now copies it, encrypted again in its destination. Objects encrypted
  with your own key (SSE-C) still cannot be copied server-side.
- **Your own key (SSE-C) works at any size too.** Objects encrypted with a
  customer-provided key are sealed as they stream, so the 256 MiB limit is
  gone there as well, and ranged downloads read only what they need.
  Objects you uploaded with SSE-C before still open with the same key.
- **`x-amz-server-side-encryption` on multipart uploads.** The header sent
  on `CreateMultipartUpload` is honoured at `CompleteMultipartUpload`, and
  both answer it when the object is encrypted. A request that asks for
  encryption while the server cannot encrypt is refused with a retryable
  `503`, never stored unencrypted.

## 2026-10-13: Large uploads are deduplicated, compressed and encrypted

- **Multipart uploads are stored like single uploads.** Every upload over
  8 MiB from aws-cli, rclone or restic is a multipart upload; until today
  those objects were kept as one whole, uncompressed blob. They now go
  through the same pipeline as a single upload of the same bytes: over
  64 MiB they are split into content-defined chunks, deduplicated and
  compressed. The ETag stays the `<md5>-<parts>` aws-cli computes.
- **Encrypted buckets encrypt multipart uploads.** A multipart upload into a
  bucket that encrypts at rest is encrypted (chunk by chunk, or sealed
  whole). One that cannot be encrypted is refused instead of being stored
  as sent (the 256 MiB limit this entry named is gone, see above).
- **Every part is checked again before it is stored.** If a staged part no
  longer matches what you uploaded, CompleteMultipartUpload fails and nothing
  is written; upload the part again.
- **Public links serve large objects.** A public bucket's `/cdn` link now
  serves objects over 64 MiB, Range requests included.

## 2026-10-13 — Versioning: the null version and batch deletes

- **Objects uploaded before you enabled versioning are kept.** Such an object
  is the key's `null` version (as `list-object-versions` already showed):
  overwriting or deleting it now keeps it, `get-object --version-id null`
  downloads it, and `DELETE ?versionId=null` removes it for good. Until today
  the first overwrite or delete on a newly versioned bucket discarded it.
- **Batch deletes respect versioning.** `DeleteObjects` (what `aws s3 rm
  --recursive` uses) writes a delete marker per key on a versioned bucket, and
  an entry with a `VersionId` deletes exactly that version; the response
  carries `DeleteMarker` / `DeleteMarkerVersionId` / `VersionId` as on AWS.
  Before, it removed the current object without a marker, which left its
  version stored, counted and unreachable.

## 2026-10-12 — Versions keep their bytes

- **Previous versions are downloadable.** On a bucket with versioning
  enabled, every upload now keeps its own copy: `aws s3api get-object
  --version-id <id>` (or `head-object`) returns that version's bytes and
  metadata, and deleting the current version brings the previous one back as
  the current object. Overwrites no longer replace bytes in place, so an
  upload that fails half-way can never damage the object you already had.
- **Older versions count toward your storage.** A version you keep is data
  we keep: it counts toward your quota (on the floor it was written to) until
  you delete it with `DELETE ?versionId=<id>`. The dashboard's bucket list
  shows how much of each bucket is older versions.
- Versions written **before 2026-10-12** were metadata only — their bytes
  were overwritten by the next upload — so fetching one of those still
  answers `501 Not Implemented` rather than the wrong bytes.

## 2026-10-06 — CORS on the S3 API

- **Browser apps can talk to the S3 API directly.** `PUT /<bucket>?cors` takes
  the standard AWS `CORSConfiguration` (the same document you would give S3,
  rclone's `backend` commands or the SDKs' `PutBucketCors`), `GET` returns it,
  `DELETE` removes it. Preflights are answered before authentication, and the
  real response carries the CORS headers on every status — so a page can read
  a 403 instead of a network error. Nothing changes for buckets without a
  configuration: cross-origin browser requests to them are refused as before.
  List `ETag` under `ExposeHeader` to do browser-side multipart uploads. The
  public `/cdn` path's CORS setting is separate and unchanged.

## 2026-09-22 — Quota pricing: pick your TB, flat rate, every tier

- **Every tier is now sold as a quota.** You choose a size in whole TB (any
  size), pay a flat per-TB rate, and resize up or down whenever you like
  (prorated). The quota is a hard cap — writes past it return a clear quota
  error until you resize — so your bill never moves on its own. No meters,
  no overage charges. Vault already worked this way; now Standard does too.
- **Standard is $4.49/TB/mo annual prepaid, $4.99 monthly.** About 15% of
  your quota stays on hot storage; data idle for 14+ days moves to tape and
  comes back hot within minutes when read. New **pin-hot add-on** ($3/TB/mo)
  for data that must never demote.
- **Egress allowances are keyed to your quota**: Standard egress is free up
  to 0.5× your quota per month, then throttled rather than billed. Vault
  restores are free up to 1× your quota per month; beyond that they queue,
  never billed. (The old "3× stored" and "$2.99/TB" overage lines are gone.)
- **Performance tier is parked** until after launch — it returns at $6.99/TB/mo.
  Until then, pin-hot on Standard covers always-hot needs.

## 2026-09-16 — Simpler Vault pricing: $2/TB, any size

- **Vault is now linear.** The prepaid pack ladder (Vault1–Vault18) is gone;
  the tape-backed archive tier is simply **$2.00/TB/mo annual prepaid** or
  $2.55/TB/mo monthly, at whatever allocation size you want. 5 TB of tape
  archive is $10/mo billed yearly. Packs punished small archives and made
  you buy sizes you didn't need — linear pricing fixes both.
- **The limits, stated plainly** (they're what make the price honest): one
  durable tape copy — we're the offsite leg of *your* 3-2-1; 30-day minimum
  per object; free restores up to 1× your stored bytes each month (run a
  restore drill monthly, free), then $2.99/TB; restored copies stay hot for
  7 days. Never retrieval fees, never egress fees.
- **Founders slots.** The first 100 TB sold go at $1/TB/mo, annual prepaid,
  3–10 TB per account, lifetime rate, numbered slots. Hard-capped — when
  they're gone, they're gone.
- **Launch date is October 31, 2026.**

## 2026-07-20 — Customer docs are live

- **Real documentation at [/docs](/docs).** Getting Started (signup to first
  upload in two minutes), a full rclone guide, and an honest FAQ covering
  pricing, trust, and the technical details. The interactive API reference
  moved to /docs/api and now carries the right name and support address.

## 2026-07-20 — New homepage

- **The real Stored front page is live.** Tier ladder (Vault archive →
  Standard → Performance), honest measured numbers, full pricing with the
  Vault packs, and a straight-answers section covering the questions most
  storage providers dodge. The old pre-launch placeholder page is gone.
- **The footer now links everything.** Terms, privacy, acceptable use, DPA,
  GDPR, BAA, EU Data Act, the status page, this changelog, and abuse
  reporting — all reachable from the homepage at last.

## 2026-07-20 — Live-iteration kit

- **Runtime feature flags.** New capabilities now roll out flag-dark and get
  enabled per account, so early adopters can opt in before a feature is on
  for everyone. Kill-switches let us turn a misbehaving subsystem off in
  seconds — no redeploy, no downtime.
- **This changelog.** Public, updated with every deploy. Entries credit the
  person who asked for the change.

## 2026-07-20 — Storage engine hardening

- **Failed backend writes now fail loudly.** If a storage backend rejects a
  write, the upload returns an error your client retries — data is never
  silently parked somewhere it doesn't belong.
- **Abandoned multipart uploads are reaped.** Uploads idle for 48 hours are
  aborted automatically and their parts cleaned up, and a single upload's
  in-flight parts are capped at 50 GiB.

## 2026-07-19 — Deduplication + garbage collection

- **Dedup GC coherence.** The nightly garbage collector, the dedup cache, and
  concurrent uploads can no longer race each other — shared chunks are
  reference-counted with database-level locking end to end.
- **Deploy auto-rollback.** A deploy that fails its health check now rolls
  back to the previous binary automatically.
