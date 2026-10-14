package api

// WP-R10-8 Part C — the minimal free-space guard of WP-R3-5. Part
// reservations bound each tenant, not the sum across tenants: N free
// accounts × the free quota still land on the hub's staging filesystem. An
// UploadPart whose bytes would leave that filesystem with less than
// MULTIPART_STAGING_MIN_FREE free answers 503 SlowDown + Retry-After before
// a byte is staged or reserved (vaultaire_multipart_staging_refusals_total;
// rule deploy/monitoring/vaultaire-multipart.yml).
//
// The default is max(10 %, min(50 GiB, 50 %)) of the filesystem: the
// prompt's max(10 %, 50 GiB) on any filesystem of at least 100 GiB, but on
// prod the staging directory is /tmp, a 61 GiB tmpfs — RAM, half the box's
// 121 GiB — where a 50 GiB floor would leave 11 GiB for every upload in
// flight; there the floor is 30 GiB (half of it kept free). Moving the
// staging directory onto the 3.4 TB data disk is the rest of WP-R3-5.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

var (
	multipartStagingRefusals = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vaultaire_multipart_staging_refusals_total",
		Help: "UploadParts answered 503 SlowDown because the staging filesystem would fall below MULTIPART_STAGING_MIN_FREE (nothing staged, nothing reserved).",
	})
	multipartStagingFree = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_multipart_staging_free_bytes",
		Help: "Free bytes on the multipart staging filesystem at the last UploadPart's check.",
	})
)

// stagingMinFree is how much of a filesystem of total bytes must stay free.
type stagingMinFree func(total uint64) uint64

// defaultStagingMinFree is max(10 %, min(50 GiB, 50 %)) of the filesystem.
func defaultStagingMinFree(total uint64) uint64 {
	const fifty = 50 << 30
	floor := min(uint64(fifty), total/2)
	return max(total/10, floor)
}

// stagingMinFreeFromEnv is MULTIPART_STAGING_MIN_FREE: unset = the default
// rule; "off" or "0" = no guard (nil); "N%" = that share of the filesystem;
// a size ("50GiB", "512MiB", bytes). A rejected value is logged at Warn and
// the default kept.
func stagingMinFreeFromEnv(getenv func(string) string, logger *zap.Logger) stagingMinFree {
	raw := strings.TrimSpace(getenv("MULTIPART_STAGING_MIN_FREE"))
	switch strings.ToLower(raw) {
	case "":
		return defaultStagingMinFree
	case "off", "0":
		return nil
	}
	if pct, ok := strings.CutSuffix(raw, "%"); ok {
		if p, err := strconv.ParseFloat(pct, 64); err == nil && p > 0 && p < 100 {
			return func(total uint64) uint64 { return uint64(float64(total) * p / 100) }
		}
	} else if n, err := parseByteSize(raw); err == nil && n > 0 {
		return func(uint64) uint64 { return uint64(n) }
	}
	logger.Warn("MULTIPART_STAGING_MIN_FREE rejected; default kept", zap.String("value", raw))
	return defaultStagingMinFree
}

// parseByteSize parses "123", "64KiB", "512MiB", "50GiB", "1TiB".
func parseByteSize(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}}
	for _, u := range units {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("size %q: %w", s, err)
			}
			return n * u.mult, nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// stagingFS reports the free and total bytes of the filesystem holding dir.
var stagingFS = func(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize) // #nosec G115 -- a block size is positive
	return st.Bavail * bsize, st.Blocks * bsize, nil
}

// stagingWouldOverfill reports whether staging n more bytes would leave the
// staging filesystem below its floor. A filesystem that cannot be read is
// not refused (logged): the guard is a backstop, never the outage.
func (s *Server) stagingWouldOverfill(n int64) bool {
	if s.multipartStagingMinFree == nil {
		return false
	}
	dir := multipartTempBase
	if _, err := os.Stat(dir); err != nil {
		dir = os.TempDir()
	}
	free, total, err := stagingFS(dir)
	if err != nil {
		s.log().Warn("multipart staging: free space unreadable; part accepted", zap.Error(err))
		return false
	}
	multipartStagingFree.Set(float64(free))
	if n < 0 {
		n = 0
	}
	floor := s.multipartStagingMinFree(total)
	if free < uint64(n) || free-uint64(n) < floor {
		multipartStagingRefusals.Inc()
		s.log().Warn("multipart staging: the filesystem would fall below its floor — UploadPart answered 503",
			zap.Uint64("free", free), zap.Int64("part", n), zap.Uint64("min_free", floor))
		return true
	}
	return false
}
