# Stage 1a — Prompt 39: closeout

*Written 2026-10-25 by the Prompt 40 review (RP58). Prompt 39 split into two sessions;
Session B was cut before it could open the docs PR this file belongs to — the WP-P35/#930
shape — so the review wrote the closeout in its place, from the merged PRs and the prod
records. No migration (the next was 108 then and still is).*

## What ran

| Session | PRs | Where its findings live |
|---|---|---|
| A (Part 0 + Part A, the web host) | #948 `72c0fa8a`, #951 `fb48d430` | [`WP-P39-PART0A.md`](WP-P39-PART0A.md) |
| B (tenant-zero proof) | #949 `2a00052f` (modes_copy: the bucket list badged a Standard bucket "Private" with the flag on — the name trap, found by the proof), #950 `bc74487b` (`deploy/scripts/tenant-zero-proof.sh` + its `_test.sh`, wired into CI) | this file; the reviews below |

- Session A and #949 were reviewed 2026-10-24 ([`WP-P39-REVIEW.md`](WP-P39-REVIEW.md), RP56-1…6; note PR #952). RP56-1 (signed S3 requests answered by web routes) became Prompt 40's gate (#956).
- #950 was reviewed by the Prompt 40 review ([`WP-P40-REVIEW.md`](WP-P40-REVIEW.md) § #950): holds. Prompt 40's #956 revised its authentication — the script takes a real MFA sign-in's JWT (`TZP_ADMIN_JWT_FILE`, mode 0600) and never mints from `JWT_SECRET`.

## The tenant-zero proof (rule 2 of the lanes)

The **evening half ran on prod 2026-10-24**: `preflight chunking multipart parallel_get
versioned_chunking` through the customer path (aws-cli, SigV4, stored.ge). Bucket
`tz-proof-1792873702`, 4 objects / 578 MB kept overnight; `chunking`, `parallel_get` and
`versioned_chunking` turned ON **for tenant zero only** at 20:28 UTC — no `'*'` row was
touched.

**Owed — the verdict table cannot be written yet:**

1. the **05:30 UTC `routing_truth`** chunked sample over the overnight bucket (the first
   run that can see it is 2026-10-25's);
2. the **morning half** (`morning bench_cleanup modes dogfood transparency`) — it needs
   the owner: a `POST /auth/login` with the admin's password **and** MFA code, the token
   in a root-only file (RP57-4; the WP-P27 `JWT_SECRET` recipe is the pattern RP56-5
   removed and must not be reused).

**No global flag flip is allowed by this closeout.** Rule 2's proof is complete only when
the morning half and the `routing_truth` read are recorded here with their verdicts.
