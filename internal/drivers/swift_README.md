# Blomp — the `blomp` driver (OpenStack Swift)

`swift.go` is a generic OpenStack Swift driver behind Keystone v2, written for
Blomp and registered as **`blomp`** when `BLOMP_PASSWORD` is set. Target-only
and a strict target, like `sync`: objects land there only for a bucket whose
`tier_preference = 'blomp'` (operator-set, SQL only) of a tenant with the
`blomp_backend` flag (default off). Never the primary (`STORAGE_MODE=blomp` is
a boot Fatal), never a fall-over destination.

## What Blomp is (probed live 2026-10-21, account partner@fairforge.io)

| | |
|---|---|
| Auth | Keystone **v2**, `https://authenticate.blomp.com/v2.0/tokens` (also `authenticate.ain.net`), tenant `storage`, user = the account e-mail, password. A token lives **1 hour** |
| Storage URL | from the catalog: `http://swiftproxy.acs.ai.net:8080/v1/AUTH_<id>` — **plain HTTP**, no TLS on any port |
| Containers | ONE, named after the e-mail. Account HEAD/listing 403, creating another container 403, container metadata POST 403 (so no TempURL key, no public ACL) |
| Quota | `x-container-meta-quota-bytes` on a container HEAD: 40 GiB on the free account |
| Cluster | Swift 2.17.1; `max_file_size` 5 GiB, `max_object_name_length` 1024, listing limit 10,000; `slo`, `bulk_delete`, `ratelimit`, `swift3`, `tempurl` enabled |
| Large objects | **SLO and DLO work with the pieces in the SAME container.** The community docs' "Permission denied" is rclone writing pieces to `<container>_segments`, which Blomp refuses |
| S3 (swift3) | unusable: SigV4 → `SignatureDoesNotMatch`, and the bucket name has an `@` |
| Writes | rate-limited per container: a 1 KiB PUT 0.3–8 s, 16 in flight = 2.75 PUT/s (p50 3.5 s, max 13.6 s). Reads on a kept-alive connection ≈ 60 ms (the RTT from SLC is 49 ms) |
| Throughput from SLC | one 64 MiB stream 16–39 MB/s up, 12–79 down; 256 MiB GET 122 MB/s single, 163 with 8 ranges; 32 × 64 MiB in parallel 150 MB/s up / 199 down with ~6 % 503; run-to-run ×4 variance |
| Listing | eventually consistent: counts and names lag writes/deletes by tens of seconds |
| Quirks | `DELETE ?multipart-manifest=delete` on a plain object answers **200** with `"Not an SLO manifest"` in the body; overwriting a large object leaves its old pieces |
| Terms | blomp.com/terms-and-conditions (2020): no explicit resale ban; a free account is closed after 1 year without access |

## Layout

`t-<tenant>/<container>/<artifact>` in the account's container, the tenant
from the call's context (`ErrNoTenant` without one) — the fixed-bucket shape.
An object larger than `BLOMP_SEGMENT_SIZE` (1 GiB), or of unknown length past
8 MiB, is a static large object: pieces at
`t-<tenant>/_segments/<sha256(name)[:16]>/<generation>/<n>` in the same
container, each MD5-checked against its ETag, then the manifest at the key.
A failed upload bulk-deletes its pieces (one write); the erasure walk
(`WalkTenant`) lists `_segments` too. Our writes never overwrite in place
(every whole-object write has a new address since WP-R2-1), so the leftover
pieces of an overwritten large object are not a product case.

## Behaviour

- **Token**: asked once and reused; asked again 5 min before it expires, and
  once on a 401 (`vaultaire_swift_auths_total{backend,reason,outcome}`). A
  changed password fails the next request after the token goes (≤ 1 h) — the
  probe's container HEAD then fails.
- **Retries**: a 5xx or a transport error is retried up to 3 attempts in all
  when the body can be sent again (no body, or a body ≤ 8 MiB held in
  memory); a streamed body is sent once (`vaultaire_swift_retries_total`).
- **Delete**: HEAD first (a HEAD is not rate-limited), then ONE write — a
  plain DELETE, or `?multipart-manifest=delete` for a large object (its pieces
  go with it). A miss is `engine.NotFoundError`.
- **Range**: 206 served as is; a 200 to a Range is skipped to the offset and
  cut.
- **Bounds** (the bound invariant, `CLAUDE.md`): 25 s to the first byte after
  the body; HEAD, DELETE, a listing page and the token request 15 s whole; a
  job's walk page 60 s; the failed-upload cleanup 2 min.
- **Probe**: `HealthCheck` = a token (when none is current) + container HEAD;
  the object canary (`api/backend_canary.go`) HEADs and writes
  `t-_probe/_probe/<host>`.
- `CleartextTransport()` is true until a token names an https storage URL.

## Known limit — the listing lags

Measured 2026-10-21: a bulk delete fed from a container listing taken right
after 128 small PUTs reported every name it was given as deleted, and ~30
objects written in that minute were still there 90 minutes later — the
listing had not named them yet. `WalkTenant` (the erasure sweep) and any
cleanup that starts from a listing can therefore miss objects written in the
last minute(s); the deletion runner's second pass narrows it, it does not
close it. Before customer data goes on `blomp`, the sweep needs a re-walk
after a delay on this backend (a follow-up), or the rows must name every
blob (they do for whole objects: the runner deletes each recorded object by
its address, not by listing).

## Encryption — plain HTTP

Rule 4 of `docs/ARCHITECTURE_CONTRACT.md`: no surface invents a key, so the
driver does not encrypt. `api.decidePlacement` seals every write of the
`BLOMP` class with SSE-S3 (the tenant key hierarchy) whatever the bucket says,
the customer's SSE-C key aside, and refuses it (503) on a process with no
master key; CopyObject into a `blomp` bucket seals the same way. The Vault
parity job never makes a cleartext-transport driver its leg — the four parity
shards of an unsealed object rebuild it.

**`BLOMP_ALLOW_UNSEALED=true`** (the owner's testing decision, 2026-10-21)
lifts both: blomp-tier writes follow the bucket's own encryption setting and
Blomp may be the parity leg. One Warn line at boot says it is set. Remove it
before any customer data goes there.

## Configuration

| Variable | Default | |
|---|---|---|
| `BLOMP_PASSWORD` | — | set = the driver is registered (never logged) |
| `BLOMP_USER` | — (required with the password) | the account e-mail |
| `BLOMP_AUTH_URL` | `https://authenticate.blomp.com/v2.0` | Keystone v2 base |
| `BLOMP_TENANT` | `storage` | |
| `BLOMP_CONTAINER` | the user | |
| `BLOMP_SEGMENT_SIZE` | `1GiB` | 1 MiB (Swift's min_segment_size) – 5 GiB |
| `BLOMP_ALLOW_UNSEALED` | off | see above |

## Live tests

`go test ./internal/drivers/ -run 'TestSwift|TestBlomp'` runs every behaviour
test on an in-process fake (`swift_fake_test.go`) and, with `BLOMP_USER` +
`BLOMP_PASSWORD` set, again against the real account (segments of 1 MiB; a
random `_test-<hex>` tenant per test, removed at the end; listing assertions
skip on the live run because the listing lags):

```
BLOMP_USER=partner@fairforge.io BLOMP_PASSWORD="$(security find-generic-password -s blomp -w)" \
  go test ./internal/drivers/ -run 'TestSwift|TestBlomp' -count=1 -v
```

Pairings: `cmd/tools/erasure-bench` registers `blomp` from the same variables
(`-layout local:4,blomp:4`, `geyser:4,blomp:1,lyve:1,sync:1,permafrost:1`, …).
