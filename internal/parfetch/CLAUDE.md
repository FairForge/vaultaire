# internal/parfetch

Ordered, windowed, hedged parallel fetch of one logical stream (2026-10-10,
download diagnosis 2026-10-09). No I/O of its own: the caller supplies
`FetchFunc(ctx, i) ([]byte, error)` and the part sizes.

- `Start(ctx, Config, sizes, fetch) *Stream` — a dispatcher launches parts while
  the window has room: `Window` bytes and `MaxParts` parts held (a part is held
  from fetch start until the caller asks for the next one), past `FreeParts`
  parts each also needs `Budget` (process-wide; `TryAcquire` never waits — a
  stream blocked on it waits for its own release or a 10 ms poll). A part
  larger than the window still runs alone, so a stream always progresses.
- Hedging: a part not done after max(`HedgeAfter`, `HedgeFactor` (3) × the
  median of the last 32 part times) gets a second attempt (≤ `MaxHedges` (8)
  per stream at once); a failed first attempt gets one retry the same way.
  First success wins, the other attempt's context is cancelled. Median, not
  mean: the tail would inflate a mean and stop the hedging it is for.
- `Next()` returns parts strictly in order (`io.EOF` after the last; errors
  sticky). `Close()` cancels, waits for every goroutine, returns all budget.
- `Reader` adapts a stream to `io.ReadCloser` + `io.WriterTo`; a part failure
  is a read error, never a clean EOF.
- `Hooks` feed metrics (`Held`, `Hedged(reason)`, `HedgeWon`).

Used by `internal/api/s3_large_get.go` (chunked GET + `parallel_get` ranged
GET). Tests: `stream_test.go` (order under out-of-order completion, hedge
cancels the loser, window/count/budget bounds, Close/cancel leak checks).
