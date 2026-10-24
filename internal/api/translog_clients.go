package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/clientbuilds"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// Client build entries (K4c-3b, Stage 1a Prompt 36; plan 43.6: "client build
// hashes … clients refuse an unlisted build"; docs/TRANSPARENCY.md § Client
// builds). The reviewed manifest (internal/translog/clientbuilds) is embedded
// in the binary; at boot every row not yet in the log is appended — one
// client_build entry per distinct (client, sha256), like server_build — and
// a row the log refuses (frozen behind the high-water file, the database
// down) stays pending here: every translog_checkpoint run tries it again,
// BEFORE it checkpoints, until it lands (RP42-1's rule).

// clientBuildsTimeout bounds the boot pass (the manifest is embedded; the
// appends are the only I/O).
const clientBuildsTimeout = time.Minute

// clientBuilds is the manifest's rows until each is in the log.
type clientBuilds struct {
	mu      sync.Mutex
	pending []translog.ClientBuild
}

// set keeps rows as the entries to append.
func (c *clientBuilds) set(rows []translog.ClientBuild) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append([]translog.ClientBuild(nil), rows...)
}

// Pending reports whether any row is still waiting for the log.
func (c *clientBuilds) Pending() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending) > 0
}

// try appends every pending row; one that lands (or is already in the log)
// is dropped, one the log refuses is kept for the next try. retried says
// whether this is the translog_checkpoint job's retry (for the log line).
func (c *clientBuilds) try(ctx context.Context, store *logstore.Store, logger *zap.Logger, retried bool) {
	if c == nil || store == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var kept []translog.ClientBuild
	for _, b := range c.pending {
		idx, appended, err := store.Append(ctx, b)
		switch {
		case errors.Is(err, logstore.ErrLogFrozen):
			logger.Warn("translog: client build waits — the log is frozen; the translog_checkpoint job retries it",
				zap.Error(err), zap.String("client", b.Client), zap.String("sha256", b.SHA256), zap.Bool("retried", retried))
			kept = append(kept, b)
		case err != nil:
			logger.Error("translog: client build not appended; the translog_checkpoint job retries it",
				zap.Error(err), zap.String("client", b.Client), zap.String("sha256", b.SHA256), zap.Bool("retried", retried))
			kept = append(kept, b)
		case appended:
			logger.Info("translog: client build appended", zap.Int64("index", idx),
				zap.String("client", b.Client), zap.String("version", b.Version), zap.String("sha256", b.SHA256), zap.Bool("retried", retried))
		default:
			logger.Info("translog: client build already in the log", zap.Int64("index", idx),
				zap.String("client", b.Client), zap.String("version", b.Version))
		}
	}
	c.pending = kept
}

// startClientBuildEntries loads the embedded manifest and appends its rows in
// the background (never blocks the boot); what the log refuses stays pending
// for the translog_checkpoint job. A manifest that does not parse is an Error
// line and nothing is appended — the manifest is also proven in CI
// (TestClientBuildManifest_Valid) and gated in the Deploy job
// (scripts/repro-check-clients.sh), so this is belt and braces.
func (s *Server) startClientBuildEntries() {
	if s.translog == nil || s.translogClients == nil {
		return
	}
	logger := s.log()
	rows, err := clientbuilds.Load()
	if err != nil {
		logstore.CountAppendFailure(translog.KindClientBuild)
		logger.Error("translog: client builds not appended: the manifest does not parse", zap.Error(err))
		return
	}
	s.translogClients.set(rows)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), clientBuildsTimeout)
		defer cancel()
		s.translogClients.try(ctx, s.translog, logger, false)
	}()
}
