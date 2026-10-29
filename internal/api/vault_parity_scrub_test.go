package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/engine"
)

// WP-VAULT-2 (docs/reviews/WP-VAULT-2.md § 1–4 + § 7): the parity scrub.
// Nothing read a parity shard until the day Geyser failed; a copy that cannot
// be read is no copy. The scrub reads every complete row's shards end to end
// once per VAULT_PARITY_SCRUB_CYCLE, checks them against the digests protect
// recorded, rebuilds one object per pass from the parity ALONE against the
// stored bytes' hash, and re-protects a row that fails. A protect that finds
// its object on tape asks for the restore itself.

// scrubFixture: the parity fixture with the scrub due on every run, its retry
// immediate, and a cycle that reads everything in one pass.
func scrubFixture(t *testing.T) *parityFixture {
	f := setupParityFixture(t)
	f.svc.ScrubEvery = time.Nanosecond
	f.svc.ScrubCycle = time.Nanosecond // budget = the whole footprint
	f.svc.ScrubRetryAfter = time.Millisecond
	return f
}

type scrubRowState struct {
	state, lastErr, objectSHA, digestEtag string
	digests                               []string
	attempts, failures                    int
	scrubbed, rebuilt, restoreRequested   bool
}

func (f *parityFixture) scrubRow(key string) scrubRowState {
	f.t.Helper()
	var r scrubRowState
	var d pq.StringArray
	var le, osum, de sql.NullString
	var sc, rb, rr sql.NullTime
	require.NoError(f.t, f.db.QueryRow(`
		SELECT state, COALESCE(last_error,''), attempts, shard_digests, object_sha256, digest_etag,
		       scrub_failures, scrubbed_at, rebuilt_at, restore_requested_at
		FROM vault_parity WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3`, f.tenantID, f.bucket, key).
		Scan(&r.state, &le, &r.attempts, &d, &osum, &de, &r.failures, &sc, &rb, &rr))
	r.lastErr, r.objectSHA, r.digestEtag, r.digests = le.String, osum.String, de.String, []string(d)
	r.scrubbed, r.rebuilt, r.restoreRequested = sc.Valid, rb.Valid, rr.Valid
	return r
}

