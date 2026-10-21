# internal/payments/btcpay

The BTCPay Server provider (WP-PAY-4, Stage 1a Prompt 24; design `docs/PAYMENTS.md` § 3, the lab's facts in § 3.2 "Verified" and the § 3.3 rows marked **Lab**). Imports `internal/payments` (and `audit`), never a provider SDK; nothing in `internal/dashboard` imports it.

## The lab (2026-10-21)
BTCPay 2.4.5 / NBXplorer 2.6.19 / bitcoind 31.1 on regtest in colima — every § 3.3 row driven with `bitcoin-cli`, 84 deliveries captured. **`testdata/` is the captures, never hand-written JSON:** `<Event>_<case>.json` = one delivery `{headers, body_b64 (the raw bytes the signature covers), body}`, `<Event>_<case>.invoice.json` / `.payment-methods.json` = what Greenfield answered right after it; `all-deliveries/` = every capture in order; `SECRET.txt` = the throwaway webhook secret of the deleted lab store; error answers `get_invoice_unknown.403.json`, `get_store_scoped.403.json`, `bad_key.401.json`, `health_{ok,nbxplorer_down}.200.json`, `create_invoice_bad_speed.422.json`, `refund_ratethen.response.json`, `reorg_after.*`, `snapshot_double_spend_reverted_to_new.*`.

## Files
| File | What |
|---|---|
| `client.go`, `api.go`, `transport.go` | The Greenfield client: `Authorization: token <key>` (store-scoped: cancreateinvoice, canviewinvoices, canmodifyinvoices), 10 s per call on a small tuned transport (the drivers' `TunedHTTPClient` knobs copied, not imported), **one retry on a fresh connection for a GET that got no answer — never a POST**, redirects never followed, a 1 MiB answer cap; the key and bodies never logged. `CreateInvoice`, `GetInvoice`, `GetPaymentMethods`, `MarkStatus` (the store path), `CreateRefund` (needs a 4th permission, `cancreatenonapprovedpullpayments` — unused by the provider), `Health` (the body's `synchronized` decides), `ProbeInvoices` (`GET …/invoices?take=1`: the scoped key cannot GET the store). `IsUnknownInvoice` = 403 `missing-permission` (BTCPay 2.4 has no 404 for an invoice it does not know) or 404; a 401 is not |
| `sig.go` | `VerifySig(secret, header, body)`: `sha256=` + lowercase hex HMAC-SHA256 over the raw body, decoded into a fixed 32-byte buffer and compared with `hmac.Equal` — one combined verdict, no early return on the header's shape (`TestBTCPaySig_ConstantTime` guards the source); empty secret verifies nothing |
| `event.go` | `Event` (common fields + the informative flags), `DedupKey` = `originalDeliveryId` (always set — equal to `deliveryId` on a first delivery), `ReadBody` (1 MiB `MaxBytesReader`, 413), `ParseEvent` |
| `money.go` | decimal strings ↔ cents (`big.Rat`, half up once) |

`payments.ClaimEvent` (`internal/payments/claim.go`) is the dedup contract on `payment_events (provider, event_id)`, mirroring billing's `claimEvent`: the claim tx held across the handler, rolled back on a retryable error, the tenant written on commit.
