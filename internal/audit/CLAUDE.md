# internal/audit

The operator audit trail (`audit_logs`, migration 008). Package comment in `audit.go` says why it exists (Review R11: nothing wrote the table before).

- `Record(ctx, db, e)` — best effort: detached from the request's cancellation (3 s), an insert that fails is logged and dropped, never fails the caller. The default for every service-layer mutation.
- `RecordTx(ctx, tx, e) error` — the row inside the caller's transaction, the error returned: the row commits with the change it records, or neither does. Used where losing the row is not acceptable: the bucket mode door (`modes.SetBucket`, `bucket.mode_changed`, RP33-4).
- Both read the client IP and user agent from `WithRequest` (stashed by the server's first middleware from `internal/clientip`) and the actor from `WithActor` (JWT / dashboard session), else the subject (`UserID`). `EventType` defaults to the part of `Action` before the first dot. `List` is the keyset-paged reader (admin API, dashboard `/admin/audit`).