func (f *parityFixture) shardPath(key, etag string, j int) string {
	return filepath.Join(f.legDir, parityContainer(f.tenantID), shardArtifact(shardPrefix(f.bucket, key, etag), j))
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestVaultParity_ProtectRecordsShardDigestsAndTheStoredBytesHash(t *testing.T) {
	f := setupParityFixture(t)
	body, etag := f.object("films/one.mkv", 300<<10)

	f.run()

	r := f.scrubRow("films/one.mkv")
	require.Equal(t, "complete", r.state)
	assert.Equal(t, sha256Hex(body), r.objectSHA, "the stored bytes, as protect read them")
	assert.Equal(t, etag, r.digestEtag)
	require.Len(t, r.digests, 4)
	for j := 0; j < 4; j++ {
		shard, err := os.ReadFile(f.shardPath("films/one.mkv", etag, j))
		require.NoError(t, err)
		assert.Equal(t, sha256Hex(shard), r.digests[j], "shard p%d", j)
	}
}

func TestVaultParityScrub_ACleanPassStampsTheRowAndRebuildsFromParityAlone(t *testing.T) {
	f := scrubFixture(t)
	f.object("films/two.mkv", 300<<10)
	f.svc.ScrubEvery = time.Hour
	f.run() // protect (the scrub ran first, with nothing to read)
	f.svc.ScrubEvery = time.Nanosecond
	okBefore := testutil.ToFloat64(vaultParityScrubShards.WithLabelValues("ok"))
	rebuildBefore := testutil.ToFloat64(vaultParityRebuilds.WithLabelValues("ok"))

	// Geyser is gone: the rebuild check never reads the object.
	f.geyser.failGet.Store(true)
	f.geyser.failRange.Store(true)
	res := f.run()

	r := f.scrubRow("films/two.mkv")
	assert.Equal(t, "complete", r.state)
	assert.True(t, r.scrubbed)
	assert.True(t, r.rebuilt, "rebuilt from the four parity shards alone: %+v", res)
	assert.Equal(t, 0, r.failures)
	assert.Equal(t, 1, res.ScrubRows)
	assert.Equal(t, "ok", res.Rebuild)
	assert.Equal(t, 4.0, testutil.ToFloat64(vaultParityScrubShards.WithLabelValues("ok"))-okBefore)
	assert.Equal(t, 1.0, testutil.ToFloat64(vaultParityRebuilds.WithLabelValues("ok"))-rebuildBefore)
	assert.Zero(t, f.geyser.rangeGets.Load())
}

func TestVaultParityScrub_ACorruptShardIsReprotected(t *testing.T) {
	f := scrubFixture(t)
	_, etag := f.object("films/three.mkv", 300<<10)
	f.run()
	require.Equal(t, "complete", f.scrubRow("films/three.mkv").state)

	// Same length, one byte flipped.
	p := f.shardPath("films/three.mkv", etag, 2)
	shard, err := os.ReadFile(p)
	require.NoError(t, err)
	shard[100] ^= 0xff
	require.NoError(t, os.WriteFile(p, shard, 0o600))
	before := testutil.ToFloat64(vaultParityScrubShards.WithLabelValues("corrupt"))

	f.svc.MaxObjectsPerRun = 0 // this run scrubs; it does not re-protect yet
	res := f.run()

	r := f.scrubRow("films/three.mkv")
	assert.Equal(t, "partial", r.state)
	assert.Equal(t, 0, r.attempts, "the protect gets every attempt back")
	assert.Equal(t, 1, r.failures)
	assert.Contains(t, r.lastErr, "p2 corrupt")
	assert.Equal(t, 1, res.ScrubReprotect)
	assert.Equal(t, 1.0, testutil.ToFloat64(vaultParityScrubShards.WithLabelValues("corrupt"))-before)

	// The next run's protect rewrites the shards; the scrub after it is clean.
	f.svc.MaxObjectsPerRun = 500
	f.run()
	r = f.scrubRow("films/three.mkv")
	require.Equal(t, "complete", r.state)
	f.run()
	r = f.scrubRow("films/three.mkv")
	assert.True(t, r.scrubbed)
	assert.Equal(t, 0, r.failures, "a clean scrub resets the count")
}

func TestVaultParityScrub_AMissingShardIsReprotectedAtOnce(t *testing.T) {
	f := scrubFixture(t)
	_, etag := f.object("films/four.mkv", 300<<10)
	f.run()
	require.NoError(t, os.Remove(f.shardPath("films/four.mkv", etag, 1)))

	f.svc.MaxObjectsPerRun = 0
	f.run()

	r := f.scrubRow("films/four.mkv")
	assert.Equal(t, "partial", r.state)
	assert.Contains(t, r.lastErr, "p1 missing")
}

func TestVaultParityScrub_ATransientReadIsReadAgainBeforeItCounts(t *testing.T) {
	f := scrubFixture(t)
	f.object("films/five.mkv", 300<<10)
	f.run()

	f.leg.failGets.Store(1) // the first shard read fails, the re-read succeeds
	f.svc.MaxObjectsPerRun = 0
	f.run()
	r := f.scrubRow("films/five.mkv")
	assert.Equal(t, "complete", r.state, "a transient is not a fault: %s", r.lastErr)
	assert.True(t, r.scrubbed)

	f.leg.failGets.Store(2) // the read and its re-read both fail
	f.run()
	r = f.scrubRow("films/five.mkv")
	assert.Equal(t, "partial", r.state)
	assert.Contains(t, r.lastErr, "unreadable")
}

func TestVaultParityScrub_ARowWithoutDigestsIsBaselinedNotFailed(t *testing.T) {
	f := scrubFixture(t)
	_, etag := f.object("films/six.mkv", 300<<10)
	f.run()
	want := f.scrubRow("films/six.mkv").digests
	// A row written before 118, and one whose new etag an older build
	// protected (its digests belong to another etag).
	_, err := f.db.Exec(`UPDATE vault_parity SET shard_digests = NULL, digest_etag = NULL, object_sha256 = NULL WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)

	res := f.run()

	r := f.scrubRow("films/six.mkv")
	assert.Equal(t, "complete", r.state)
	assert.Equal(t, want, r.digests, "the baseline is what the scrub read")
	assert.Equal(t, etag, r.digestEtag)
	assert.Equal(t, 1, res.ScrubBaselined)
	assert.Equal(t, "none", res.Rebuild, "no stored-bytes hash: nothing to rebuild against")

	_, err = f.db.Exec(`UPDATE vault_parity SET shard_digests = ARRAY['00','00','00','00'], digest_etag = 'another-etag' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	f.run()
	assert.Equal(t, "complete", f.scrubRow("films/six.mkv").state, "digests of another etag are not this row's")
}

// A parity computed wrong passes every digest it recorded; only the rebuild
// catches it.
func TestVaultParityScrub_ARebuildThatDoesNotMatchIsReprotected(t *testing.T) {
	f := scrubFixture(t)
	_, etag := f.object("films/seven.mkv", 300<<10)
	f.run()
	p := f.shardPath("films/seven.mkv", etag, 0)
	shard, err := os.ReadFile(p)
	require.NoError(t, err)
	shard[0] ^= 0x01
	require.NoError(t, os.WriteFile(p, shard, 0o600))
	r := f.scrubRow("films/seven.mkv")
	r.digests[0] = sha256Hex(shard) // the digest says what is there
	_, err = f.db.Exec(`UPDATE vault_parity SET shard_digests = $2 WHERE tenant_id = $1`, f.tenantID, pq.Array(r.digests))
	require.NoError(t, err)
	before := testutil.ToFloat64(vaultParityRebuilds.WithLabelValues("mismatch"))

	f.svc.MaxObjectsPerRun = 0
	res := f.run()

	assert.Equal(t, "mismatch", res.Rebuild)
	assert.Equal(t, 1.0, testutil.ToFloat64(vaultParityRebuilds.WithLabelValues("mismatch"))-before)
	got := f.scrubRow("films/seven.mkv")
	assert.Equal(t, "partial", got.state)
	assert.Contains(t, got.lastErr, "rebuild")
}

func TestVaultParityScrub_AnOpenLegBreakerIsNoVerdict(t *testing.T) {
	f := scrubFixture(t)
	f.object("films/eight.mkv", 300<<10)
	f.run()
	f.leg.failGet.Store(true)
	ctx := common.WithTenantID(context.Background(), f.tenantID)
	for i := 0; i < 6 && f.eng.GetFailoverStatus()["permafrost"] != engine.StateOpen.String(); i++ {
		_, _ = f.eng.GetOn(ctx, "permafrost", "c", "k")
	}
	require.Equal(t, engine.StateOpen.String(), f.eng.GetFailoverStatus()["permafrost"])

	res := f.run()

	r := f.scrubRow("films/eight.mkv")
	assert.Equal(t, "complete", r.state, "an unreachable leg is never a corrupt shard")
	assert.False(t, r.scrubbed)
	assert.Equal(t, 1, res.ScrubSkipped)
}

// The budget is the footprint over the cycle (A1): a pass reads its share and
// the cursor carries the rest to the next pass, wrapping at the end.
func TestVaultParityScrub_TheCycleBudgetSpreadsRowsOverPasses(t *testing.T) {
	assert.Equal(t, int64(1000), scrubBudget(720_000, 30*time.Minute, 360*time.Hour), "a 720th of the footprint per 30 min of a 15 d cycle")
	assert.Equal(t, int64(1), scrubBudget(10, time.Minute, 30*24*time.Hour), "never 0: at least one row moves")
	assert.Equal(t, int64(500), scrubBudget(500, time.Hour, time.Minute), "never more than the footprint")

	f := scrubFixture(t)
	for _, k := range []string{"a/1", "a/2", "a/3"} {
		f.object(k, 300<<10)
	}
	f.svc.ScrubEvery = time.Hour
	f.run()
	f.svc.ScrubEvery = time.Nanosecond
	f.svc.ScrubCycle = 0 // the test's budget below
	f.svc.scrubBudgetOverride = 1

	scrubbed := func() int {
		var n int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM vault_parity WHERE tenant_id = $1 AND scrubbed_at IS NOT NULL`, f.tenantID).Scan(&n))
		return n
	}
	f.run()
	assert.Equal(t, 1, scrubbed())
	f.run()
	assert.Equal(t, 2, scrubbed())
	f.run()
	assert.Equal(t, 3, scrubbed())
	res := f.run()
	assert.True(t, res.ScrubWrapped, "the fourth pass starts the cycle again")
	assert.Equal(t, 1, res.ScrubRows)
}

// A row too large for a whole pass is passed over and named, never cut on
// every pass with the cursor stuck on it.
func TestVaultParityScrub_ARowTooLargeForAPassIsPassedOverNotRetriedForEver(t *testing.T) {
	f := scrubFixture(t)
	f.object("a/big", 300<<10)
	f.object("a/small", 300<<10)
	f.svc.ScrubEvery = time.Hour
	f.run()
	f.svc.ScrubEvery = time.Nanosecond
	f.svc.ScrubSlice = 2 * time.Second
	f.svc.ProtectRate = 1 << 20 // 1 MiB/s: a/small's 4 × 128 KiB shards ≈ 0.5 s
	f.svc.ProtectOverhead = time.Millisecond
	_, err := f.db.Exec(`UPDATE vault_parity SET shard_bytes = 4 << 20 WHERE tenant_id = $1 AND object_key = 'a/big'`, f.tenantID)
	require.NoError(t, err) // its estimate: 16 MiB at 1 MiB/s, longer than the slice

	res := f.run()

	assert.Equal(t, 1, res.ScrubTooLarge, "%+v", res)
	assert.Equal(t, 1, res.ScrubRows, "the walk went on past it")
	assert.Contains(t, res.ScrubFindings[0], "a/big")
}

func TestVaultParityScrub_RunsAtMostEveryScrubEveryAndCarriesItsState(t *testing.T) {
	f := scrubFixture(t)
	f.object("films/nine.mkv", 300<<10)
	f.run()
	f.svc.ScrubEvery = time.Hour
	res := f.run()
	assert.True(t, res.ScrubDeferred)
	assert.False(t, res.ScrubAt.IsZero(), "the state is carried in every result")
}

// A3: a protect that finds its object on tape asks for the restore itself,
// spends no attempt, asks again only after restoreRecheck, and protects the
// object once the restore has landed.
func TestVaultParity_AProtectOfAnObjectOnTapeRequestsItsRestore(t *testing.T) {
	f := setupParityFixture(t)
	tape := &restorableGeyser{flakyGetDriver: f.geyser}
	tape.archived.Store(true)
	require.NoError(t, f.eng.AddDriver("geyser", tape))
	f.object("films/ten.mkv", 300<<10)
	before := testutil.ToFloat64(vaultParityRestores.WithLabelValues("requested"))

	res := f.run()

	r := f.scrubRow("films/ten.mkv")
	assert.Equal(t, "partial", r.state)
	assert.Equal(t, 0, r.attempts, "waiting for a restore spends no attempt")
	assert.True(t, r.restoreRequested)
	assert.Equal(t, int32(1), tape.restores.Load())
	assert.Equal(t, 1, res.RestoresRequested)
	assert.Equal(t, 1.0, testutil.ToFloat64(vaultParityRestores.WithLabelValues("requested"))-before)

	f.run()
	assert.Equal(t, int32(1), tape.restores.Load(), "not asked again within restoreRecheck")

	tape.archived.Store(false) // the restore landed
	_, err := f.db.Exec(`UPDATE vault_parity SET restore_checked_at = NOW() - INTERVAL '1 hour' WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)
	f.run()
	r = f.scrubRow("films/ten.mkv")
	assert.Equal(t, "complete", r.state, r.lastErr)
	assert.False(t, r.restoreRequested, "cleared once protected")
}

func TestVaultParityScrubSettingsFromEnv(t *testing.T) {
	env := func(k, v string) func(string) string {
		return func(n string) string {
			if n == k {
				return v
			}
			return ""
		}
	}
	lg := zap.NewNop()
	assert.Equal(t, defaultScrubEvery, scrubEveryFromEnv(env("", ""), lg))
	assert.Equal(t, 2*time.Hour, scrubEveryFromEnv(env("VAULT_PARITY_SCRUB_EVERY", "2h"), lg))
	assert.Equal(t, defaultScrubEvery, scrubEveryFromEnv(env("VAULT_PARITY_SCRUB_EVERY", "10s"), lg), "below 1m: rejected")
	assert.Equal(t, defaultScrubCycle, scrubCycleFromEnv(env("", ""), lg))
	assert.Equal(t, 14*24*time.Hour, scrubCycleFromEnv(env("VAULT_PARITY_SCRUB_CYCLE", "14d"), lg))
	assert.Equal(t, 48*time.Hour, scrubCycleFromEnv(env("VAULT_PARITY_SCRUB_CYCLE", "48h"), lg))
	assert.Equal(t, defaultScrubCycle, scrubCycleFromEnv(env("VAULT_PARITY_SCRUB_CYCLE", "200d"), lg), "above 180d: rejected")
}

// restorableGeyser is Geyser with its tape: Get answers ErrArchived while
// archived, RestoreObject counts the requests.
type restorableGeyser struct {
	*flakyGetDriver
	archived atomic.Bool
	restores atomic.Int32
}

func (d *restorableGeyser) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	if d.archived.Load() {
		return nil, engine.ErrArchived
	}
	return d.flakyGetDriver.Get(ctx, container, artifact)
}

func (d *restorableGeyser) RestoreObject(context.Context, string, string, int32) error {
	if d.restores.Add(1) > 1 {
		return engine.ErrRestoreAlreadyInProgress
	}
	return nil
}

func (d *restorableGeyser) RestoreStatus(context.Context, string, string) (*engine.RestoreStatus, error) {
	return &engine.RestoreStatus{}, nil
}
