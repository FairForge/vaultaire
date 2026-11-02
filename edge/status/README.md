# stored-status — Stored's status page (WP-DOMAIN-1 part 3B)

A Cloudflare Worker on `status.stored.cloud` (contract rule 11; DEVELOPER
charter, "The first ten" 2). Its one rule: **it never asks the origin
anything.** The box pushes its state every minute and the page shows the last
report it got. When the reports stop, the page says so under a red banner
instead of claiming health, so an outage of SLC — or of Cloudflare → SLC — is
visible here.

- `GET /`: the page. It shows the overall verdict (All systems operational /
  Degraded / Partial outage / No recent report), one row per component, the
  names of the alerts firing (names only), the build sha and when the service
  was last heard. It refreshes itself every 60 s with a meta refresh: no
  script.
- `GET /state.json`: the same as data (`verdict`, `stale`, `last_heard`,
  `age_seconds`, `build`, `components`, `alerts`).
- `POST /push`: the box's report. It needs `Authorization: Bearer <STATUS_PUSH_TOKEN>`
  (compared in constant time) and a body ≤ 16 KiB:
  `{"v":1,"at":<unix s>,"build":"<sha>","components":[{"id","name","state":"up|degraded|down","detail"}],"alerts":["AlertName"]}`.
  The answers are 204 stored, 400 refused, 401 wrong token, 409 not newer than
  the stored report, and 503 when the secret is not set. A report whose `at`
  is more than 10 min from the Worker's clock is refused, so a captured report
  cannot be replayed later.
- Everything else is a 404. Every answer is `no-store`, sets no cookie, and
  carries a CSP of `default-src 'none'`, so nothing can load: no beacon, no
  font, no image.
- A report is **stale** once it is older than `STALE_AFTER_SECONDS` (300). The
  Worker's own receive time decides this, never the box's clock.

State is one Durable Object (`StatusState`, SQLite storage), not KV. A report
must read back exactly as written from every colo, and the deploy token (the
stored.cloud zone token) can upload scripts but cannot create a KV namespace.

The pusher is `deploy/scripts/status-push.sh` (timer
`vaultaire-status-push.timer`, every 60 s; rule `StatusPushStale`). Install:
`docs/DEPLOY.md` § Status page.

```bash
node --test                              # make test-edge runs it in CI
bash test/dev-test.sh                    # the real runtime: wrangler dev --local
# Deploy (PRODUCTION account, the zone token):
CLOUDFLARE_API_TOKEN=… CLOUDFLARE_ACCOUNT_ID=… npx -y wrangler@4.151.3 deploy
printf %s "$TOKEN" | CLOUDFLARE_API_TOKEN=… CLOUDFLARE_ACCOUNT_ID=… npx -y wrangler@4.151.3 secret put STATUS_PUSH_TOKEN
```

DNS: `AAAA status.stored.cloud 100::`, proxied. This is edge/mcp's shape: the
route `status.stored.cloud/*` answers and the host never reaches SLC. Its name
is in no origin certificate.
