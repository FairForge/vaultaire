# internal/clientip

Single source of truth for "what is the client's IP" — the only place allowed to
read `X-Forwarded-For` / `CF-Connecting-IP`. Feeds the API-key IP allowlist
(`api/s3.go`), the S3 access log, the waitlist limiter and the dashboard
login/reset/abuse limiters (`dashboard/middleware.ClientIP`).

**Trust model** (from the SLC `haproxy.cfg`, `option forwardfor` = append):
peer = **last** entry of the **last** `X-Forwarded-For` header occurrence (HAProxy adds a new
occurrence rather than merging — its manual says use the last occurrence only; Go's `Header.Get`
returns the first, so always `Header.Values`) or `RemoteAddr`; `CF-Connecting-IP`
is used only when that peer is a Cloudflare edge and the header occurs exactly once (`cloudflareRanges`, published
list snapshot — refresh deliberately; the test pins the count). Everything the
client can write (earlier XFF entries, a CF header on the grey-cloud
`s3.stored.ge` endpoint) is ignored. Review R1-01 explains the bypass this closes.

Do not add per-caller header parsing elsewhere; extend `FromRequest` instead.
