# uloz-bench

Standalone benchmark for [Uloz.to](https://uloz.to) cloud storage via their public REST API and resumable upload CDN.

**Not S3-compatible** — uses uloz.to's proprietary chunked upload API (`upload-resumable.greencdn.io`) and REST endpoints (`apis.uloz.to`).

## Usage

```bash
export ULOZ_LOGIN=username
export ULOZ_AUTH_TOKEN=app_token   # Settings → Application tokens
go run ./cmd/uloz-bench            # full suite (~5 min)
go run ./cmd/uloz-bench -smoke     # quick: auth + 4MB round-trip
```

## Workloads

| Name | What it does |
|------|-------------|
| `auth_latency` | 5x POST /v5/auth/token |
| `upload_4mb_chunk4` | 4MB file, 1x 4MB chunk |
| `upload_32mb_chunk4` | 32MB file, 8x 4MB chunks |
| `upload_32mb_chunk32` | 32MB file, 1x 32MB chunk |
| `upload_128mb_chunk128` | 128MB file, 1x 128MB chunk |
| `download_4mb` | Download 4MB via download-link API |
| `download_128mb` | Download 128MB via download-link API |
| `integrity_4mb` | Upload + download + SHA256 verify |
| `list_folder` | 5x list root folder |
| `cleanup` | Delete all bench files |

## Upload pipeline

Each upload goes through 6 API calls: `upload/link` → register → chunk POST(s) → status poll → `file-list/private` PATCH → `upload-batch` commit.

## API reference

- Upload API: https://uloz.to/upload-resumable-api-beta
- Public API: https://uloz.to/apidoc/public (ReDoc, spec at `apis.uloz.to/v5/open-api/public`)
- Chunk sizes: exactly 4MB, 32MB, or 128MB
- CAPTCHA may be required for uploads from outside CZ/SK

## Environment

| Variable | Required | Purpose |
|----------|----------|---------|
| `ULOZ_LOGIN` | Yes | uloz.to username |
| `ULOZ_AUTH_TOKEN` | Yes | Application login token |
| `ULOZ_API_KEY` | No | API key (defaults to public test key) |
| `ULOZ_API_HOST` | No | API hostname (default: `apis.uloz.to`) |
