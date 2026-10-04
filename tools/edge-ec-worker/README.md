# edge-ec-worker — k-of-n erasure reassembly at the Cloudflare edge (bench, 2026-10-04)

Not part of the product. A Cloudflare Worker that rebuilds an object from
erasure shards spread over vendors — the read plane the plan calls Phase
40.1(d) / Phase 11 — measured in `bench-results/VAULTAIRE-PATH-2026-10-04.md`
§10.2. Not Go, so it lives under `tools/` like `geyser-grabber`.

- `codec/` — Rust → `wasm32-unknown-unknown`, plain C ABI (no bindgen):
  `ec_alloc`, `ec_free`, `rs_decode`, `rs_reconstruct` (in place, so the
  contiguous data slots stream straight out of wasm memory),
  `rq_decode` (RFC 6330 via the `raptorq` crate; symbols dealt round-robin to
  the n shards, the layout `cmd/tools/erasure-bench -codec raptorq` writes).
  Build: `rustup target add wasm32-unknown-unknown && cargo build --release
  --target wasm32-unknown-unknown`, copy `eccodec.wasm` next to `index.js`.
- `worker/` — `wrangler deploy` with `CLOUDFLARE_API_TOKEN` /
  `CLOUDFLARE_ACCOUNT_ID` in the environment. Binds an R2 bucket that holds
  manifests (`manifests/<name>.json`: k, m, shard_len, size, codec, symbol,
  shards[] with presigned `url`s or an `r2key`) and the R2 shards.
  Routes: `/ec/<manifest>?mode=whole|stripe&cache=1&lose=<vendor>&drop=<slots>`,
  `/echo` (POST, upload probe), `/relay?to=` and `/relaygen?mb=&to=` (upload
  probes to a presigned PUT), `/r2/<key>`, `/ping`.

Limits found: the 128 MB isolate bounds a whole-block decode at ~64 MiB for RS
and ~16 MiB for RaptorQ; above that the shards must be stripe-interleaved so
`mode=stripe` can decode 1 MiB stripes (the erasure bench writes contiguous
klauspost shards, for which only `mode=whole` is correct). `Date.now()` is
frozen during CPU work in Workers — time decodes from the client.
