# WebDAV driver (`webdav.go`) — first instance: Sync.com's encrypted bridge (`sync`)

A generic WebDAV (RFC 4918) backend. One driver type serves any WebDAV
server — Sync.com's local bridge, Hetzner Storage Box, Koofr, pCloud,
Nextcloud — through `NewWebDAVDriver(name, baseURL, username, password, root, logger)`.
`cmd/vaultaire/main.go` registers ONE instance today: `sync`.

## Role: target-only, flag-gated

`sync` is never the primary and never a failover destination:

- `engine.targetOnlyBackends` lists it (a STANDARD write that fails on the
  primary never lands on it; `CheckPrimaryEligible` refuses it, so the admin
  "Set as Primary" button cannot pick it);
- `config.DetectStorageMode` never selects it, and `STORAGE_MODE=sync` is a
  Fatal at boot (`refusedPrimary` in `cmd/vaultaire/main.go`);
- the only placement is the internal `SYNC` storage class: a bucket whose
  `tier_preference` is `sync` (operator-set — no customer-facing control
  offers it: the management API and the dashboard reject the value) of a
  tenant with the **`sync_backend` feature flag** (default OFF, per tenant,
  never a global row). Without the flag such a bucket places exactly like an
  `auto` one. Reads follow `object_head_cache.backend_name` like every other
  backend. `SYNC` objects are stored whole (no chunking: chunk blobs live on
  the primary) and are reported to the customer as `STANDARD`.

```sql
-- the operator, for a tenant Sync has agreed to in writing (or our own):
UPDATE buckets SET tier_preference = 'sync' WHERE tenant_id = '<tenant>' AND name = '<bucket>';
```
```bash
curl -X PUT -H "Authorization: Bearer $ADMIN_JWT" -d '{"enabled":true,"tenant_id":"<tenant>"}' \
  https://stored.ge/api/v1/admin/flags/sync_backend
```

A dashboard save of that bucket's settings rewrites `tier_preference` from
the form (which does not offer `sync`) — set it again after one.

Not wired: generated objects (access logs, inventory, exports) delivered into
a `sync` bucket place on the primary (`generatedObjectWriter` passes no flag
gate), and a region-pinned bucket keeps its region driver.

### Sync's terms (read before enabling a tenant)

Sync's Terms of Service forbid reselling or providing the service to third
parties without Sync's **written consent**. Our own data and our own backups
on our own Sync account are fine. **Customer data goes there only with that
written consent** — that is what the per-tenant flag is for. Never set a
global `sync_backend` row.

## Layout and key mapping

`<root>/t-<tenant>/<container>/<artifact…>` below the server URL — the
fixed-bucket shape of iDrive/R2/Geyser (`tenantKey`), with the tenant from the
call's context. A call whose context names no tenant is refused with
`ErrNoTenant` (`tenant_ctx.go`); chunk blobs (`engine.ChunkContext`) sit at
`t-_global/_global/_chunks/<hash>`. `ObjectKey` (engine.KeyAddresser) and
`StoreID` (`dav:<origin><path>/<root>/`, the routing-truth shared-store check)
are implemented.

Each `/`-separated key segment is one WebDAV path segment, `url.PathEscape`d
(spaces, unicode, `%`, `+`, `#`, `?` are fine). Segments WebDAV cannot hold are
mapped reversibly (`davName` / `keySegment`):

| key segment | resource name | why |
|---|---|---|
| `""` (trailing `/` — an S3 folder marker, or `a//b`) | `~` | an empty segment is a collection URL |
| `.` / `..` | `~.` / `~..` | servers normalise them away; `..` would climb out of the tenant's folder |
| `~anything` | `~~anything` | keeps the mapping one-to-one |

**Known limit:** WebDAV cannot hold a file `a` and a folder `a` side by side.
An S3 key `a` and a key `a/b` in the same container conflict — the second PUT
fails (S3 allows both). `photos/` (marker) + `photos/x.jpg` works (the marker
is the file `~` in the folder).

