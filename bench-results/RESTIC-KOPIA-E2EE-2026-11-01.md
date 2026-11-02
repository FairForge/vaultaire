# restic and Kopia against prod, 2026-11-01 — init, backup, check, restore, prune

**Why:** the landing page says restic-style client-side encryption means we "can't read a
byte of it" (`internal/api/landing/landing.src.html:282`), and nothing in the repo recorded
an actual restic or Kopia run against the service. A LowEndTalk poster (thread 222800,
2026-11-01) asks for S3 storage for kopia/restic backups of 10–70 GB. This is the one run
that makes the compatibility claim tested. Both clients passed every step.

**Path:** from the owner's Mac (`Ike-2`, arm64, residential uplink) through the public
endpoint `https://stored.ge` — Cloudflare → HAProxy → the active slot (build
2026-11-01T05:30:10Z, `STORAGE_MODE=idrive`). Tenant `Bench` (`tenant-14a623b16b3f7012`),
the bench key, region `us-east-1` in the signature. One fresh bucket per client, created
with aws-cli, removed at the end. Nothing of this run is left on prod (0 head rows, no
bucket row).

**Clients:** restic 0.18.1 (go1.25.1; S3 through minio-go v7.0.95), Kopia 0.23.1
(minio-go v7.0.98), aws-cli 2.28.19 for the bucket and the server-side listing.

**Dataset:** 3,043 files, 713.7 MiB — 3,000 "documents" of 1–50 KB (compressible text),
40 "photos" of 2–16 MiB and 3 "videos" of 100 MiB (random bytes). Every file starts with
the marker `STORED-E2EE-PLAINTEXT-MARKER-7f3a9c`, so a grep of the server-side objects
answers whether any plaintext landed. SHA-256 of every file taken before the first backup.

## 1. restic

| Step | Wall | Result |
|---|---|---|
| `restic init` | 4.1 s | repository created |
| `restic backup` (3,043 files, 713.7 MiB) | 25.3 s | 3,478 data blobs, 28 tree blobs, 714.2 MiB stored, snapshot `f72af844` |
| `restic check` | 3.1 s | no errors |
| `restic check --read-data` (every pack read back) | 12.3 s | 44 packs, no errors |
| `restic restore latest` | 13.2 s | 3,070 files/dirs, 713.7 MiB |
| `shasum -c` of the restore against the pre-backup list | 3.0 s | **ALL_HASHES_OK** (3,043 of 3,043) |
| incremental backup (1 file changed, 1 added, 1 removed) | 3.1 s | 108 KiB added (22.5 KiB stored), snapshot `59295005` |
| `restic forget --keep-last 1 --prune` | 3.9 s | 1 snapshot removed, 1 pack repacked, 1 pack deleted |
| `restic check` after the prune | 2.9 s | no errors |

Throughput at the Mac's link: backup ≈ 30 MB/s, read-back ≈ 61 MB/s (83 MB/s inside the
9 s read phase), restore ≈ 57 MB/s. These are the uplink's numbers, not the service's.

**Server side after the second backup** (`aws s3 ls --recursive`): 52 objects,
749,051,627 bytes (0.13 % over the 714.2 MiB restic says it stored); keys `config` (1),
`data/<aa>/<sha256>` (46, ~17 MB each), `index/` (2), `keys/` (1), `snapshots/` (2).
Every object downloaded and grepped: **0 of 52 contain the marker, 0 of 52 contain a
filename** (`doc-0001.pdf`). The `config` object is 155 bytes of ciphertext.

