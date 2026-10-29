package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-VAULT-2 A4: the scrub covers the second copy, never the first. Once a
// week the vault_tape_audit job reads ONE random vault object back from its
// own backend — asking for its restore first when it is on tape, and waiting
// for it across runs — and hashes the stored bytes against what protect read
// (vault_parity.object_sha256).

func tapeAuditFixture(t *testing.T) (*parityFixture, *TapeAudit) {
	f := setupParityFixture(t)
	a := NewTapeAudit(f.db, f.eng, f.logger)
	require.NotNil(t, a)
	a.scopeTenant = f.tenantID
	a.JobName = "test_tape_audit_" + f.tenantID[:8]
	return f, a
}

func (f *parityFixture) geyserFile(key string) string {
	return filepath.Join(f.geyserDir, f.tn.NamespaceContainer(f.bucket), key)
}

func TestTapeAudit_NothingToAuditIsNoFailure(t *testing.T) {
	_, a := tapeAuditFixture(t)
	before := testutil.ToFloat64(vaultTapeAudits.WithLabelValues("nothing"))

	res, err := a.RunOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "nothing", res.Outcome)
	assert.False(t, res.LastOKAt.IsZero(), "nothing auditable is not a stale audit")
	assert.Equal(t, 1.0, testutil.ToFloat64(vaultTapeAudits.WithLabelValues("nothing"))-before)
}

func TestTapeAudit_ReadsAnObjectBackAndMatchesItsStoredBytes(t *testing.T) {
	f, a := tapeAuditFixture(t)
	f.object("films/a.mkv", 300<<10)
	f.run() // protect: the row records the stored bytes' hash
	before := testutil.ToFloat64(vaultTapeAudits.WithLabelValues("ok"))

	res, err := a.RunOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "ok", res.Outcome, res.Detail)
	assert.Equal(t, f.bucket+"/films/a.mkv", res.Object)
	assert.Equal(t, int64(300<<10), res.Bytes)
	assert.False(t, res.FromTape)
	assert.Equal(t, 1.0, testutil.ToFloat64(vaultTapeAudits.WithLabelValues("ok"))-before)

	// A week has not passed: the next run does not read again.
	res, err = a.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "not_due", res.Outcome)
}

func TestTapeAudit_BytesThatDoNotMatchAreAMismatch(t *testing.T) {
	f, a := tapeAuditFixture(t)
	f.object("films/b.mkv", 300<<10)
	f.run()
	body, err := os.ReadFile(f.geyserFile("films/b.mkv"))
	require.NoError(t, err)
	body[1000] ^= 0xff
	require.NoError(t, os.WriteFile(f.geyserFile("films/b.mkv"), body, 0o600))
	before := testutil.ToFloat64(vaultTapeAudits.WithLabelValues("mismatch"))

	res, err := a.RunOnce(context.Background())

	require.NoError(t, err, "a finding is not a failed run")
	assert.Equal(t, "mismatch", res.Outcome)
	assert.Equal(t, 1.0, testutil.ToFloat64(vaultTapeAudits.WithLabelValues("mismatch"))-before)
	assert.True(t, res.LastOKAt.IsZero(), "no clean audit yet")
}

func TestTapeAudit_AnObjectOnTapeIsRestoredThenRead(t *testing.T) {
	f, a := tapeAuditFixture(t)
	f.object("films/c.mkv", 300<<10)
	f.run()
	tape := &restorableGeyser{flakyGetDriver: f.geyser}
	tape.archived.Store(true)
	require.NoError(t, f.eng.AddDriver("geyser", tape))

	res, err := a.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "waiting", res.Outcome)
	require.NotNil(t, res.Pending)
	assert.Equal(t, int32(1), tape.restores.Load())

	res, err = a.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "waiting", res.Outcome, "still on tape: the same object is waited for")

	tape.archived.Store(false)
	res, err = a.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ok", res.Outcome, res.Detail)
	assert.True(t, res.FromTape)
	assert.Nil(t, res.Pending)
}

func TestTapeAudit_ARestoreThatNeverLandsIsAnError(t *testing.T) {
	f, a := tapeAuditFixture(t)
	f.object("films/d.mkv", 300<<10)
	f.run()
	tape := &restorableGeyser{flakyGetDriver: f.geyser}
	tape.archived.Store(true)
	require.NoError(t, f.eng.AddDriver("geyser", tape))
	_, err := a.RunOnce(context.Background())
	require.NoError(t, err)
	a.now = func() time.Time { return time.Now().Add(49 * time.Hour) }
	before := testutil.ToFloat64(vaultTapeAudits.WithLabelValues("error"))

	res, err := a.RunOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "error", res.Outcome)
	assert.Contains(t, res.Detail, "restore")
	assert.Nil(t, res.Pending)
	assert.Equal(t, 1.0, testutil.ToFloat64(vaultTapeAudits.WithLabelValues("error"))-before)
}

func TestTapeAudit_AnObjectDeletedWhileItsRestoreRunsIsDropped(t *testing.T) {
	f, a := tapeAuditFixture(t)
	f.object("films/e.mkv", 300<<10)
	f.run()
	tape := &restorableGeyser{flakyGetDriver: f.geyser}
	tape.archived.Store(true)
	require.NoError(t, f.eng.AddDriver("geyser", tape))
	_, err := a.RunOnce(context.Background())
	require.NoError(t, err)
	_, err = f.db.Exec(`DELETE FROM object_head_cache WHERE tenant_id = $1`, f.tenantID)
	require.NoError(t, err)

	res, err := a.RunOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "changed", res.Outcome)
	assert.Nil(t, res.Pending)
	assert.True(t, res.LastAuditAt.IsZero(), "the next run picks another object")
}
