package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Prompt 2b.4 E4.2: every bot probe (/xyz.php, /global-protect/login.esp,
// dozens an hour) logged "authentication failed" at Error — with a stack
// trace in production. A refused credential is the client's outcome; the
// signal is vaultaire_auth_failures_total{reason,key_known}.
// Before (eb79aaf): 1 Error entry per request.
func TestS3AuthFailure_IsNeverLoggedAtError(t *testing.T) {
	cases := map[string]func(*http.Request){
		"scanner, no credentials": func(*http.Request) {},
		"unknown key": func(r *http.Request) {
			r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=VKnobody/20260929/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
			r.Header.Set("x-amz-date", "20260929T000000Z")
		},
		"bad presigned URL": func(r *http.Request) {
			r.URL.RawQuery = "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=VKnobody%2F20260929%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260929T000000Z&X-Amz-Expires=60&X-Amz-SignedHeaders=host&X-Amz-Signature=abc"
		},
	}
	for name, sign := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			s, mock, cleanup := newMgmtTestServer(t)
			defer cleanup()
			core, logs := observer.New(zapcore.DebugLevel)
			s.logger = zap.New(core)
			s.router.HandleFunc("/*", s.handleS3Request)
			mock.MatchExpectationsInOrder(false)
			for i := 0; i < 3; i++ {
				mock.ExpectQuery(`.`).WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}))
			}
			req := httptest.NewRequest(http.MethodGet, "/global-protect/login.esp", nil)
			sign(req)
			rec := httptest.NewRecorder()

			// Act
			s.router.ServeHTTP(rec, req)

			// Assert
			assert.GreaterOrEqual(t, rec.Code, 400)
			assert.Less(t, rec.Code, 500)
			assert.Zero(t, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "%v", logs.All())
		})
	}
}
