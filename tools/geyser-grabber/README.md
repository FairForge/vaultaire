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
| Shell env | `export GEYSER_ACCESS_TOKEN/_USER_ID/_COOKIE` for `.env.bench` |
| Vail API test | ready curl testing whether the console token is a Vail JWT (200 = full control plane) |
| Console API calls | cookie-authenticated curls for `/api/buckets`, `/api/keys`, `/api/invoices`, `/api/tapeCollections` |
| Discovered endpoints | every `/api/` or `/sl/` URL this page actually fetched, via resource timing — API discovery without DevTools |
| Access keys / tables | rendered table contents (the S3 keypairs on `/access-keys`) |
| Key-shaped strings | 20-char IDs and 40-char secrets found in the page text |
| Storage + cookies | full localStorage/sessionStorage dump and raw cookie JSON |

"Copy everything as one report" concatenates all blocks.

## Scope and safety

- Host permissions are limited to `geyserdata.com`; it cannot read any other site.
- Everything runs locally in the popup. Nothing is transmitted anywhere.
- Output contains **live credentials** — treat a copied report like a password.
  Session tokens expire; `geyser_admin.go`'s `StartKeepalive()` holds one open
  once you've pasted it into the env.
