package drivers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Lyve console has no separate backend: account-management actions are
// SigV4-signed (service "iam") form POSTs to iam.global.lyve.seagate.com, and
// RSCustomerDetails answers only for the ROOT key. It is the one probe that
// catches auth-class outages a TCP dial never sees (a revoked/expired key, a
// suspended account). Root gets an intermittent 403 from it, so a single 403
// is retried once before it counts as a failure.

type consoleRecorder struct {
	statuses []int
	calls    atomic.Int32
	last     *http.Request
	lastBody string
}

func newConsoleServer(t *testing.T, statuses ...int) (*consoleRecorder, *httptest.Server) {
	t.Helper()
	rec := &consoleRecorder{statuses: statuses}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(rec.calls.Add(1)) - 1
		body, _ := io.ReadAll(r.Body)
		rec.last = r
		rec.lastBody = string(body)
		status := http.StatusOK
		if n < len(rec.statuses) {
			status = rec.statuses[n]
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`<RSCustomerDetailsResponse/>`))
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func newTestConsoleClient(srv *httptest.Server, retry time.Duration) *LyveConsoleClient {
	return NewLyveConsoleClient("AKIAROOT", "secret",
		WithLyveConsoleEndpoint(srv.URL),
		WithLyveConsoleHTTPClient(srv.Client()),
		WithLyveConsoleRetryDelay(retry))
}

func TestLyveConsole_CustomerDetails_SignedFormPost(t *testing.T) {
	// Arrange
	rec, srv := newConsoleServer(t, http.StatusOK)
	c := newTestConsoleClient(srv, 0)

	// Act
	err := c.CustomerDetails(context.Background(), "v01")

	// Assert
	require.NoError(t, err)
	require.NotNil(t, rec.last)
	assert.Equal(t, http.MethodPost, rec.last.Method)
	assert.Equal(t, "application/x-www-form-urlencoded", rec.last.Header.Get("Content-Type"))
	auth := rec.last.Header.Get("Authorization")
	assert.True(t, strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIAROOT/"), auth)
	assert.Contains(t, auth, "/iam/aws4_request", "console actions are signed as service iam")
	assert.NotEmpty(t, rec.last.Header.Get("X-Amz-Date"))
	assert.NotEmpty(t, rec.last.Header.Get("X-Amz-Content-Sha256"), "payload must be bound into the signature")
	assert.Contains(t, rec.lastBody, "Action=RSCustomerDetails")
	assert.Contains(t, rec.lastBody, "CustomerName=v01")
	assert.Contains(t, rec.lastBody, "Version=2010-05-08")
}

func TestLyveConsole_CustomerDetails_SingleForbiddenIsRetriedOnce(t *testing.T) {
	// Arrange: root intermittently gets 403 from RSCustomerDetails.
	rec, srv := newConsoleServer(t, http.StatusForbidden, http.StatusOK)
	c := newTestConsoleClient(srv, 0)

	// Act
	err := c.CustomerDetails(context.Background(), "v01")

	// Assert
	require.NoError(t, err, "one 403 followed by 200 is healthy")
	assert.Equal(t, int32(2), rec.calls.Load(), "exactly one retry")
}

func TestLyveConsole_CustomerDetails_SustainedForbiddenFails(t *testing.T) {
	// Arrange
	rec, srv := newConsoleServer(t, http.StatusForbidden, http.StatusForbidden, http.StatusOK)
	c := newTestConsoleClient(srv, 0)

	// Act
	err := c.CustomerDetails(context.Background(), "v01")

	// Assert
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLyveConsoleForbidden), "error must be classifiable: %v", err)
	assert.Equal(t, int32(2), rec.calls.Load(), "never more than one retry")
}

func TestLyveConsole_CustomerDetails_ServerErrorNotRetried(t *testing.T) {
	// Arrange
	rec, srv := newConsoleServer(t, http.StatusInternalServerError, http.StatusOK)
	c := newTestConsoleClient(srv, 0)

	// Act
	err := c.CustomerDetails(context.Background(), "v01")

	// Assert
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Equal(t, int32(1), rec.calls.Load(), "only 403 gets the propagation retry")
}

func TestLyveConsole_CustomerDetails_RetryHonoursContext(t *testing.T) {
	// Arrange: a long retry delay, but the caller gives up quickly.
	_, srv := newConsoleServer(t, http.StatusForbidden, http.StatusOK)
	c := newTestConsoleClient(srv, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Act
	start := time.Now()
	err := c.CustomerDetails(ctx, "v01")

	// Assert
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "got %v", err)
	assert.Less(t, time.Since(start), 2*time.Second, "must not sleep through the caller's deadline")
}
