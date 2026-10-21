package drivers

// Writes to the `sia` driver — SIA_WRITE_BUDGET (sia_budget.go has why).
//
// s3d answers a PUT once the object is on its local disk and uploads to Sia
// hosts from a background loop, as fast as the account's funding allows: a
// fast ingest burst empties the budget the reads share. s3d cannot upload
// faster than bytes arrive, so pacing what the driver takes in bounds the
// account's upload spend on average to the budget (s3d's local buffer
// smooths it, it never raises it).
//
// WHERE it paces — the driver's input body, i.e. back-pressure on the
// source, never the request to s3d:
//
//   - Put wraps the body it is given in the token bucket before the
//     uploader reads it. The uploader reads a part (≤ 16 MiB) into its
//     buffer, THEN sends it as a seekable request; so the paced wait
//     happens before any request to s3d starts, and the per-size PUT
//     deadline (FIXED_BUCKET_PUT_TIMEOUT, put_deadline.go) and its one
//     fresh-connection retry only ever time s3d itself — pacing can never
//     look like a stalled s3d. (Pacing the request body instead would run
//     the deadline on our own wait, and a retry would re-send paced bytes.)
//   - For a PutObject the source is the client's request body (clientBody,
//     WP-R2-3): the client's TCP send window fills and it sends at the
//     budget. The server has no body ReadTimeout (server.go), and the sia
//     driver has no minimum source rate — SYNC_WEBDAV_SOURCE_MIN_RATE is the
//     WebDAV driver's — so a paced upload is never a 408 RequestTimeout or a
//     400 IncompleteBody: a wait is one Read's share of the bucket (a
//     quarter-second burst at most per Read, times the uploads queued on
//     it), far below a client's or HAProxy's idle timeout. Only the request
//     context ending (the client gone) stops a wait, and that is the
//     caller's error as for any body (clientBodyFault).
//   - For CompleteMultipartUpload the source is the staged parts (they
//     arrived at the client's speed, the UploadParts are not paced): the
//     Complete takes size / budget, under its 200 + whitespace keep-alive
//     (s3_long_op.go). A slot that stops cuts it at LONG_OP_DRAIN_BOUND like
//     any long Complete. Every other write that reaches the sia driver
//     (a copy, a generated object, the re-encrypt job) goes through Put
//     and is paced the same — Put is the driver's only write path.
//
// Retries the SDK or the PUT deadline make re-send a buffered part and do
// not take tokens again (rare; a stall, not a budget spend). Every other
// driver's Put is unchanged (d.siaWrite is nil there).
//
// Metrics: vaultaire_sia_write_budget_bytes_per_second (0 = off),
// vaultaire_sia_write_budget_wait_seconds_total.

import (
	"context"
	"io"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

var (
	siaWriteBudgetRate = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_sia_write_budget_bytes_per_second",
		Help: "The pace of the bytes the sia driver takes in for a write (SIA_WRITE_BUDGET); 0 = not paced.",
	})
	siaWriteBudgetWait = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_sia_write_budget_wait_seconds_total",
		Help: "Seconds sia writes waited on the SIA_WRITE_BUDGET token bucket (back-pressure on the source body).",
	})
)

// siaWriteBudgetFromEnv reads SIA_WRITE_BUDGET (bytes per second such as
// 40MiB; unset/0/off = none). A rejected value is a Warn and not paced.
func siaWriteBudgetFromEnv(getenv func(string) string, logger *zap.Logger) int64 {
	return siaBudgetFromEnv(getenv, "SIA_WRITE_BUDGET", "40MiB", logger)
}

// setSiaWrite installs (or replaces) the sia write pacing; ≤ 0 = none.
func (d *IDriveDriver) setSiaWrite(bytesPerSec int64) {
	d.siaWrite = newSiaPacer("sia write budget", bytesPerSec, siaWriteBudgetWait)
	siaWriteBudgetRate.Set(float64(max(bytesPerSec, 0)))
}

// pacedSource is the body Put uploads: data itself on every driver but a
// paced sia driver.
func (d *IDriveDriver) pacedSource(ctx context.Context, data io.Reader) io.Reader {
	if d.siaWrite == nil || data == nil {
		return data
	}
	return d.siaWrite.reader(ctx, data)
}
