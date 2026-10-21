package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cancelOnLastByte is a client that hangs up the moment it has the whole
// body: aws-cli closes its connection on the last byte, and net/http then
// cancels the request context while the handler is still running.
type cancelOnLastByte struct {
	*httptest.ResponseRecorder
	want    int
	written int
	cancel  context.CancelFunc
}

func (w *cancelOnLastByte) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	w.written += n
	if w.written >= w.want {
		w.cancel()
	}
	return n, err
}

func downloadedEvents(t *testing.T, f *adapterTestFixture, key string) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM events
		WHERE tenant_id = $1 AND type = 'object.downloaded' AND data->>'key' = $2`, f.tenantID, key).Scan(&n))
	return n
}

// getClosingOnLastByte PUTs body at key, then GETs it with a client that
// goes away on the last byte.
func getClosingOnLastByte(t *testing.T, f *adapterTestFixture, key string, body []byte) {
	t.Helper()
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM events WHERE tenant_id = $1`, f.tenantID) })

	put := httptest.NewRequest(http.MethodPut, "/test-bucket/"+key, bytes.NewReader(body))
	put.ContentLength = int64(len(body))
	put = put.WithContext(s3Ctx(put.Context(), f.tenant))
	pw := httptest.NewRecorder()
	f.adapter.HandlePut(pw, put, "test-bucket", key)
	require.Equal(t, http.StatusOK, pw.Code, pw.Body.String())

	ctx, cancel := context.WithCancel(s3Ctx(context.Background(), f.tenant))
	defer cancel()
	get := httptest.NewRequest(http.MethodGet, "/test-bucket/"+key, nil).WithContext(ctx)
	gw := &cancelOnLastByte{ResponseRecorder: httptest.NewRecorder(), want: len(body), cancel: cancel}
	f.adapter.HandleGet(gw, get, "test-bucket", key)

	require.Equal(t, http.StatusOK, gw.Code)
	require.Equal(t, body, gw.Body.Bytes())
	require.Error(t, ctx.Err(), "the client went away before the handler returned")
}

func TestObjectDownloaded_RecordedWhenTheClientClosesOnTheLastByte(t *testing.T) {
	// Arrange
	f := setupAdapterFixture(t)
	body := generateTestData(64 * 1024)

	// Act
	getClosingOnLastByte(t, f, "whole.bin", body)

	// Assert: the event of a download the client finished is recorded.
	assert.Equal(t, 1, downloadedEvents(t, f, "whole.bin"))
}

func TestObjectDownloaded_ChunkedRecordedWhenTheClientClosesOnTheLastByte(t *testing.T) {
	// Arrange
	f := setupChunkingFixture(t)
	body := generateTestData(8 * 1024) // above the fixture's 1 KiB threshold

	// Act
	getClosingOnLastByte(t, f, "chunked.bin", body)

	// Assert
	assert.Equal(t, 1, downloadedEvents(t, f, "chunked.bin"))
}
