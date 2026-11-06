package drivers

// The sia.storage account behind s3d has ONE funding budget shared by
// uploads and reads (bench-results/SIA-S3D-2026-10.md): a fast ingest burst
// (~80 MB/s logical, ×2.3–3 on the wire) empties it and starves reads for
// minutes. SIA_READ_BUDGET (sia_read.go) paces what the driver delivers,
// SIA_WRITE_BUDGET (sia_write.go) what it takes in; both are one siaPacer —
// a token bucket in bytes per second, one per driver (one sia driver per
// process).

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// siaBudgetMaxPerSec is the largest budget either variable accepts.
const siaBudgetMaxPerSec = int64(100 << 30)

// siaBudgetFromEnv reads a budget variable: bytes per second as a size such
// as 25MiB; unset, 0 or off = 0 (not paced). A rejected value is a Warn and
// 0 (the default).
func siaBudgetFromEnv(getenv func(string) string, name, example string, logger *zap.Logger) int64 {
	v := strings.TrimSpace(getenv(name))
	if v == "" || v == "0" || strings.EqualFold(v, "off") {
		return 0
	}
	n, err := parseByteSize(v)
	if err != nil || n <= 0 || n > siaBudgetMaxPerSec {
		logger.Warn("invalid "+name+" (need bytes per second such as "+example+", or off), keeping default (not paced)",
			zap.String("value", v))
		return 0
	}
	return n
}

// siaPacer is one token bucket of bytes — the shared pacer (pacer.go). Its
// burst is a quarter of a second of the rate, 32 KiB..4 MiB: a Read never
// takes more than the bucket can ever hold, so every wait is short and a
// stream keeps moving.
type siaPacer = pacer

// newSiaPacer returns the pacer of bytesPerSec, nil when it is ≤ 0; the
// time each take waits is added to wait.
func newSiaPacer(what string, bytesPerSec int64, wait prometheus.Counter) *siaPacer {
	if bytesPerSec <= 0 {
		return nil
	}
	burst := int(min(max(bytesPerSec/4, 32<<10), 4<<20))
	return newPacer(what, float64(bytesPerSec), burst, func(d time.Duration) { wait.Add(d.Seconds()) })
}
