package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/drivers"
)

type fakeReaper struct {
	res   drivers.StripeReapResult
	err   error
	grace time.Duration
}

func (f *fakeReaper) ReapOrphanStripes(_ context.Context, olderThan time.Duration) (drivers.StripeReapResult, error) {
	f.grace = olderThan
	return f.res, f.err
}

func TestStripeGC_ReportsReapedGenerationsAndNotes(t *testing.T) {
	// Arrange
	f := &fakeReaper{res: drivers.StripeReapResult{Backend: "sync", Reaped: 3, Errors: []string{"t-x/b%p/aa/h/g: bridge down"}}}
	g := &stripeGC{r: f, Grace: 6 * time.Hour, JobName: "stripe_gc", Every: time.Hour}

	// Act
	rep, err := g.spec().Run(context.Background())

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int64(3), rep.Rows)
	assert.Contains(t, rep.Note, "1 generation(s) kept on an error")
	assert.Equal(t, 6*time.Hour, f.grace)
}

func TestStripeGC_AListingFailureFailsTheRun(t *testing.T) {
	g := &stripeGC{r: &fakeReaper{err: errors.New("every bridge is down")}, Grace: time.Hour, JobName: "stripe_gc"}
	_, err := g.spec().Run(context.Background())
	require.Error(t, err)

	g = &stripeGC{r: &fakeReaper{err: context.DeadlineExceeded}, Grace: time.Hour, JobName: "stripe_gc"}
	rep, err := g.spec().Run(context.Background())
	require.NoError(t, err)
	assert.Contains(t, rep.Note, "run ceiling")
}

func TestStripeGC_OnlyWithAStripingSyncBackend(t *testing.T) {
	assert.Nil(t, newStripeGC(nil, nil))
}
