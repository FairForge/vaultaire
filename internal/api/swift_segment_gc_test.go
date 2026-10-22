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

type fakeSegmentReaper struct {
	res   drivers.SwiftSegmentReapResult
	err   error
	grace time.Duration
}

func (f *fakeSegmentReaper) ReapOrphanSegments(_ context.Context, olderThan time.Duration) (drivers.SwiftSegmentReapResult, error) {
	f.grace = olderThan
	return f.res, f.err
}

func TestSwiftSegmentGC_ReportsDeletionsAndNotes(t *testing.T) {
	// Arrange
	f := &fakeSegmentReaper{res: drivers.SwiftSegmentReapResult{Backend: "blomp", PiecesDeleted: 5, MarkersDeleted: 1,
		Unidentified: 2, BudgetStopped: true, Errors: []string{"t-x/_segments/aa/bb/: manifest: 503"}}}
	g := &swiftSegmentGC{r: f, Grace: 6 * time.Hour, JobName: "swift_segment_gc", Every: time.Hour}

	// Act
	rep, err := g.spec().Run(context.Background())

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int64(6), rep.Rows)
	assert.Contains(t, rep.Note, "piece budget")
	assert.Contains(t, rep.Note, "2 generation(s) without a marker")
	assert.Contains(t, rep.Note, "1 generation(s) kept on an error")
	assert.Equal(t, 6*time.Hour, f.grace)
}

func TestSwiftSegmentGC_AListingFailureFailsTheRun(t *testing.T) {
	g := &swiftSegmentGC{r: &fakeSegmentReaper{err: errors.New("blomp list t-: 503")}, Grace: time.Hour, JobName: "swift_segment_gc"}
	_, err := g.spec().Run(context.Background())
	require.Error(t, err)

	g = &swiftSegmentGC{r: &fakeSegmentReaper{err: context.DeadlineExceeded}, Grace: time.Hour, JobName: "swift_segment_gc"}
	rep, err := g.spec().Run(context.Background())
	require.NoError(t, err)
	assert.Contains(t, rep.Note, "run ceiling")
}

func TestSwiftSegmentGC_OnlyWithTheBlompBackend(t *testing.T) {
	assert.Nil(t, newSwiftSegmentGC(nil, nil))
}
