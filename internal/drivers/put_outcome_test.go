package drivers

// RP13-1: a chunk PUT that failed may still commit when the backend got the
// whole body and never answered (RP8-11). The engine tells the caller which
// attempts ended without an answer (engine.PutOutcome); these tests drive the
// real fixed-bucket driver — its deadline client, the SDK — through the real
// engine against the stalling fake S3, and pin the error shapes a stalled, an
// answering and an unreachable backend produce.

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func outcomeEngine(t *testing.T, d engine.Driver) *engine.CoreEngine {
	t.Helper()
	eng := engine.NewEngine(nil, zap.NewNop(), &engine.Config{DefaultBackend: "wasabi"})
	require.NoError(t, eng.AddDriver("wasabi", d))
	return eng
}

func putWithOutcome(ctx context.Context, eng *engine.CoreEngine, key string, body []byte) (*engine.PutOutcome, error) {
	o := &engine.PutOutcome{}
	_, err := eng.Put(ctx, "c", key, bytes.NewReader(body), engine.WithContentLength(int64(len(body))), engine.WithPutOutcome(o))
	return o, err
}

func TestPutOutcome_AStalledBackendIsUnanswered_DeadlineExceeded(t *testing.T) {
	// Arrange: every attempt reads the body and says nothing.
	prev := putDeadlineFloor
	putDeadlineFloor = 200 * time.Millisecond
	t.Cleanup(func() { putDeadlineFloor = prev })
	f := newStallS3()
	f.stall = func(putAttempt, int) bool { return true }
	eng := outcomeEngine(t, newDeadlineDriver(t, f, 200*time.Millisecond))

	// Act
	o, err := putWithOutcome(tctx(), eng, "stall.bin", randomBody(t, 4096))

	// Assert: the deadline client's "no answer … twice", through the
	// engine's wrap: DeadlineExceeded, no HTTP status, unanswered.
	require.Error(t, err)
	t.Logf("stalled backend, engine error: %v", err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ErrorIs(t, err, engine.ErrAllBackendsUnavailable)
	assert.Contains(t, err.Error(), "twice (after one retry on a fresh connection)")
	assert.False(t, engine.PutAnsweredOrUnsent(err))
	assert.True(t, o.MayHaveLanded())
	assert.Equal(t, []string{"wasabi"}, o.Unanswered())
	assert.Len(t, f.attemptsFor("c/stall.bin", 0), 2, "the body reached the backend twice")
}

func TestPutOutcome_TheCallersBoundEndingAStalledPutIsUnanswered(t *testing.T) {
	// Arrange: the driver's own deadline is long; the caller's bound (the
	// chunk store's chunkPutBound) ends the attempt.
	f := newStallS3()
	f.stall = func(putAttempt, int) bool { return true }
	eng := outcomeEngine(t, newDeadlineDriver(t, f, 30*time.Second))
	ctx, cancel := context.WithTimeout(tctx(), 300*time.Millisecond)
	defer cancel()

	// Act
	o, err := putWithOutcome(ctx, eng, "bound.bin", randomBody(t, 4096))

	// Assert
	require.Error(t, err)
	t.Logf("caller's bound, engine error: %v", err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, o.MayHaveLanded())
}

func TestPutOutcome_ACancelAfterTheBodyWentOutIsUnanswered(t *testing.T) {
	// Arrange: the request is cancelled (the client left, a sibling chunk
	// failed) while the backend holds the whole body.
	f := newStallS3()
	f.stall = func(putAttempt, int) bool { return true }
	eng := outcomeEngine(t, newDeadlineDriver(t, f, 30*time.Second))
	ctx, cancel := context.WithCancel(tctx())
	go func() {
		for len(f.attemptsFor("c/cancel.bin", 0)) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	// Act
	o, err := putWithOutcome(ctx, eng, "cancel.bin", randomBody(t, 4096))

	// Assert
	require.Error(t, err)
	t.Logf("cancelled mid-wait, engine error: %v", err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.True(t, o.MayHaveLanded(), "the body had been sent: the backend may commit it")
}

func TestPutOutcome_ACancelBeforeTheAttemptSendsNothing(t *testing.T) {
	f := newStallS3()
	eng := outcomeEngine(t, newDeadlineDriver(t, f, 30*time.Second))
	ctx, cancel := context.WithCancel(tctx())
	cancel()

	o, err := putWithOutcome(ctx, eng, "early.bin", randomBody(t, 4096))

	require.Error(t, err)
	assert.False(t, o.MayHaveLanded(), "no backend was asked")
	assert.Empty(t, f.attemptsFor("c/early.bin", 0))
}

func TestPutOutcome_AnErrorAnswerIsAnswered(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			// Arrange
			f := newStallS3()
			f.failCode = code
			eng := outcomeEngine(t, newDeadlineDriver(t, f, 30*time.Second))

			// Act
			o, err := putWithOutcome(tctx(), eng, "answered.bin", randomBody(t, 1024))

			// Assert
			require.Error(t, err)
			t.Logf("%d answer, engine error: %v", code, err)
			assert.True(t, engine.PutAnsweredOrUnsent(err))
			assert.False(t, o.MayHaveLanded(), "an error answer committed nothing")
		})
	}
}

func TestPutOutcome_ARefusedDialIsUnsent(t *testing.T) {
	// Arrange: an endpoint nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	d, err := NewFixedBucketS3Driver("wasabi", "ak", "sk", "http://"+addr, "us-west-1", "b", zap.NewNop())
	require.NoError(t, err)
	d.setPutTimeout(5 * time.Second)
	eng := outcomeEngine(t, d)

	// Act
	o, err := putWithOutcome(tctx(), eng, "nowhere.bin", randomBody(t, 1024))

	// Assert
	require.Error(t, err)
	t.Logf("refused dial, engine error: %v", err)
	var opErr *net.OpError
	require.True(t, errors.As(err, &opErr), "a dial error through the SDK and the engine")
	assert.True(t, strings.Contains(err.Error(), "connect") || strings.Contains(err.Error(), "refused"))
	assert.False(t, o.MayHaveLanded(), "nothing reached a backend")
}
