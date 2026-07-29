# Geyser Console Grabber (Chrome extension)

One-click extraction of everything needed to drive Geyser's console API and the
underlying Spectra Vail management API from a shell.

Geyser (Spectra Logic Vail) exposes three API layers; only the S3 data plane
accepts our S3 keys. The console API and the Vail management API
(`la1.geyserdata.com/sl/api/`) authenticate with a browser session instead, and
`internal/drivers/geyser_admin.go` expects `GEYSER_ACCESS_TOKEN` /
`GEYSER_USER_ID` to be lifted out of that session by hand. This extension does
that lift, and also fingerprints the console's API surface while it's there.

## Install

1. `chrome://extensions` → enable **Developer mode** (top right)
2. **Load unpacked** → select this directory
3. Pin it, log in to `console.geyserdata.com`, click the icon

## What it collects

| Block | Contents |
|---|---|
| Endpoint probe | GETs ~18 candidate `/api/*` paths **from inside the page**, so whatever auth the app uses applies automatically — status + response snippet each |
| Captured auth headers | hooks `fetch`/`XHR` and records the app's own outgoing auth. `/api/keepalive` fires on a timer, so reopen the popup ~30s later to see the real scheme |
| IndexedDB | full dump — where SPAs commonly park tokens |
| Discovered endpoints | every `/api/` or `/sl/` URL the page actually fetched, via resource timing |
| Tables / grids | rendered `<table>` **and** div/aria grid contents (the console's access-key list is not a real table) |
| Key-shaped strings | 20-char IDs and 40-char secrets found in page text |
| Shell env + cookies | `GEYSER_COOKIE` export where applicable, plus raw cookie JSON |
| Storage dump | localStorage / sessionStorage |

**Finding (2026-07-29):** the console does *not* authenticate with readable
cookies — the only cookies on `geyserdata.com` are Wix cookies from the
marketing site. The `accessToken`/`userId` cookie flow documented in
`internal/drivers/geyser_admin.go` is therefore **stale**. Hence the
probe-and-capture approach above rather than credential extraction.

"Copy everything as one report" concatenates all blocks.

## Scope and safety

- Host permissions are limited to `geyserdata.com`; it cannot read any other site.
- Everything runs locally in the popup. Nothing is transmitted anywhere.
- Output contains **live credentials** — treat a copied report like a password.
  Session tokens expire; `geyser_admin.go`'s `StartKeepalive()` holds one open
  once you've pasted it into the env.
