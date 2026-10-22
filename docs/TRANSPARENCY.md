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
| `server_build` | `git_sha`, `binary_sha256` (SHA-256 of the running executable), `build_time`, `go_version` | every boot, once per distinct (git sha, binary) — a restart of the same binary appends nothing |
| `key_ceremony` | `ceremony` (`master_key_install`, `master_key_rewrap`, `escrow_share_rotation`, `recovery_realm`, `translog_key_install`), `key_fingerprint` (`sha256:<hex>` — never the key), `participants`, `statement` (≤ 2000 bytes; no e-mail, IP, UUID, tenant id or object path) | `cmd/tools/translog-append` / the admin route |

Not yet (K4b): admin reads of customer metadata, client build hashes, law-enforcement productions, `audit_logs` roots, the fTPM quote of a build.

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
4. Append the ceremony itself: `translog-append -ceremony translog_key_install -fingerprint sha256:<sha256 of the verifier key line> -participants 1 -statement "…"`.

Losing the key loses nothing already signed: a new key is a new ceremony entry signed by the new key, and readers pin both. A leaked key is the same, plus a statement.

## The checkpoint job

`translog_checkpoint`, every 10 min (+90 s at boot, 2 min ceiling), under the job lock: when the log grew past the newest checkpoint it computes the root from the stored hashes, **proves the log extends the newest checkpoint** (a consistency proof verified by `transparency-dev/merkle`), and signs. A log that does not extend its last checkpoint is a failed run with the Error line `translog checkpoint: nothing signed … does not extend checkpoint N` — **never re-sign over it**: that is the event the log exists to expose; investigate the database (restore? manual SQL?) first. Result in `job_runs.result`: `{size, checkpoint_size, signed, root, reason}` (`reason`: `unchanged`, `empty`, `no signing key`, `already signed`).

## Metrics and alerts

`vaultaire_translog_size`, `vaultaire_translog_checkpoint_size`, `vaultaire_translog_checkpoint_timestamp_seconds` (read from the tables on scrape), `vaultaire_translog_signing_key_loaded`, `vaultaire_translog_append_failures_total{kind}`. Rules `deploy/monitoring/vaultaire-translog.yml`: **TranslogCheckpointStale** (key loaded, entries past the newest checkpoint, which is > 1 h old, for 15 min), **TranslogAppendFailing**; the job is in `PeriodicJobStale`.

## Verifier key

Not generated yet (owner ceremony above).
