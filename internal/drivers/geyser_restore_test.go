package drivers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeVail simulates the Spectra Vail gateway's Glacier semantics: GET on an
// evicted object returns 403 InvalidObjectState (measured live 2026-07-29,
// geyser_README.md), HEAD always works and carries x-amz-restore once a
// restore was requested, POST ?restore accepts the recall.
type fakeVail struct {
	restoreCalls  int
	lastBody      string
	restoreHeader string // x-amz-restore value returned on HEAD, "" = none
	restoreStatus int    // status for POST ?restore, default 202
}

func (f *fakeVail) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Has("restore") {
			f.restoreCalls++
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			f.lastBody = string(buf[:n])
			status := f.restoreStatus
			if status == 0 {
				status = http.StatusAccepted
			}
			if status == http.StatusConflict {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>RestoreAlreadyInProgress</Code><Message>Object restore is already in progress</Message></Error>`))
				return
			}
			w.WriteHeader(status)
			return
		}
		switch r.Method {
		case http.MethodHead:
			if f.restoreHeader != "" {
				w.Header().Set("x-amz-restore", f.restoreHeader)
			}
			w.Header().Set("x-amz-storage-class", "GLACIER")
			w.Header().Set("Content-Length", "11")
			w.Header().Set("ETag", `"abc123"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InvalidObjectState</Code><Message>The operation is not valid for the object's storage class</Message></Error>`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
}

func newVailStub(t *testing.T) (*fakeVail, *GeyserDriver) {
	t.Helper()
	f := &fakeVail{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	// The tuned transport caches DNS etc. but follows the test URL fine.
	t.Setenv("VAULTAIRE_TUNED_TRANSPORT", "false")
	d, err := NewGeyserDriver("test-key", "test-secret", "test-bucket", "tenant-x",
		zap.NewNop(), WithGeyserEndpoint(srv.URL))
	require.NoError(t, err)
	return f, d
}

// TestGeyserGet_ArchivedMapsToErrArchived: a 403 InvalidObjectState from Vail
// must surface as the typed engine.ErrArchived, never an opaque error the API
// would turn into a 500.
func TestGeyserGet_ArchivedMapsToErrArchived(t *testing.T) {
	_, d := newVailStub(t)

	_, err := d.Get(context.Background(), "test-bucket", "frozen.bin")
	require.Error(t, err)
	assert.True(t, errors.Is(err, engine.ErrArchived),
		"InvalidObjectState must map to engine.ErrArchived, got: %v", err)

	_, err = d.GetRange(context.Background(), "test-bucket", "frozen.bin", 0, 100)
	require.Error(t, err)
	assert.True(t, errors.Is(err, engine.ErrArchived), "GetRange too, got: %v", err)
}

// TestGeyserRestoreObject: the driver issues RestoreObject with Days and maps
// RestoreAlreadyInProgress to the typed sentinel.
func TestGeyserRestoreObject(t *testing.T) {
	f, d := newVailStub(t)

	require.NoError(t, d.RestoreObject(context.Background(), "test-bucket", "frozen.bin", 2))
	assert.Equal(t, 1, f.restoreCalls)
	assert.Contains(t, f.lastBody, "<Days>2</Days>")

	f.restoreStatus = http.StatusConflict
	err := d.RestoreObject(context.Background(), "test-bucket", "frozen.bin", 2)
	require.Error(t, err)
	assert.True(t, errors.Is(err, engine.ErrRestoreAlreadyInProgress),
		"409 RestoreAlreadyInProgress must map to the sentinel, got: %v", err)
}

// TestGeyserRestoreStatus: HEAD passthrough of x-amz-restore + storage class.
func TestGeyserRestoreStatus(t *testing.T) {
	f, d := newVailStub(t)

	st, err := d.RestoreStatus(context.Background(), "test-bucket", "frozen.bin")
	require.NoError(t, err)
	assert.Empty(t, st.Restore, "no restore requested yet — header absent")
	assert.Equal(t, "GLACIER", st.StorageClass)

	f.restoreHeader = `ongoing-request="true"`
	st, err = d.RestoreStatus(context.Background(), "test-bucket", "frozen.bin")
	require.NoError(t, err)
	assert.Equal(t, `ongoing-request="true"`, st.Restore)
}
