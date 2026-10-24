# recovery-realm

One Juicebox-protocol realm (keystone K5; protocol and decisions in `docs/reviews/K5-PROBE.md`; library in `internal/recovery`). It keeps PIN-protected shares for the users of the tenants it serves, counts wrong guesses durably, and erases a record at its limit.

**Not deployed. Not linked into `cmd/vaultaire`** (`TestRecoveryRealm_NotLinkedIntoVaultaire`). A realm belongs on another box: first the attested EPYC VM of plan 41.4, then a second realm under a different operator (F-19). Until both exist, the copy is "attempt-limited by our realm", never "hardware".

## Run (lab, loopback)

```sh
go build -o recovery-realm ./cmd/recovery-realm
openssl rand -hex 32 > realm.key && chmod 600 realm.key
echo '{"stored":{"1":"'"$(openssl rand -hex 32)"'"}}' > tenants.json && chmod 600 tenants.json
RECOVERY_REALM_ID=$(openssl rand -hex 16) RECOVERY_REALM_DB=$PWD/realm.db \
RECOVERY_REALM_KEY_FILE=$PWD/realm.key RECOVERY_REALM_TENANTS_FILE=$PWD/tenants.json ./recovery-realm
curl -s 127.0.0.1:8780/        # {"realm_id": "…", "public_key": "…"}: the configuration a client pins
```

| Variable | Default | |
|---|---|---|
| `RECOVERY_REALM_ID` | (required) | 32 hex characters; a token's `aud` must be exactly this |
| `RECOVERY_REALM_DB` | (required) | the bbolt file (fsync per commit). Erasure is logical: bbolt frees pages without zeroing them, so the volume must be encrypted with a key that does not rest on it |
| `RECOVERY_REALM_LISTEN` | `127.0.0.1:8780` | anything other than loopback needs TLS |
| `RECOVERY_REALM_TLS_CERT`, `RECOVERY_REALM_TLS_KEY` | (none) | both or neither |
| `RECOVERY_REALM_METRICS_LISTEN` | `127.0.0.1:8781` | `off` for none. Serves `recovery_realm_requests_total{op,outcome}` (every pair exported at 0) and `recovery_realm_records_destroyed_total` |
| credential `recovery_realm_key` → `RECOVERY_REALM_KEY_FILE` → `RECOVERY_REALM_KEY` | (required) | the Noise static secret, 64 hex |
| credential `recovery_realm_tenants` → `RECOVERY_REALM_TENANTS_FILE` → `RECOVERY_REALM_TENANTS` | (required) | `{"<tenant>": {"<version>": "<hex HS256 key, ≥ 32 bytes>"}}`; the tokens' `kid` is `<tenant>:<version>` |

A secret source that is set but unusable refuses the boot. A key is never logged; the public key is (it is public).

## Wire

`POST /req` only: a `ClientRequest` (CBOR) with the tenant's JWT inside and the request under Noise NK to the realm's static key. Register2, Recover2 and Recover3 are refused unless an established session carries them. Sessions are keyed by (tenant, subject, session id) and live 5 minutes from last use. With the table full (100,000; 8 per subject), a request that rode the handshake (Delete, Register1, Recover1) is still applied and answered, with session lifetime 0 (no session kept); a handshake carrying nothing answers `Unavailable` (RP54-2). A refused token gets 401 `InvalidAuth`, whether it is missing, expired, has the wrong `aud`, an unknown `kid`, `exp − nbf` over 86 400 s, or a scope other than `user`.

## Enrol

`recovery-realm enrolment` (same environment) prints the `key_ceremony` body to append. See `docs/TRANSPARENCY.md` § Recovery realms.

## Lab against the MIT SDK

`RECOVERY_LAB_SDK_DEMO=<juicebox-sdk>/target/release/demo go test -run Lab ./cmd/recovery-realm/` runs the SDK's own demo client against two realms over Noise: register, recover, self-destruct, re-register, delete.
