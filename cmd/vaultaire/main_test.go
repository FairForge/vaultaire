package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type recordingShutdowner struct {
	name  string
	order *[]string
	err   error
}

func (r *recordingShutdowner) Shutdown(context.Context) error {
	*r.order = append(*r.order, r.name)
	return r.err
}

// R1-03: the engine closes the shared *sql.DB, so HTTP must drain (and the
// trackers must flush) BEFORE the engine shuts down.
func TestGracefulShutdown_DrainsHTTPBeforeEngineClosesDB(t *testing.T) {
	var order []string
	srv := &recordingShutdowner{name: "server", order: &order}
	eng := &recordingShutdowner{name: "engine", order: &order, err: errors.New("boom")}

	gracefulShutdown(context.Background(), zap.NewNop(), srv, eng)

	assert.Equal(t, []string{"server", "engine"}, order)
}

// R1-02: ListenAndServe returns ErrServerClosed the instant Shutdown closes
// the listener — before in-flight requests drain. main must wait for the
// shutdown sequence instead of treating that return as fatal.
func TestServeUntilShutdown_WaitsForShutdownOnErrServerClosed(t *testing.T) {
	done := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond)
		close(done)
	}()
	start := time.Now()

	err := serveUntilShutdown(func() error { return http.ErrServerClosed }, done)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond, "returned before shutdown finished")
}

func TestServeUntilShutdown_RealFailureIsReturnedImmediately(t *testing.T) {
	bind := errors.New("listen tcp :8000: bind: address already in use")
	never := make(chan struct{})

	err := serveUntilShutdown(func() error { return bind }, never)

	assert.ErrorIs(t, err, bind)
}