**Request URLs** (CodeQL `go/request-forgery`): the configured URL is parsed
once in the constructor (http/https, a host, no userinfo/query/fragment) and
kept as a `url.URL` holding only scheme + host. Every request URL is a copy of
it with only `Path`/`RawPath` set (`requestURL`); the escaped path must match
what `escapedPath` produces (absolute, `url.PathEscape`d segments — no `?`,
`#`, raw `\`, scheme), may hold no empty, `.` or `..` segment (also between
backslashes, which Windows-backed servers split on), no encoded `/` or NUL,
and must be the root folder or below it (or exactly the server's own folder,
the health check's fallback). The built request's scheme + host are compared
to the configured ones before it is sent. A refusal wraps
`engine.ErrInvalidInput` (no breaker charge). Keys that try to climb
(`..\..\x`, `a\..\b`) are refused; `%2e%2e` is a literal name (`%252e%252e`).

## Operations

| Op | Request | Notes |
|---|---|---|
| `Put` | `PUT` (streamed; `Content-Length` when `PutOptions` carries it, else chunked) | Missing folders: the deepest unknown parent is `MKCOL`ed first (201/405 = exists), on 409 every ancestor top-down; known folders are cached (`sync.Map`, per process). One MKCOL per path at a time in the process (concurrent PUTs into a new folder otherwise race and a server answers the loser **423 Locked** — x/net/webdav does; found by `webdav-bench` parity), and a 423 from another process is retried 5× (100 ms × attempt). A PUT answered 404/409 (a folder removed behind the cache) recreates the folders and is retried once when the body can be rewound or was not read; otherwise the error wraps `engine.ErrNoFailover`. With a known length the stored size is checked by `PROPFIND` Depth 0 `getcontentlength`: a short object is an error ("truncated upload"). Overwrite replaces. |
| `Get` | `GET` | 404 → `engine.NotFoundError` (the engine's miss: no breaker charge), like local/OneDrive |
| `GetRange` | `GET` + `Range: bytes=off-end` | 206 accepted (`Content-Range` start checked); a server that ignores Range and answers **200** has the bytes before the offset discarded and the rest limited — never the wrong bytes; 416 / offset past the end = empty |
| `Delete` | `PROPFIND` Depth 0, then `DELETE` | A WebDAV DELETE of a folder is recursive, so a key that is a folder is left alone (`DeleteObject("a")` never removes `a/b`); 404 = nil |
| `Exists` | `PROPFIND` Depth 0 | Not HEAD (some bridges special-case it); a folder is not an object |
| `List` | `PROPFIND` Depth 1 per folder, recursively | Depth infinity is often disabled. Keys relative to the container, sorted; folders skipped and pruned by the prefix; hrefs may be absolute URLs or paths, percent-encoded any way, with or without the trailing slash; an href outside the folder asked for is an error |
| `WalkTenant` | the same walk of `t-<tenant>/` | `engine.TenantWalker` for the erasure sweep: every file with a `Remove` bound to the listed resource; `""` / `/` ids refused (`checkWalkTenant`); a listing that cannot finish is an error. Empty folders are left behind (names only) |
| `HealthCheck` | authenticated `PROPFIND` Depth 0 of the root (the server URL when the root folder does not exist yet) | A wrong password is a 401 → failure (architecture decision 1: never a bare GET). Probed by `api/backend_probes.go` when `SYNC_WEBDAV_PASSWORD` is set; alert `SyncProbeFailing` (warning, 10 m) in `deploy/monitoring/vaultaire-backends.yml` |

HTTP: `TunedHTTPClient(WithHTTP1Only(), WithResponseHeaderTimeout(5m))`
(`VAULTAIRE_TUNED_TRANSPORT=false` = `http.DefaultClient`); 60 s per
PROPFIND/MKCOL/DELETE. Basic auth on every request; credentials in a URL are
refused (a URL lands in error messages); the password is never logged. 5xx,
timeouts and 401/403 are returned as errors — the engine's breaker handles them.

## Configuration (`sync` instance)

| Env | Default | |
|---|---|---|
| `SYNC_WEBDAV_PASSWORD` | — | **Required to register** the `sync` driver: the bridge's generated password (`sync-webdav credentials`) |
| `SYNC_WEBDAV_URL` | `http://127.0.0.1:4918` | The bridge |
| `SYNC_WEBDAV_USER` | `sync` | |
| `SYNC_WEBDAV_ROOT` | `vaultaire` | Folder under the bridge's root objects live in |

## The Sync.com bridge (`sync-webdav`)

Facts from Sync's guide (help.sync.com, "WebDAV Guide for Linux", 2026-10-02):
the program runs on OUR box, encrypts client-side (end-to-end encryption is
preserved) and serves plain WebDAV at `http://127.0.0.1:4918/` with Basic auth
(user `sync`, a generated password). It accepts connections from localhost only
unless `--allow-from` says otherwise. Uploads are spilled to a temp directory
(`--upload-temp-dir`) while they stream to Sync.

```bash
# as a dedicated system user (sync-webdav), on the box
sudo useradd --system --create-home --home-dir /var/lib/sync-webdav --shell /usr/sbin/nologin sync-webdav
sudo -u sync-webdav -H sh -c 'curl -LsSf https://www10.sync.com/download/webdav/install.sh | sh'
# sign in once (headless: prints a URL, paste the code back); saves the session
sudo -u sync-webdav -H /var/lib/sync-webdav/.sync-webdav/bin/sync-webdav login --headless
# show / create the WebDAV password → SYNC_WEBDAV_PASSWORD in /opt/vaultaire/configs/.env
sudo -u sync-webdav -H /var/lib/sync-webdav/.sync-webdav/bin/sync-webdav credentials
```

Options we use: `--mount <folder>/` scopes the bridge to one folder of the
Sync account (`vault/` = the Vault; default = Files) — use a dedicated folder,
e.g. `--mount stored/`; `--allow-from none` (localhost only, the default — keep
it); `--upload-temp-dir` on a disk with room for the largest object in flight
(a full spill disk is what the size check after each PUT catches);
`--analytics false`; `--log-file`. `--daemon` backgrounds the process — under
systemd run it in the foreground instead.

`/etc/systemd/system/sync-webdav.service`:

```ini
[Unit]
Description=Sync.com WebDAV bridge (encrypted, localhost only) for Vaultaire
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=sync-webdav
Group=sync-webdav
Environment=HOME=/var/lib/sync-webdav
ExecStartPre=/usr/bin/mkdir -p /var/lib/sync-webdav/spool
ExecStart=/var/lib/sync-webdav/.sync-webdav/bin/sync-webdav \
  --port 4918 --mount stored/ --allow-from none \
  --upload-temp-dir /var/lib/sync-webdav/spool \
  --analytics false --log-file /var/lib/sync-webdav/sync-webdav.log
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/sync-webdav

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now sync-webdav
curl -s -u sync:"$SYNC_WEBDAV_PASSWORD" -X PROPFIND -H 'Depth: 0' http://127.0.0.1:4918/ -o /dev/null -w '%{http_code}\n'   # 207
```

Then set `SYNC_WEBDAV_PASSWORD` in `/opt/vaultaire/configs/.env` and restart
Vaultaire: the boot log says "Sync WebDAV driver added", `/health/backends`
shows `sync`. Updating the bridge: re-run the installer, restart the unit
(`sync-webdav update` also exists). `sync-webdav credentials --reset` changes
the password — update the env, or the probe pages 401.

## Other WebDAV services

The same constructor serves them; register a new name in `main.go` (and in
`backend_probes.go`, `engine.targetOnlyBackends` if it must not be a failover
target, `backendToStorageClass`): Hetzner Storage Box
(`https://uXXXXX.your-storagebox.de`), Koofr
(`https://app.koofr.net/dav/Koofr`), pCloud (`https://webdav.pcloud.com`),
Nextcloud (`https://host/remote.php/dav/files/<user>`). A URL path is kept
(the root is below it).

## Tests

`webdav_test.go` — `golang.org/x/net/webdav` (MemFS + MemLS) behind
`httptest` with a Basic-auth check: round trip (multi-MiB streamed body, known
and unknown length), overwrite, nested keys + MKCOL caching, a folder deleted
behind the cache (rewindable retry; a sent stream is `ErrNoFailover`), special
characters (`..`, `.`, empty segments, folder markers, `~`, `%`, `#`, `?`,
unicode), GetRange 206 and a server that ignores Range, not-found mapping,
delete (missing = nil; never a folder), List (prefix, nesting, empty folders,
href shapes, a foreign href), WalkTenant (neighbour tenants, Remove twice, bad
ids, fn error), no tenant → `ErrNoTenant` + chunk context, HealthCheck (ok /
401 / down), a server that truncates a PUT, 5xx returned. `webdav_guard_test.go`
— the request-URL guard (scheme/host fixed, root escapes / `%2e%2e` /
backslash dot segments / query / fragment / encoded `/` refused), hostile keys
end to end (no request off the root or to another host), 32 concurrent PUTs
into new folders (the 423 race), a MKCOL 423 retried. Benchmark:
`cmd/tools/webdav-bench` (cmd/tools/README.md). `webdav_config_test.go`
— env defaults and `StoreID`.
