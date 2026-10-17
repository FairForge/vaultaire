package api

// WP-R3-5, the rest (Stage 1a Prompt 8, Part C): where multipart parts are
// staged. MULTIPART_STAGING_DIR, default /tmp/vaultaire-multipart (today's
// directory: nothing changes until the operator sets it). On prod /tmp is a
// 61 GiB tmpfs — RAM, wiped by systemd-tmpfiles at boot, shared with the
// Sync stripe staging and the pack store — so the operator's choice is a
// directory on the NVMe data disk, /opt/vaultaire/data/multipart.
//
// One directory per process, set once at boot (NewServer) before the first
// request: every staging path (multipartDir, partFilePath), the free-space
// guard (multipart_staging_guard.go) and the reaper's orphan sweep use it.
// A staged part survives a slot switch only when both slots see the same
// directory: they share the unit's EnvironmentFile and run as the same
// user, so they do (docs/DEPLOY.md). Upload ids are still checked by
// validUploadID before any path is built from them.

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"
)

var multipartStagingRoot atomic.Pointer[string]

// multipartStagingDir is the directory parts are staged under.
func multipartStagingDir() string {
	if p := multipartStagingRoot.Load(); p != nil {
		return *p
	}
	return defaultMultipartStagingDir
}

func setMultipartStagingDir(dir string) { multipartStagingRoot.Store(&dir) }

// stagingDirFromEnv is MULTIPART_STAGING_DIR, validated: an absolute path
// that is (or can be made, 0700) a directory this process can write into.
// Unset = the default, created when the first part arrives as before. A
// rejected value is logged at Warn and the default kept (the R13-19
// convention) — never a boot failure, never a part staged where it cannot be.
func stagingDirFromEnv(getenv func(string) string, logger *zap.Logger) string {
	raw := strings.TrimSpace(getenv("MULTIPART_STAGING_DIR"))
	if raw == "" {
		return defaultMultipartStagingDir
	}
	reject := func(why string, err error) string {
		logger.Warn("MULTIPART_STAGING_DIR rejected; parts stage in the default directory",
			zap.String("value", raw), zap.String("reason", why), zap.Error(err), zap.String("default", defaultMultipartStagingDir))
		return defaultMultipartStagingDir
	}
	if !filepath.IsAbs(raw) {
		return reject("not an absolute path", nil)
	}
	dir := filepath.Clean(raw)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return reject("cannot be created", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return reject("cannot be read", err)
	}
	if !info.IsDir() {
		return reject("not a directory", nil)
	}
	probe, err := os.CreateTemp(dir, ".vaultaire-staging-probe-*")
	if err != nil {
		return reject("not writable", err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return dir
}

// configureMultipartStaging sets the process's staging directory from the
// environment and names it, with its filesystem's free bytes, once.
func configureMultipartStaging(getenv func(string) string, logger *zap.Logger) {
	dir := stagingDirFromEnv(getenv, logger)
	setMultipartStagingDir(dir)
	measured := dir
	if _, err := os.Stat(measured); err != nil {
		measured = os.TempDir() // the default before its first part
	}
	free, total, err := stagingFS(measured)
	if err != nil {
		logger.Warn("multipart staging: free space unreadable", zap.String("dir", dir), zap.Error(err))
		return
	}
	logger.Info("multipart staging",
		zap.String("dir", dir), zap.Bool("default", dir == defaultMultipartStagingDir),
		zap.Uint64("free_bytes", free), zap.Uint64("total_bytes", total))
}
