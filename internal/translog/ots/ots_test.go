package ots_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
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
	cal.PendingURL = "http://127.0.0.1:1/elsewhere"
	cal.Confirm()
	c := ots.NewClient("test")
	p, err := c.Submit(ctx, cal.URL, sha256.Sum256([]byte("x")))
	require.NoError(t, err)

	// Act
	_, _, err = c.Upgrade(ctx, cal.URL, p)

	// Assert
	assert.ErrorIs(t, err, ots.ErrPending)
	assert.Zero(t, cal.Gets.Load(), "never asked")
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
