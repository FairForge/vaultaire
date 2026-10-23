# The transparency log

Keystone K4 (Stage 1a Prompt 28; plan Phase 43.6; decision F-16; `docs/ARCHITECTURE_CONTRACT.md` rule 7: *any act that could widen our access writes to the transparency log*). This page is the operator's page and the source of what trust.stored.cloud will publish.

## What it is

An append-only Merkle log in the RFC 6962 / RFC 9162 shape. Every entry is a small JSON document of a known kind; the log's state is a **checkpoint** — tree size and root hash — signed as a C2SP signed note ([c2sp.org/tlog-checkpoint](https://c2sp.org/tlog-checkpoint), [c2sp.org/signed-note](https://c2sp.org/signed-note)). Anyone holding an earlier checkpoint can ask for a **consistency proof** and check that today's log extends it: a rewritten or deleted entry cannot hide. Anyone holding an entry can ask for an **inclusion proof**.

- **Origin:** `stored.cloud/translog/v1` — the checkpoint's first line and the signing key's name. A new origin is a new log.
- **Code:** `internal/translog` (no hand-written Merkle code: `golang.org/x/mod/sumdb/tlog` and `sumdb/note`, `transparency-dev/formats/log`, verification by `transparency-dev/merkle/proof`), `internal/translog/logstore` (Postgres), migration 103.
- **Tables:** `translog_entries` (idx, kind, body = the exact bytes hashed, dedup_key, created_at), `translog_hashes` (tlog's stored hashes by storage index), `translog_checkpoints` (size, root, note, signed_at). Append-only by trigger (UPDATE/DELETE/TRUNCATE raise) and by structure (only `logstore` writes them). Public infrastructure: no tenant column, never erased, never exported, never pruned.

## What is recorded

Each kind is a typed struct with an allow-list of fields; none names a tenant, a person, an e-mail, an IP address or an object (`TestTranslog_EntriesNeverNameATenant`). A body is `{"kind":…,"v":1,"at":"<UTC>","entry":{…}}`.

| Kind | Fields | Written by |
|---|---|---|
| `server_build` | `git_sha`, `binary_sha256` (SHA-256 of the running executable), `build_time`, `go_version` | every boot, once per distinct (git sha, binary) — a restart of the same binary appends nothing; an append refused (a frozen log, the database down) is kept and retried by every `translog_checkpoint` run until it lands (RP42-1) |
| `key_ceremony` | `action` (closed list: `key_generated`, `key_installed`, `key_rewrapped`, `key_rotated`, `key_retired`, `escrow_split`, `escrow_tested`, `realm_enrolled`, `log_restored`), `key_role` (`master_key`, `translog_signing_key`, `recovery_realm_key`), `key_fingerprint` (`sha256:<64 hex>` — never the key), `participants` (1–20), `shares` / `threshold` (the trustee shares written and how many recombine the key; 0/0 = none, else 2–20 and 1–shares), `signed_size` / `restored_size` / `new_origin` (`log_restored` only), `reference` (empty, or `https://github.com/FairForge/vaultaire/` + `pull/<n>`, `commit/<40 hex>` or `blob/<40 hex>/docs/<NAME>.md`). **No free text** (RP37-10): a person's name, an address, an e-mail, a tenant id cannot be written in any field — what a person would say goes in the referenced document, which we can correct; the entry cannot be (the tables refuse DELETE). An unknown field (the removed `statement` of #884 included — 0 ceremony entries existed when it went) is refused | `POST /api/v1/admin/translog/ceremony` (admin JWT + a fresh TOTP code of the admin's own MFA, consumed only after the entry validated; `Idempotency-Key` = a retry returns the first answer and appends nothing; audited `admin.translog_ceremony`, a non-admin's 403 included) |
| `admin_read` | `scope` (closed list, below), `actor_role` (`admin` — a role, never the person), `nonce` (16 random bytes, hex) and `commitment` (HMAC-SHA256 under the read tenant's salt, hex) for a read of ONE tenant; both empty for a read across tenants | every admin GET that reads customer metadata, BEFORE it is served (K4c-1, § Admin reads), behind `translog_admin_reads` |

Not yet (K4c-2/3): client build hashes, law-enforcement productions, `audit_logs` roots, the fTPM quote of a build; OpenTimestamps anchoring, the Rekor mirror, and a co-signature by an independent third party. K4b (Stage 1a Prompt 32) built our own off-box witness (§ The witness), the byte-exact restore (§ After a database restore) and the trust host (§ Reading the log).

On prod since 2026-10-22 16:07 UTC (`83ef6c9`): entry 0 is that build's `server_build`, its `binary_sha256` equal to `sha256sum /opt/vaultaire/bin/vaultaire-8001`.

### Recompute a `server_build` entry

Since RP37-9 (Stage 1a Prompt 30) every deployed binary is built by ONE reproducible recipe, `scripts/build-release.sh`: go.mod's `toolchain` (go1.26.10), `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false`, `BuildSHA` = the commit's first 12 hex, `BuildDate` = the **commit** time in UTC (so `/version`'s `build` and the entry's `build_time` are when the commit was made). The deploy job fails unless two more builds from fresh copies of the commit, with their own `GOCACHE` and `GOPATH`, give the same bytes (`scripts/repro-check.sh`). Anyone can rebuild the binary an entry names:

```sh
git clone https://github.com/FairForge/vaultaire && cd vaultaire
git checkout <the entry's git_sha>          # 12 hex is enough for git
GOOS=linux GOARCH=amd64 scripts/build-release.sh /tmp/vaultaire-linux   # needs go ≥ 1.21 and git; fetches go1.26.10 itself
sha256sum /tmp/vaultaire-linux             # (macOS: shasum -a 256) = the entry's binary_sha256
```

The entry's `build_time` and `go_version` must also read as the commit's time and `go1.26.10`. **Entries 0–7 (2026-10-22, builds before this recipe) cannot be recomputed**: their builds stamped the wall clock and kept the build paths (two binaries for one sha — `c2a7fa698eb2`, `d464454ea785`). They stay as they are: history, true about what ran, unverifiable about what it was built from.

## Admin reads

K4c-1 (Stage 1a Prompt 33; contract rule 7: "an admin read of customer metadata" writes to the log; "customers read outcomes; auditors read the log"). Before K4c-1 an operator reading one tenant's records left a row in `audit_logs` — which we can rewrite — and nothing in the log.

**What is logged.** Every admin GET route is classified beside where it is registered (`internal/api/admin_reads.go` `apiAdminReads`, `internal/dashboard/admin_reads.go` `adminReadRoutes`); a route with no classification panics at registration, and `TestAdminReadRoutes_AllClassified` walks the whole router (every admin GET is classified and registered through the gate). The inventory with every route's verdict is in `docs/reviews/WP-P33.md` § K4c-1.

| Scope | Reads | Routes |
|---|---|---|
| `tenant_detail` | one tenant's record | dashboard `/admin/tenants/{id}` |
| `tenant_payments` | one tenant's entitlement and ledger | `GET /api/v1/admin/tenants/{tenant_id}/payments` |
| `tenant_audit` | one tenant's (or one user's, logged against the user's tenant) audit trail, viewed or exported | `/admin/audit?tenant=` / `?user=`, `/admin/audit/export` filtered, `GET /api/v1/admin/audit?tenant_id=` / `?user_id=` |
| `support_case` | one customer's support view (buckets, keys, IPs of its errors, notes) | `/admin/support/{id}` |
| `abuse_case` | one abuse report and the tenant it names | `/admin/abuse/{id}` |
| `abuse_report` | one abuse report that names no tenant | `/admin/abuse/{id}` |
| `tenant_list` | tenants or users with their e-mail / name | `/admin/`, `/admin/tenants`, `/admin/support` |
| `audit_list`, `audit_export` | the audit trail across tenants | `/admin/audit`, `/admin/audit/export`, `GET /api/v1/admin/audit` |
| `waitlist_list`, `waitlist_export` | the waitlist's e-mails | `/admin/waitlist`, `/admin/waitlist/export` |
| `abuse_list` | the abuse reports | `/admin/abuse` |
| `billing_list` | revenue, costs, margins per tenant by e-mail | `/admin/revenue`, `/admin/costs` |
| `usage_list` | dedup per tenant by e-mail | `/admin/dedup` |
| `flag_list` | the tenants a flag is set for | `/admin/flags`, `GET /api/v1/admin/flags` |
| `job_list` | job results and notes, which name tenants, buckets, keys | `/admin/system`, `GET /api/v1/admin/jobs` |
| `notice_list` | the operator notifications | `/admin/notifications` |

Exempt, with the reason in the map: the site statistics, the unread count, the backends' health, routing-truth counts, the vendor plans, the breach register and ROPA, and the compliance scaffolding.

**The commitment.** A read of one tenant carries a random 16-byte `nonce` and `commitment = HMAC-SHA256(salt_t, "stored.cloud/translog/admin_read" ‖ nonce)`. `salt_t` is the tenant's 32 random bytes in `translog_subjects` (migration 104), created at its first admin read, erased by `account.EraseRows`, excluded from the GDPR export, and never shown to anyone. So:
- **the public** sees how many admin reads happened and of which scope, and cannot link two entries to one tenant (a fresh nonce each time; the commitment is an HMAC under a key it does not have);
- **the tenant** finds its own entries: `GET /api/v1/manage/account/transparency` (behind `translog_customer_view`, default OFF, per tenant first) recomputes each entry's commitment under its salt and lists index, scope, time and the first signed checkpoint whose tree includes it;
- **after an erasure** no one — we included — can link the past entries to the tenant: the salt is gone, the entries stay (they name no one), and their coalescing keys are keyed by the same salt.

**Coalescing.** One entry per (operator, scope, tenant) per UTC hour: a page refresh appends nothing. The key is `admin_read:<HMAC(salt_t, operator ‖ scope ‖ hour)>` for a tenant read and `admin_read:<SHA-256(operator ‖ scope ‖ hour)>` for a list, in `translog_entries.dedup_key` — never in the body (the tiles serve bodies only).

**Fail closed, append first.** The entry is appended BEFORE the handler reads anything, so an entry exists even if the response is lost. An append that fails — a frozen log (§ After a database restore), the database — refuses the read: the API answers 503 `{"error":"log_frozen"}` or `{"error":"admin_read_not_logged"}`, the dashboard a 503 page. There is no break-glass. The operator stays in `audit_logs` (`admin.read`, with the entry's `translog_index` — the reference the log itself does not reveal).

**The flag.** `translog_admin_reads` (default OFF) is keyed on the SUBJECT tenant (per tenant first; a list reads the global row). OFF = no entry and no refusal, the behaviour before K4c-1. **ON globally** (the owner's flip) = every admin read of customer metadata is in the public log, and an operator cannot read a tenant's records while the log is frozen or the database refuses the append.

### What the log does not see

The log records what goes through the application. **Root on the box reading Postgres directly** (`psql`, a dump, a copy of the data directory) leaves no entry — and neither does anything else done with the box's own privileges: a binary that never logs, a debugger attached to a slot. Until the attested data plane exists (contract rule 10: measured image, no SSH, published measurements), nothing in this log covers that, and we do not claim it does. The `server_build` entries say which binaries ran; they do not prove that nothing else did.

## Reading the log

On the **trust host** — `https://trust.stored.cloud/translog/` (`TRUST_PUBLIC_HOST`, default `trust.<canonical host>`; `trust.stored.ge` answers too) — behind the flag **`translog_public`** (default OFF, global; OFF = 404, owner decision to flip): `GET /translog/checkpoint` (the signed note, `Cache-Control: no-store`) and `GET /translog/tile/<L>/<N>[.p/<W>]`, `/translog/tile/entries/<N>[.p/<W>]` in the [c2sp.org/tlog-tiles](https://c2sp.org/tlog-tiles) layout (`internal/translog/serve.go`). Only what the newest signed checkpoint commits to is served: no checkpoint (no signing key) = 404 everywhere, even with the flag on. A full tile is `public, max-age=31536000, immutable`; a partial tile, a 503 and every 404 (a tile beyond the checkpoint, no checkpoint yet, the flag OFF) are `no-store`, so the Cloudflare edge in front of the trust host never keeps an answer that changes. `HEAD` answers like `GET` with no body (RP41-6).

**Only on the trust host (K4b, RP35-1).** With a public canonical origin, `/translog/…` on any other host — `stored.cloud`, `s3.stored.cloud`, `stored.ge` — is the S3 API's, signed or not, exactly as before K4: an anonymous GET of a public customer bucket named `translog` is no longer shadowed. On the trust host a signed or presigned S3 request is 404, never the S3 API. **The trust host serves the log and nothing else** (RP42-2, Stage 1a Prompt 33; contract rule 11): `TrustHostRouter` (`internal/api/trust_host.go`) answers it before the app's router, with GET/HEAD of the two log paths and `/.well-known/security.txt`; every other path — `/`, `/dashboard/…`, `/api/…`, `/auth/…`, `/metrics`, a signed `PUT /bucket/key` — is `404 text/plain`, `no-store`, any method, signed or not. No landing page and no login form are served from the origin readers are taught to trust. A development origin (localhost, an IP) keeps K4's rule on its one origin (a signed request is the S3 API's, an unsigned one the log's), so labs work unchanged.

Verify from outside with `cmd/tools/translog-verify` (any machine; nothing of ours trusted but the pinned key):

```sh
go run ./cmd/tools/translog-verify -url https://trust.stored.cloud/translog -vkey @translog.vkey      # signature + consistency with the last run
go run ./cmd/tools/translog-verify -url https://trust.stored.cloud/translog -vkey @translog.vkey -entry 0  # + inclusion of entry 0, printed
```

It keeps the checkpoint it verified last in `~/.translog-verify/checkpoint` (`-state`) and moves it forward only after every check passed; a checkpoint that does not extend it — a rewritten or forked history — is a non-zero exit. Run it from a cron of your own and you hold a checkpoint the log must extend; `translog-witness` (§ The witness) also co-signs and keeps a byte copy.

## The witness

K4b (Stage 1a Prompt 32; plan 43.6: "an independent party co-signing each checkpoint and keeping its own record"). `cmd/tools/translog-witness` (library `internal/translog/witness`, never linked into `cmd/vaultaire`) runs on **another machine**, against the public read side, and trusts nothing of ours but the pinned verifier key of the log. One pass:

1. fetch the newest checkpoint and verify its signature;
2. prove it extends the checkpoint the witness accepted last (`<dir>/checkpoint`). A fork at the same size, a rewritten prefix or a shrunk log is a non-zero exit, and nothing moves: the tiles of a refused tree are never kept;
3. **mirror** byte for byte, under `<dir>/cache/` in the tlog-tiles layout itself, every full entry bundle and full level-0 hash tile it does not hold yet, plus the partial entry bundle at the checkpoint's size (replaced as it widens). Every entry's leaf hash is proven against the checkpoint before it is kept, and every tile is authenticated by `tlog.TileHashReader`. Full tiles are immutable, so they are never fetched again;
4. **co-sign:** the log's note, opened with the log's key, signed again with the witness's own key (`note.Sign` keeps the verified signature). The note goes to `<dir>/cosigned` with both signature lines, and each new note is appended to `<dir>/cosigned.history` (append-only, base64, one per line).

The accepted checkpoint moves forward only after every step passed. The witness refuses the log's own key as its `-key`.

```sh
translog-keygen -name witness.<host> -out ~/.translog-witness/witness.key > witness.vkey   # the witness's OWN key, kept on that machine
translog-witness -vkey @translog.vkey -key @$HOME/.translog-witness/witness.key           # one pass (-once, the default): for cron
# */10 * * * *  translog-witness -vkey @… -key @… >> ~/.translog-witness/log 2>&1
```

What it is for:
- with its cache, entries a database restore lost can be put back **byte for byte** (§ After a database restore, `translog-restore -from <dir>`);
- its accepted checkpoint is a tree the restored log must extend, and it survives the loss of the box and its high-water file (RP38-5).

**What it is not yet:** this is our own second record, under our own key, on another box (the owner's Mac from a cron, once the signing-key ceremony has run). A co-signature by an **independent third party**, in the [c2sp.org/tlog-cosignature](https://c2sp.org/tlog-cosignature) timestamped form, comes with a real external witness (K4c), as do OpenTimestamps and the Rekor mirror.

## The signing key

Loaded like the master key (`docs/ENCRYPTION.md`): the systemd credential `translog_signing_key` (`$CREDENTIALS_DIRECTORY/translog_signing_key`), then `TRANSLOG_SIGNING_KEY_FILE=<path>`, then `TRANSLOG_SIGNING_KEY`. A source set but unreadable or malformed — or a key named for another origin — **refuses the boot**. One Info line `translog signing key: credential|file|env` with the key ID (public: it is in every signature), never the key; a Warn names lower sources also set. **No key:** entries still append, no checkpoint is signed, `vaultaire_translog_signing_key_loaded 0` (prod until the owner's ceremony).

### Ceremony (owner; can share the F-14 master-key session)

In one root shell on the box (`sudo -i`):

```sh
umask 077
install -d -m 0700 -o root -g root /etc/vaultaire/credentials
/opt/vaultaire/bin/translog-keygen -out /root/translog.key > /root/translog.vkey   # key ID on stderr
systemd-creds encrypt --with-key=tpm2+host --name=translog_signing_key \
    /root/translog.key /etc/vaultaire/credentials/translog_signing_key.cred
systemd-creds decrypt --name=translog_signing_key \
    /etc/vaultaire/credentials/translog_signing_key.cred - | cmp - /root/translog.key && echo sealed-ok
shred -u /root/translog.key
cat /root/translog.vkey                     # the VERIFIER key: public
```

(`translog-keygen` is built with `GOOS=linux GOARCH=amd64 go build -o translog-keygen ./cmd/tools/translog-keygen` and copied to the box.)

1. **Publish the verifier key outside our infrastructure before the first signed checkpoint** — a signed git tag or a gist under the owner's account, a post, a printed copy — so a reader can pin it independently of anything we serve. Then add it to `docs/TRANSPARENCY.md` § Verifier key.
2. Add `LoadCredentialEncrypted=translog_signing_key:/etc/vaultaire/credentials/translog_signing_key.cred` to `/etc/systemd/system/vaultaire@.service`, `systemctl daemon-reload`, `sudo vaultaire-switch restart`.
3. Check the new slot's log: `translog signing key: credential` with the key ID; `vaultaire_translog_signing_key_loaded 1`; within ~12 min `translog checkpoint signed`.
4. Append the ceremony itself (the admin account must have MFA enrolled; the code is consumed once):

   ```sh
   FP=$(tr -d '\n' < /root/translog.vkey | sha256sum | cut -c1-64)
   for A in key_generated key_installed; do
     curl -s -X POST https://stored.ge/api/v1/admin/translog/ceremony -H "Authorization: Bearer $ADMIN_JWT" \
       -H "Idempotency-Key: translog-key-$A-$(date -u +%F)" \
       -d '{"action":"'"$A"'","key_role":"translog_signing_key","key_fingerprint":"sha256:'"$FP"'","participants":1,
            "reference":"https://github.com/FairForge/vaultaire/blob/<40-hex commit>/docs/TRANSPARENCY.md","mfa_code":"<6 digits>"}'
   done   # one fresh code per call (each is consumed once)
   ```

   The F-14 master-key install is the same calls with `"key_role":"master_key"` and the fingerprint of the master key (`sha256` of its 32 bytes, computed on the box — never the key), then `escrow_split` with `"shares":3,"threshold":2` and `escrow_tested` after the recombination test. 201 `{"index": n}`; a retry with the same `Idempotency-Key` answers the same index and appends nothing; a refused entry (400) does not consume the code.

Losing the key loses nothing already signed: a new key is a new ceremony entry signed by the new key, and readers pin both. A leaked key is the same, plus a `key_retired` entry for it and a `reference` to the incident's write-up.

## The checkpoint job

`translog_checkpoint`, every 10 min (+90 s at boot, 2 min ceiling), under the job lock: when the log grew past the newest checkpoint it computes the root from the stored hashes, **proves the log extends the newest checkpoint** (a consistency proof verified by `transparency-dev/merkle`), and signs. A log that does not extend its last checkpoint is a failed run with the Error line `translog checkpoint: nothing signed … does not extend checkpoint N` — **never re-sign over it**: that is the event the log exists to expose; investigate the database (restore? manual SQL?) first. Result in `job_runs.result`: `{size, checkpoint_size, highwater_size, signed, root, reason, refused, note}` (`reason`: `unchanged`, `empty`, `no signing key`, `already signed`; `refused`: below).

**The high-water file (RP37-11).** A database restored from the 03:00 dump holds the checkpoints of the dump's moment; without more, the job would find the restored log consistent with its own newest checkpoint and sign a DIFFERENT tree at sizes the key already signed and published — the key equivocating. So the job keeps the newest checkpoint it signed **outside the database**: `TRANSLOG_HIGHWATER_FILE` (default `/opt/vaultaire/data/translog/highwater`, on the NVMe, read by both slots; an absolute path), the checkpoint's body (origin, size, base64 root) written atomically and 0600 after each signature (after its row, never before). Every pass proves the stored log extends **both** the newest checkpoint in the database and the file's, and refuses otherwise, counted in `vaultaire_translog_checkpoint_refusals_total{reason}`:

- `behind_highwater` — the database holds fewer entries than the tree in the file (a restore);
- `rewritten` — the stored log does not extend a tree the key signed (the database's or the file's: a restore whose log has since grown with different entries, or edited rows);
- `highwater_unreadable` — the file exists but cannot be read or is not a checkpoint of this log. Never taken as "first run".

An absent file is the first run (`note: first run …`): it is written after the next signature. Every pass re-proves, so a refusal repeats every 10 min until it is resolved; `TranslogCheckpointRefused` **pages** at the first one. **The same file gates every append** (RP42-1): behind it, or unreadable, the log is frozen — § After a database restore.

## After a database restore

Decided in Stage 1a Prompt 30; the log's freeze since Prompt 33 (RP42-1); the cost of the new origin is the owner's to confirm (F-33).

**The log freezes by itself.** `logstore.Append` reads the high-water file inside its transaction, under the log's lock, on every append: while the database holds fewer entries than the file (`behind_highwater`), or the file exists but is not a checkpoint of this log (`highwater_unreadable`), **every append is refused** (`ErrLogFrozen`, nothing written, `vaultaire_translog_append_refusals_total{reason}`, **TranslogFrozen** pages). An absent file is the first run and appends. So no entry the key never signed can take index M, and the re-append below always stays possible — before Prompt 33 a slot's boot `server_build` took index M and made it impossible for ever (the tables refuse DELETE). While frozen:
- **the slots may run and serve customers**; only the log waits. Stopping them is no longer the precondition, and `translog-restore` runs against the live database;
- the boot's `server_build` is kept in the process and retried by every `translog_checkpoint` run (before it checkpoints) until it lands — after the re-append, at N or later;
- the ceremony route answers **503 `{"error":"log_frozen"}`** before the TOTP code is consumed (retry with the same code and `Idempotency-Key` once the log is repaired); an admin read of customer metadata (K4c-1) is refused while the `translog_admin_reads` flag is on for its tenant;
- the checkpoint job refuses to sign (`behind_highwater`, **TranslogCheckpointRefused**);
- `logstore.ReAppend` (the way out) is not gated: its own checks stand (the count, the size, the recomputed root).

Compare the two:

```sh
sudo cat /opt/vaultaire/data/translog/highwater          # stored.cloud/translog/v1 / N / root
sudo -u postgres psql -At -d vaultaire -c "SELECT COALESCE(MAX(idx)+1,0) FROM translog_entries"   # M
```

1. **M ≥ N and the job would pass** (the dump was taken after the newest signature): nothing happened to the log. The next pass proves it.
2. **M < N** (entries the key signed are gone; the log is frozen): the tree the key signed at N exists again only if entries M…N−1 come back **byte for byte**. Any other tree this database grows is not the one the key signed, and a signature over it would be the equivocation.

   **Keep the origin: re-append from the witness** (K4b). This works when a witness — `translog-witness`, run off the box — saw a checkpoint at N or later, so its cache holds every entry up to N−1. The slots may be running:

   ```sh
   # copy the witness's directory (its cache/ is what is read) to /root/translog-witness, and the tool:
   #   GOOS=linux GOARCH=amd64 go build -o translog-restore ./cmd/tools/translog-restore
   DATABASE_URL='postgres://…/vaultaire' /root/translog-restore \
       -from /root/translog-witness -highwater /opt/vaultaire/data/translog/highwater
   # database: M entries; high-water: stored.cloud/translog/v1 size N root …
   # re-appended entries M…N-1: the root at N is … = the high-water's
   ```

   `translog-restore` (`logstore.ReAppend`) appends the cached bodies M…N−1 as they are, in ONE transaction under the log's lock, with their stored hashes. Kind and dedup key come from each body. It commits only when the root it recomputes at N is the file's root. It refuses, and writes nothing, when M ≥ N (that is case 1), when a bundle it needs is missing or narrower than N (it names the bundle), when a body does not decode, or when the root differs. It never signs. Because the gate holds every append back, M is the dump's size, whatever ran in between.

   Then:
   1. the log thaws at once: the next `translog_checkpoint` run (≤ 10 min, or `POST /api/v1/admin/jobs/translog_checkpoint/run`) appends each running slot's waiting `server_build` after the re-appended entries, proves the restored log extends both the database's newest checkpoint and the file's, and signs: `refused` empty, the refusal counters unchanged from then on;
   2. append the ceremony `log_restored` with `key_role: translog_signing_key`, the key's fingerprint, `signed_size: N`, `restored_size: M`, **`new_origin: false`** and a `reference` to the incident's pull request.

   One thing the re-append cannot give back: a ceremony's `Idempotency-Key` dedup lives outside the body (RP37-14), so a re-appended ceremony entry has no dedup key (RP41-1). A retry of that very call after the restore would append it a second time. Within the idempotency window this is the operator's to avoid.

   **The fallback: a new origin** (the cost is F-33, the owner's). Use it when no witness cache reaches N, or when `translog-restore` refuses because the root differs. The log stays frozen — refusing, paging, appending nothing — until it is done; the customers are served meanwhile:
   - keep the high-water file and the old verifier key public: the last tree of `stored.cloud/translog/v1` is the file's (size N, root);
   - start a **new log**, `stored.cloud/translog/v2`. This is a code change of `translog.Origin` and a new key from the ceremony above; the old key is never loaded again. It starts on empty `translog_*` tables: export the restored v1 rows to a file and keep it, then the operator empties the tables with the triggers disabled, logged in `audit_logs`;
   - **move the v1 high-water file aside before the v2 build starts: `sudo mv /opt/vaultaire/data/translog/highwater /opt/vaultaire/data/translog/highwater.v1`** (RP42-6). A file of another origin is never read as "archived" by itself: the job and the append gate refuse it as `highwater_unreadable`, and the error names this step. Keep `highwater.v1` with the export: it is v1's last signed tree;
   - the v2 log's **first entry** is a `key_ceremony` `log_restored` with `key_role: translog_signing_key`, the OLD key's fingerprint, `signed_size: N`, `restored_size: M`, `new_origin: true` and a `reference` to the incident's pull request. Then `key_retired` for the old key, and `key_generated` / `key_installed` for the new one;
   - publish the new verifier key as in the ceremony; readers re-pin.
3. **The box is lost too** (no high-water file): an absent file reads as "first run" and would not freeze the log, so **write the high-water file BEFORE the first slot starts on the new box**. The witness holds the checkpoint the log must reach: its `cosigned` note (signed by the log's key, so take it only after checking it with the pinned verifier key). Its body is the high-water file:

   ```sh
   install -d -m 0700 -o user1 -g user1 /opt/vaultaire/data/translog   # the slots' user (`stat -c %U /opt/vaultaire/data`)
   sed '/^$/,$d' ~/.translog-witness/cosigned > /opt/vaultaire/data/translog/highwater   # origin / N / root, nothing after
   chown user1:user1 /opt/vaultaire/data/translog/highwater; chmod 0600 /opt/vaultaire/data/translog/highwater
   ```

   With the file in place the log is frozen from the first boot, exactly as in case 2, and the slots may start. Then follow case 2 with that file: re-append from the witness's cache. The size and root are the newest tree the witness proved, which may be older than the last one the box signed. A checkpoint signed after the witness's last pass is lost with the box. That window is the cron's interval: readers who fetched it hold a tree the restored log may not reach, so say so in the incident's write-up. With no witness, this case is case 2's fallback (RP38-5).

**What closed RP41-5.** `translog-restore` still does not check that no slot runs, and no longer needs to: a running slot cannot append while the log is frozen, and a slot that loaded the key refuses to sign until the re-append. Stopping the slots was the only protection before the gate; it is not a step any more.

`docs/DEPLOY.md` § Backups points here.

## Metrics and alerts

`vaultaire_translog_size`, `vaultaire_translog_checkpoint_size`, `vaultaire_translog_checkpoint_timestamp_seconds` (read from the tables on scrape), `vaultaire_translog_signing_key_loaded`, `vaultaire_translog_append_failures_total{kind}`, `vaultaire_translog_checkpoint_refusals_total{reason}` (every reason at 0 from boot), `vaultaire_translog_append_refusals_total{reason}` (`behind_highwater`, `highwater_unreadable`, at 0 from boot; a frozen log's refusals are NOT in `append_failures_total`). Rules `deploy/monitoring/vaultaire-translog.yml`: **TranslogCheckpointRefused** (page: any refusal in 30 min, per reason), **TranslogFrozen** (page: any append refused in 30 min, per reason — RP42-1), **TranslogCheckpointStale** (key loaded, entries past the newest checkpoint, which is > 1 h old, for 15 min), **TranslogAppendFailing**; the job is in `PeriodicJobStale`.

## Verifier key

Not generated yet (owner ceremony above).
