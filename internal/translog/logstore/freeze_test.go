package logstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
)

// Stage 1a Prompt 33 Part 0a (RP42-1): the log freezes while the database is
// behind the high-water file. Before the gate, ANY append to a restored
// database — a slot's boot server_build, a ceremony — took index M with a
// body the key never signed, and translog-restore's re-append of M…N-1 then
// failed on the root for ever (the tables refuse DELETE).

func TestAppend_BehindHighwater_Refused(t *testing.T) {
	// Arrange: a log of 9 signed at 5 and 9, the database restored at 5.
	ctx := context.Background()
	hw, at9, live := signedLog(t)
	lost, err := live.Entries(ctx, 5, 4)
	require.NoError(t, err)
	restored := restoreOf(t, live.db, 5)
	r := New(restored, live.signer).WithHighwater(hw)
	refusals := promtest.ToFloat64(appendRefusals.WithLabelValues(RefusedBehindHighwater))
	failures := promtest.ToFloat64(appendFailures.WithLabelValues(translog.KindServerBuild))

	// Act: a slot boots on the restored database before the re-append.
	_, appended, err := r.Append(ctx, uniqueBuild(t))

	// Assert: refused, nothing written, counted as a refusal (not a failure:
	// TranslogAppendFailing must not page on the same event).
	require.ErrorIs(t, err, ErrLogFrozen)
	assert.False(t, appended)
	assert.Contains(t, err.Error(), "5 entries")
	size, err := r.Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(5), size)
	assert.Equal(t, refusals+1, promtest.ToFloat64(appendRefusals.WithLabelValues(RefusedBehindHighwater)))
	assert.Equal(t, failures, promtest.ToFloat64(appendFailures.WithLabelValues(translog.KindServerBuild)))
	reason, err := r.Frozen(ctx)
	require.NoError(t, err)
	assert.Equal(t, RefusedBehindHighwater, reason)

	// The way out is not gated: the re-append succeeds, then appends pass.
	require.NoError(t, r.ReAppend(ctx, lost, at9))
	reason, err = r.Frozen(ctx)
	require.NoError(t, err)
	assert.Empty(t, reason)
	idx, appended, err := r.Append(ctx, uniqueBuild(t))
	require.NoError(t, err)
	assert.True(t, appended)
	assert.Equal(t, int64(9), idx)
	res, err := r.Checkpoint(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Refused)
	assert.True(t, res.Signed)
}

func TestAppend_HighwaterUnreadable_Refused(t *testing.T) {
	ctx := context.Background()
	db := logstoretest.PrivateLog(t)
	dir := t.TempDir()
	cases := map[string]func(path string){
		"garbage": func(p string) { require.NoError(t, os.WriteFile(p, []byte("not a checkpoint\n"), 0o600)) },
		"empty":   func(p string) { require.NoError(t, os.WriteFile(p, nil, 0o600)) },
		// RP42-6: a v1 file left in place under a v2 log is not this log's.
		"another origin": func(p string) {
			require.NoError(t, os.WriteFile(p, []byte("stored.cloud/translog/v0\n1\n"+strings.Repeat("A", 43)+"=\n"), 0o600))
		},
		"a directory": func(p string) { require.NoError(t, os.Mkdir(p, 0o700)) },
	}
	for name, mk := range cases {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		mk(p)
		before := promtest.ToFloat64(appendRefusals.WithLabelValues(RefusedHighwaterUnreadable))

		_, appended, err := New(db, nil).WithHighwater(p).Append(ctx, ceremony(t))

		require.ErrorIs(t, err, ErrLogFrozen, name)
		assert.False(t, appended, name)
		assert.Equal(t, before+1, promtest.ToFloat64(appendRefusals.WithLabelValues(RefusedHighwaterUnreadable)), name)
	}
	n, err := New(db, nil).Size(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "nothing appended")

	// An absent file is the first run: the append passes.
	_, appended, err := New(db, nil).WithHighwater(filepath.Join(dir, "absent")).Append(ctx, ceremony(t))
	require.NoError(t, err)
	assert.True(t, appended)
}

// RP42-6: another origin's file says so, and names the way to archive it.
func TestReadHighwater_AnotherOrigin_NamesTheArchiveStep(t *testing.T) {
	p := filepath.Join(t.TempDir(), "highwater")
	require.NoError(t, os.WriteFile(p, []byte("stored.cloud/translog/v0\n1\n"+strings.Repeat("A", 43)+"=\n"), 0o600))

	_, ok, err := ReadHighwater(p)

	require.Error(t, err)
	assert.False(t, ok)
	assert.Contains(t, err.Error(), `"stored.cloud/translog/v0"`)
	assert.Contains(t, err.Error(), "highwater.v0")
}

// Every append-refusal reason is a series from boot (at 0).
func TestAppendRefusals_PreCreated(t *testing.T) {
	assert.Equal(t, 2, promtest.CollectAndCount(appendRefusals))
	assert.ElementsMatch(t, []string{"behind_highwater", "highwater_unreadable"}, AppendRefusalReasons())
}
