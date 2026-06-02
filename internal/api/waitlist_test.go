package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func waitlistRequest(email, ip string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/waitlist",
		strings.NewReader("email="+email))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", ip) // unique IP per test → no limiter cross-talk
	return req
}

func TestWaitlist_ValidEmail_Inserts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`INSERT INTO waitlist_signups`).
		WithArgs("alice@example.com", "landing", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	s := &Server{db: db, logger: zap.NewNop()}
	w := httptest.NewRecorder()
	s.handleWaitlistSignup(w, waitlistRequest("alice@example.com", "203.0.113.1"))

	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	assert.Contains(t, w.Body.String(), "ok")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWaitlist_InvalidEmail_400(t *testing.T) {
	s := &Server{logger: zap.NewNop()} // no DB call expected
	w := httptest.NewRecorder()
	s.handleWaitlistSignup(w, waitlistRequest("not-an-email", "203.0.113.2"))

	require.Equal(t, http.StatusBadRequest, w.Result().StatusCode)
}

func TestWaitlist_Duplicate_IsOK(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// ON CONFLICT DO NOTHING → 0 rows affected, but still a success for the visitor.
	mock.ExpectExec(`INSERT INTO waitlist_signups`).
		WithArgs("dup@example.com", "landing", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	s := &Server{db: db, logger: zap.NewNop()}
	w := httptest.NewRecorder()
	s.handleWaitlistSignup(w, waitlistRequest("dup@example.com", "203.0.113.3"))

	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWaitlist_NilDB_Degrades(t *testing.T) {
	s := &Server{logger: zap.NewNop()} // db nil
	w := httptest.NewRecorder()
	require.NotPanics(t, func() {
		s.handleWaitlistSignup(w, waitlistRequest("bob@example.com", "203.0.113.4"))
	})
	require.Equal(t, http.StatusOK, w.Result().StatusCode)
}

func TestWaitlist_NormalizesEmailCase(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`INSERT INTO waitlist_signups`).
		WithArgs("mixed@example.com", "landing", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	s := &Server{db: db, logger: zap.NewNop()}
	w := httptest.NewRecorder()
	s.handleWaitlistSignup(w, waitlistRequest("Mixed@Example.com", "203.0.113.5"))

	require.Equal(t, http.StatusOK, w.Result().StatusCode)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWaitlistLimiter(t *testing.T) {
	l := newWaitlistLimiter(3, time.Hour)
	const now = int64(1_000_000)

	// First 3 from an IP are allowed; the 4th is throttled.
	assert.True(t, l.allow("1.1.1.1", now))
	assert.True(t, l.allow("1.1.1.1", now))
	assert.True(t, l.allow("1.1.1.1", now))
	assert.False(t, l.allow("1.1.1.1", now))

	// A different IP is unaffected.
	assert.True(t, l.allow("2.2.2.2", now))

	// After the window passes, the first IP is allowed again.
	assert.True(t, l.allow("1.1.1.1", now+3601))
}
