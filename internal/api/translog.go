package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// The transparency log at the server (keystone K4, plan Phase 43.6,
// docs/TRANSPARENCY.md): the signing key at boot, the store, and the
// translog_checkpoint job. The producers (server builds, ceremonies) and the
// read side are their own files.

const translogCheckpointJobName = "translog_checkpoint"

// translogSigningKeyOrFatal loads the checkpoint signing key
// (translog.LoadSigningKey: the systemd credential translog_signing_key,
// TRANSLOG_SIGNING_KEY_FILE, then TRANSLOG_SIGNING_KEY) and stops the process
// when a source is set but unusable — the master key's rule. One Info line
// names the source and the key ID (public: it is in every signature), never
// the key; a lower source also set is a Warn. No source = nil: entries still
// append, no checkpoint is signed, vaultaire_translog_signing_key_loaded 0.
func translogSigningKeyOrFatal(logger *zap.Logger, getenv func(string) string, readFile func(string) ([]byte, error)) note.Signer {
	sk, err := translog.LoadSigningKey(getenv, readFile)
	if err != nil {
		logstore.SetSigningKeyLoaded(false)
		logger.Fatal("translog signing key: refusing to start", zap.Error(err))
		return nil
	}
	if sk.Signer == nil {
		logstore.SetSigningKeyLoaded(false)
		logger.Info("translog signing key: none — entries append, no checkpoint is signed")
		return nil
	}
	logstore.SetSigningKeyLoaded(true)
	logger.Info("translog signing key: "+sk.Source, zap.String("source", sk.Source),
		zap.String("key_id", fmt.Sprintf("%08x", sk.Signer.KeyHash())), zap.String("origin", translog.Origin))
	if len(sk.Ignored) > 0 {
		logger.Warn("translog signing key: lower-precedence sources are also set and ignored — remove them (docs/TRANSPARENCY.md)",
			zap.Strings("ignored", sk.Ignored))
	}
	return sk.Signer
}

// defaultTranslogHighwater is where the checkpoint job keeps the newest
// checkpoint this box signed, outside the database (RP37-11): on the NVMe,
// read by both slots (one user, no PrivateTmp — like MULTIPART_STAGING_DIR).
const defaultTranslogHighwater = "/opt/vaultaire/data/translog/highwater"

// translogHighwaterFile is TRANSLOG_HIGHWATER_FILE (an absolute path), else
// the default; a relative value is a Warn and the default is kept.
func translogHighwaterFile(logger *zap.Logger, getenv func(string) string) string {
	p := strings.TrimSpace(getenv("TRANSLOG_HIGHWATER_FILE"))
	switch {
	case p == "":
		p = defaultTranslogHighwater
	case !filepath.IsAbs(p):
		logger.Warn("TRANSLOG_HIGHWATER_FILE is not an absolute path — the default is kept",
			zap.String("value", p), zap.String("default", defaultTranslogHighwater))
		p = defaultTranslogHighwater
	}
	logger.Info("translog high-water file", zap.String("path", p))
	return p
}

// translogCheckpointer is the translog_checkpoint job: every 10 min, under
// the job lock, a checkpoint of the whole log is signed when the tree grew
// past the newest one — after proving from the stored hashes that the log
// extends it (a log that does not is a failed run and an Error line, and
// nothing is signed). Without a key the run is ok and signs nothing.
type translogCheckpointer struct {
	store  *logstore.Store
	logger *zap.Logger
	// build is this process's boot entry while it waits for the log
	// (RP42-1): each run tries it first, then checkpoints.
	build *bootBuild
	// clients is the client-build manifest's rows the log refused so far
	// (K4c-3b): retried like build, before the checkpoint.
	clients *clientBuilds
	// cosign submits each new checkpoint to the witnesses (K4c-2,
	// translog_witnesses.go); nil = TRANSLOG_WITNESSES unset.
	cosign  *cosignCollector
	JobName string
}

func newTranslogCheckpointer(store *logstore.Store, logger *zap.Logger) *translogCheckpointer {
	if store == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &translogCheckpointer{store: store, logger: logger, JobName: translogCheckpointJobName}
}

func (c *translogCheckpointer) spec() jobSpec {
	return jobSpec{Name: c.JobName, Every: 10 * time.Minute, BootDelay: 90 * time.Second, MaxRunTime: 2 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			c.build.try(ctx, c.store, c.logger, true)
			c.clients.try(ctx, c.store, c.logger, true)
			res, err := c.store.Checkpoint(ctx)
			if res.Signed {
				c.logger.Info("translog checkpoint signed", zap.Int64("size", res.Size), zap.String("root", res.Root))
			}
			// Only a pass that refused nothing submits: a refused log is
			// never put in front of a witness.
			if err == nil {
				res.Witnesses = c.cosign.collect(ctx)
			}
			rep := jobReport{Result: res, Note: res.Reason}
			if n := witnessNote(res.Witnesses); n != "" {
				rep.Note = strings.TrimPrefix(rep.Note+"; "+n, "; ")
			}
			if res.Signed {
				rep.Rows = 1
			}
			if err != nil {
				c.logger.Error("translog checkpoint: nothing signed", zap.Error(err), zap.String("refused", res.Refused),
					zap.Int64("size", res.Size), zap.Int64("checkpoint_size", res.CheckpointSize), zap.Int64("highwater_size", res.HighwaterSize))
			}
			return rep, err
		}}
}
