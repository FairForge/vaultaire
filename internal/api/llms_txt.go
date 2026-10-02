package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/api/landing"
)

// /llms.txt — the plain-text summary an AI assistant reads before it writes
// code against Stored. Prices come from the one price file
// (internal/api/landing/prices.json) so this never quotes a retired number
// again (Review R14-04: it said "$3.99/TB" for four months). The route list
// is the one TestOpenAPIRouteInventory prints; /openapi.json is the
// authoritative machine-readable version.
const llmsTxtTemplate = `# Stored API Reference
> S3-compatible object storage, sold as a whole-TB quota at a flat rate:
> Standard $%s/TB/mo annual ($%s monthly), Vault archive $%s/TB/mo annual
> ($%s monthly, $%s monthly minimum), pin-hot add-on $%s/TB/mo. 5 GB free,
> no card. No API, request or retrieval fees; egress is never billed.
> Free egress per month: %s× the Standard quota + %s× the Vault quota.
> %s
> %s
> While rate-limited, more than 16 parallel downloads are answered 503
> SlowDown (retryable). GET /api/v1/user/usage reports egress_allowance,
> egress_used, egress_throttled, egress_rate_limit_bytes_per_sec and
> egress_resets_at.

## Authentication

### S3 API (SigV4)
Endpoint https://stored.ge, region us-east-1, path-style addressing.
AWS Signature Version 4 with the access key + secret shown once at signup
(or a scoped key from Dashboard -> API Keys). Any S3 SDK works: aws-cli,
boto3, rclone, restic, aws-sdk-go, JuiceFS, Cyberduck.
curl: curl --aws-sigv4 "aws:amz:us-east-1:s3" --user "KEY:SECRET" https://stored.ge/bucket/key

### JSON API (Bearer JWT)
POST /auth/register  {"email","password","company"} -> {"accessKeyId","secretAccessKey","endpoint"} (shown once)
POST /auth/login     {"email","password"} -> {"token","tenant_id"} (24 h JWT)
POST /auth/password-reset {"email"}; POST /auth/password-reset/complete {"token","new_password"}
Send the JWT as: Authorization: Bearer <token>

## S3-compatible endpoints (base https://stored.ge)

GET    /                              list buckets
PUT    /<bucket>                      create bucket (optional LocationConstraint = an enabled region)
DELETE /<bucket>                      delete empty bucket
GET    /<bucket>?list-type=2          list objects (prefix, delimiter, continuation-token, max-keys)
PUT    /<bucket>/<key>                upload (streamed; Content-MD5 verified; x-amz-storage-class STANDARD|GLACIER)
GET    /<bucket>/<key>                download (Range, If-Match/If-None-Match/If-(Un)Modified-Since)
HEAD   /<bucket>/<key>                metadata from cache (~1 ms, never touches a backend)
DELETE /<bucket>/<key>                delete; POST /<bucket>?delete batch delete
PUT    /<bucket>/<key>  + x-amz-copy-source   copy
POST   /<bucket>/<key>?uploads        create multipart; PUT ?partNumber=&uploadId= ; POST ?uploadId= complete
Sub-resources: ?versioning ?object-lock ?retention ?legal-hold ?tagging ?cors ?notification ?acl (public-read)

### S3 features
- Presigned URLs (GET and PUT, SigV4 query auth)
- Object Lock: GOVERNANCE and COMPLIANCE retention, legal hold (WORM)
- Versioning: version history and delete markers; retrieving a previous
  version's bytes is not yet available (GET ?versionId=<old> answers 501)
- Scoped API keys (per bucket, per operation, IP allowlist, expiry) and
  STS temporary credentials (POST /api/v1/sts/token)
- Public-read buckets are served from the CDN: GET https://cdn.stored.ge/<slug>/<bucket>/<key>
  (Range, conditional requests, CORS per bucket, Cache-Control from the bucket's cache TTL)

## JSON API (base https://stored.ge/api/v1, Bearer JWT, 100 requests/min per tenant)

Management (/api/v1/manage):
  GET/POST /buckets, GET/PATCH/DELETE /buckets/{name}, GET /buckets/{name}/objects,
  PUT /buckets/{name}/tier, PUT /buckets/{name}/residency,
  GET/POST /keys, DELETE /keys/{id}, GET /usage,
  POST /account/export, GET /account/export/{id}, DELETE /account (30-day grace), POST /account/cancel-deletion
Account (/api/v1/user):
  GET /, GET /quota, GET /quota/history, GET /usage, GET /usage/alerts, GET /presigned,
  GET/POST /apikeys, DELETE /apikeys/{id}, POST /apikeys/{id}/rotate, POST /apikeys/{id}/expire, GET /apikeys/audit
Webhooks and events: GET/POST /api/v1/webhooks, PATCH/DELETE /webhooks/{id},
  GET /webhooks/{id}/deliveries, POST /webhooks/{id}/test, GET /api/v1/events
STS: POST /api/v1/sts/token {"duration_seconds","permissions","bucket_scope","parent_key_id"}

Conventions: single object {"object": "...", ..., "request_id"}; lists
{"object":"list","data":[...],"has_more":bool,"next_cursor":"..."} with ?limit=&starting_after=;
errors {"error":{"type","code","message","request_id"}}; Idempotency-Key header on mutations (24 h);
X-RateLimit-Limit / -Remaining / -Reset headers.

## Health and status
GET /health          JSON service health (always 200 while serving; backend counts in the body)
GET /health/live     process liveness    GET /health/ready  readiness
GET /status          HTML status page    GET /version       build version

## Docs
GET /docs            guides hub (getting started, rclone, FAQ)
GET /docs/api        interactive API reference (Swagger UI)
GET /openapi.json    OpenAPI 3.0 specification (the authoritative route list)
GET /changelog       what changed, dated
GET /llms.txt        this file
`

// llmsTxtBody renders the file from the price file once per process.
func llmsTxtBody() string {
	p := landing.Get()
	money := func(v float64) string { return strings.TrimSuffix(fmt.Sprintf("%.2f", v), ".00") }
	ratio := func(v float64) string {
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
	}
	return fmt.Sprintf(llmsTxtTemplate,
		money(p.Standard.Annual), money(p.Standard.Monthly),
		money(p.Vault.Annual), money(p.Vault.Monthly), money(p.Vault.MonthlyMinimum),
		money(p.PinHot), ratio(p.Egress.StandardFreeRatio), ratio(p.Egress.VaultRestoreFreeRatio),
		p.Egress.PastAllowance, p.Egress.OneAllowance)
}

func (s *Server) handleLlmsTxt(w http.ResponseWriter, r *http.Request) {
	body := llmsTxtBody()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write([]byte(body))
}