**What prod recorded:** one `HeadObject` 404 `NoSuchKey` (init's "does a repository
exist" probe; minio-go user agent). Successful operations of a bucket without
`logging_enabled` are not written to `s3_access_log` by design (R13-14), so the 200s are
in the request metrics only.

## 2. Kopia

Repository created with Kopia's defaults: `AES256-GCM-HMAC-SHA256`, hash
`BLAKE2B-256-128`, key derivation `scrypt-65536-8-1`, splitter `DYNAMIC-4M-BUZHASH`,
compression off, max pack 21 MB. `--endpoint=stored.ge --region=us-east-1`, TLS on.

| Step | Wall | Result |
|---|---|---|
| `kopia repository create s3` | 4.8 s | connected |
| `kopia snapshot create` (3,043 files, 748.3 MB decimal) | 28.0 s | snapshot in 23 s + Kopia's automatic full maintenance (GC: 3,205 in-use contents) |
| `kopia snapshot verify --verify-files-percent=100` | 52.4 s | 3,062 objects, 3,042 files read, 748.3 MB, no errors |
| `kopia restore <snapshot>` | 2.1 s | 3,043 files, 20 dirs — **served from the local content cache the verify had just filled**, not a network figure |
| restore compared with the source by `diff -rq` | — | **3,043 files identical**; the only differences are the three edits made to the source AFTER this snapshot (one changed, one added, one removed — see the note below) |
| incremental snapshot | 2.4 s | second snapshot |
| `kopia snapshot delete --delete` + `kopia maintenance run --full --safety=none` | 2.0 s + 8.6 s | 5 unused contents collected, 3,204 kept |
| `kopia snapshot verify --verify-files-percent=100` after | 1.3 s | 3,061 objects, no errors (cache-warm) |

Note on the comparison: the run script first checked the Kopia restore with the SHA list
taken before the restic backup; two entries failed (`doc-0001.pdf`, `doc-0003.pdf`)
because the restic incremental step had modified the source between the two runs, so the
list was stale for Kopia, not the restore. The `diff -rq` of the restore against the live
source is the check that counts: every file Kopia snapshotted came back bit-identical.

Kopia's full-content verify is ~4× slower than restic's `--read-data` at this uplink
(14 vs 61 MB/s): Kopia verifies content by content with ranged GETs inside 21 MB packs
(3,205 contents), restic reads 44 whole packs.

**Server side after the second snapshot:** 46 objects, 748,938,012 bytes; 34 `p…` data
packs of 21–27 MB, 3 `q…` metadata packs, 3 `xn0_…` indexes, 3 `_log_…` session logs,
`kopia.repository`, `kopia.blobcfg`, `kopia.maintenance`. Every object downloaded and
grepped: **0 of 46 contain the marker, 0 of 46 contain a filename.** The one plaintext
object is `kopia.repository`, Kopia's format blob by design: tool, version, algorithm
names, a random unique id and an encrypted block — no data, no names.

**What prod recorded:** 18 `GetObject` 404 `NoSuchKey` (Kopia probing for index and
format blobs that do not exist yet — its normal startup shape), no other error.

## 3. Verdict

- **Compatibility:** restic 0.18 and Kopia 0.23 run their whole lifecycle against the
  service through the customer path — init, full backup, incremental, full read-back
  verify, restore, forget/prune and maintenance with blob deletion — with no option
  beyond endpoint and credentials, no error, no retry visible to the user.
- **What the server holds:** encrypted packs plus each tool's own ciphertext metadata.
  No file content and no filename is recoverable from the stored objects. That is the
  client's encryption, not ours; what Stored adds is that it does not get in the way.
- **What is visible to us regardless:** the repository layout (pack count and sizes, the
  snapshot cadence, Kopia's tool/version/algorithm format blob) and the request pattern.
  Say "we hold ciphertext", never "we see nothing".
- **Cloudflare's 100 MB body limit** (Free/Pro, `project_cloudflare_edge_limits`): both
  tools' default pack sizes (restic ~17 MB, Kopia ≤ 21 MB) are far under it. A restic
  repository tuned with `--pack-size 128` would hit the limit on `stored.ge` /
  `s3.stored.cloud` as they are proxied today; leave the default or stay under ~96 MiB.
- **Not measured here:** throughput from a fast link (the Mac's uplink bounded every
  number above), a 10–70 GB repository, a cold-cache Kopia restore.

## 4. How it was run

```bash
set -a; . ./.env.bench; set +a      # VAULTAIRE_BENCH_ACCESS_KEY / _SECRET_KEY (the bench tenant)
export AWS_ACCESS_KEY_ID=$VAULTAIRE_BENCH_ACCESS_KEY AWS_SECRET_ACCESS_KEY=$VAULTAIRE_BENCH_SECRET_KEY AWS_DEFAULT_REGION=us-east-1

# restic
aws --endpoint-url https://stored.ge s3 mb s3://restic-e2ee-20261101
export RESTIC_REPOSITORY=s3:https://stored.ge/restic-e2ee-20261101 RESTIC_PASSWORD=…
restic init && restic backup "$DATASET" && restic check && restic check --read-data
restic restore latest --target "$RESTORE" && (cd "$RESTORE$DATASET" && shasum -a 256 -c "$SHA_LIST" --quiet)
restic backup "$DATASET"                      # after editing three files
restic forget --keep-last 1 --prune && restic check
aws --endpoint-url https://stored.ge s3 cp --recursive s3://restic-e2ee-20261101/ ./ct/ && grep -rlF "$MARKER" ./ct | wc -l   # 0
aws --endpoint-url https://stored.ge s3 rb --force s3://restic-e2ee-20261101

# kopia
aws --endpoint-url https://stored.ge s3 mb s3://kopia-e2ee-20261101
export KOPIA_PASSWORD=… KOPIA_CONFIG_PATH=… KOPIA_CACHE_DIRECTORY=… KOPIA_CHECK_FOR_UPDATES=false
kopia repository create s3 --bucket=kopia-e2ee-20261101 --endpoint=stored.ge --region=us-east-1 \
      --access-key="$AWS_ACCESS_KEY_ID" --secret-access-key="$AWS_SECRET_ACCESS_KEY"
kopia snapshot create "$DATASET" && kopia snapshot verify --verify-files-percent=100
kopia restore <snapshot-id> "$RESTORE" && diff -rq "$RESTORE" "$DATASET"
kopia snapshot create "$DATASET"              # after editing three files
kopia snapshot delete <snapshot-id> --delete && kopia maintenance run --full --safety=none
kopia snapshot verify --verify-files-percent=100 && kopia repository disconnect
aws --endpoint-url https://stored.ge s3 cp --recursive s3://kopia-e2ee-20261101/ ./ct/ && grep -rlF "$MARKER" ./ct | wc -l    # 0
aws --endpoint-url https://stored.ge s3 rb --force s3://kopia-e2ee-20261101
```

The dataset generator plants the marker as the first bytes of every file and fills the
rest with `os.urandom` (incompressible) or hex text (compressible). Run 15:19–15:24 UTC.
