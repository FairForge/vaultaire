package ots_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/translog/ots"
	"github.com/FairForge/vaultaire/internal/translog/ots/otstest"
)

func TestOTS_SubmitUpgradeCombine(t *testing.T) {
	// Arrange
	ctx := context.Background()
	a, b := otstest.New(t), otstest.New(t)
	c := ots.NewClient("test")
	digest := sha256.Sum256([]byte("a checkpoint note"))

	// Act: two calendars commit; a confirms.
	pa, err := c.Submit(ctx, a.URL, digest)
	require.NoError(t, err)
	pb, err := c.Submit(ctx, b.URL+"/", digest)
	require.NoError(t, err)
	_, _, errPending := c.Upgrade(ctx, a.URL, pa)
	a.Confirm()
	up, complete, err := c.Upgrade(ctx, a.URL, pa)
	require.NoError(t, err)
	both, err := ots.Combine(up, pb)
	require.NoError(t, err)

	// Assert
	assert.ErrorIs(t, errPending, ots.ErrPending, "404 = not in a block yet")
	assert.True(t, complete)
	d, atts, err := ots.Describe(pa)
	require.NoError(t, err)
	assert.Equal(t, digest[:], d)
	assert.Equal(t, []ots.Attestation{{Calendar: a.URL}}, atts)
	_, atts, err = ots.Describe(both)
	require.NoError(t, err)
	assert.ElementsMatch(t, []ots.Attestation{{BitcoinHeight: 900000}, {Calendar: b.URL}}, atts)
	done, err := ots.Complete(pb)
	require.NoError(t, err)
	assert.False(t, done)
}

// A pending attestation is completed only by the calendar configured, and
// only when it names that calendar: a calendar must not send the log to an
// address of its choosing.
func TestOTS_UpgradeAsksOnlyTheCalendarItNames(t *testing.T) {
	// Arrange
	ctx := context.Background()
	cal := otstest.New(t)
	cal.Confirm()
	c := ots.NewClient("test")
	// A stored proof naming another URL (Submit refuses such an answer since
	// RP48-1, so the test builds it): Upgrade must not ask that URL.
	p := otstest.PendingProof(sha256.Sum256([]byte("x")), "http://127.0.0.1:1/elsewhere")

	// Act
	_, _, err := c.Upgrade(ctx, cal.URL, p)

	// Assert
	assert.ErrorIs(t, err, ots.ErrPending)
	assert.Zero(t, cal.Gets.Load(), "never asked")
}

// RP48-1: a calendar whose answer's pending attestation names another URL —
// every aggregator pool, e.g. a.pool.opentimestamps.org, whose attestations
// name alice/bob — is refused at Submit: such a proof would never be asked
// again (Upgrade asks only the configured calendar) and could never
// complete. The error names both URLs. An Upgrade tail is held to the same
// rule.
func TestOTS_SubmitRefusesAPoolsAnswer(t *testing.T) {
	// Arrange
	ctx := context.Background()
	cal := otstest.New(t)
	cal.PendingURL = "https://alice.btc.calendar.opentimestamps.org"

	// Act
	_, err := ots.NewClient("test").Submit(ctx, cal.URL, sha256.Sum256([]byte("x")))

	// Assert
	require.ErrorIs(t, err, ots.ErrBadAnswer)
	assert.Contains(t, err.Error(), "https://alice.btc.calendar.opentimestamps.org")
	assert.Contains(t, err.Error(), cal.URL)
}

func TestOTS_CalendarFailuresAreClassified(t *testing.T) {
	ctx := context.Background()
	digest := sha256.Sum256([]byte("x"))
	for name, c := range map[string]struct {
		h    http.HandlerFunc
		want error
	}{
		"a 503":           {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, ots.ErrUnreachable},
		"a 400":           {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(400) }, ots.ErrRefused},
		"not a timestamp": {func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>hello</html>")) }, ots.ErrBadAnswer},
		"an empty 200":    {func(http.ResponseWriter, *http.Request) {}, ots.ErrBadAnswer},
		"a redirect": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/x", http.StatusFound)
		}, ots.ErrRefused},
		"too large": {func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(make([]byte, ots.MaxProof+1)) }, ots.ErrBadAnswer},
	} {
		srv := httptest.NewServer(c.h)
		_, err := ots.NewClient("test").Submit(ctx, srv.URL, digest)
		srv.Close()
		assert.ErrorIs(t, err, c.want, name)
	}
	_, err := ots.NewClient("test").Submit(ctx, "http://127.0.0.1:1", digest)
	assert.ErrorIs(t, err, ots.ErrUnreachable, "connection refused")
}

// The library panics on an operation it does not implement; a calendar
// answering one is a bad answer, never a crash of the job.
func TestOTS_LibraryPanicIsABadAnswer(t *testing.T) {
	// Arrange
	ctx := context.Background()
	cal := otstest.New(t)
	cal.Reverse = true
	cal.Confirm()
	c := ots.NewClient("test")
	p, err := c.Submit(ctx, cal.URL, sha256.Sum256([]byte("x")))
	require.NoError(t, err, "parsing does not apply the operations")

	// Act
	_, _, err = c.Upgrade(ctx, cal.URL, p)

	// Assert
	assert.ErrorIs(t, err, ots.ErrBadAnswer)
	assert.Zero(t, cal.Gets.Load())
}

// testdata/checkpoint3-pending.ots is what the lab served for its checkpoint
// 3 (Stage 1a Prompt 35): the real answers of three public calendars,
// combined. The upstream client (`ots info`, v0.7.2) read it as the same
// digest and the same three pending attestations.
func TestOTS_RealCalendarsProof(t *testing.T) {
	// Arrange
	raw, err := os.ReadFile("testdata/checkpoint3-pending.ots")
	require.NoError(t, err)

	// Act
	d, atts, err := ots.Describe(raw)
	require.NoError(t, err)
	again, err := ots.Combine(raw, raw)
	require.NoError(t, err)
	done, err := ots.Complete(raw)
	require.NoError(t, err)

	// Assert
	assert.Equal(t, "19668629f6804000310c6443fbceb7e9c9ce36df12888c9d9cf30097d6b44df5", hex.EncodeToString(d))
	assert.ElementsMatch(t, []ots.Attestation{
		{Calendar: "https://alice.btc.calendar.opentimestamps.org"},
		{Calendar: "https://bob.btc.calendar.opentimestamps.org"},
		{Calendar: "https://finney.calendar.eternitywall.com"},
	}, atts)
	assert.Equal(t, raw, again, "combining a proof with itself is the proof")
	assert.False(t, done)
}
