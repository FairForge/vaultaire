package idrive

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-CAP-1 B: the two read-only reseller calls the backend_capacity job
// makes, against the live shapes (the console wraps some answers in `data`,
// the doc shows others bare — both are accepted), and the key's sources.

func TestResellerClient_UsersAndUsageStats(t *testing.T) {
	// Arrange: a fake console that checks the token header and answers the
	// live shapes — /users wrapped in data, /usage_stats bare.
	var gotStats map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("token") != "rk_test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"api_error","code":"unauthorized","message":"invalid token"}}`))
			return
		}
		switch r.URL.Path {
		case "/users":
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "prod@stored.ge", r.URL.Query().Get("email"))
			_, _ = w.Write([]byte(`{"data":[{"email":"prod@stored.ge","storage_used":1.37,"enabled":true}]}`))
		case "/usage_stats":
			assert.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotStats))
			_, _ = w.Write([]byte(`[{"date":"2026-10-01","uploads":1048576,"downloads":2097152,"disk_used":1470000000},
				{"date":"2026-10-02","uploads":0,"downloads":4194304,"disk_used":1470000000}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewResellerClient(srv.URL, "rk_test")

	// Act + Assert: users.
	users, err := c.Users(context.Background(), "prod@stored.ge")
	require.NoError(t, err)
	require.Len(t, users, 1)
	assert.Equal(t, "prod@stored.ge", users[0].Email)
	assert.InDelta(t, 1.37, users[0].StorageUsedGB, 0.001)

	// Usage stats: the request carries email + ISO dates, the days decode.
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	days, err := c.UsageStats(context.Background(), "prod@stored.ge", from, to)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"email": "prod@stored.ge", "date_from": "2026-10-01", "date_to": "2026-10-02"}, gotStats)
	require.Len(t, days, 2)
	assert.Equal(t, int64(2097152), days[0].Downloads)
	assert.Equal(t, int64(1470000000), days[1].DiskUsed)

	// A wrong key is the API's error envelope, not a decode failure.
	bad := NewResellerClient(srv.URL, "wrong")
	_, err = bad.Users(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid token")
	assert.NotContains(t, err.Error(), "rk_test", "the key is never in an error")
}

func TestLoadResellerKey_Precedence(t *testing.T) {
	files := map[string]string{
		"/creds/idrive_reseller_api_key": "rk_cred\n",
		"/tmp/rk.txt":                    "rk_file",
	}
	read := func(path string) ([]byte, error) {
		if v, ok := files[path]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	}
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	// The credential wins; the file next; then the variable; then "".
	key, err := LoadResellerKey(env(map[string]string{"CREDENTIALS_DIRECTORY": "/creds",
		"IDRIVE_RESELLER_API_KEY_FILE": "/tmp/rk.txt", "IDRIVE_RESELLER_API_KEY": "rk_env"}), read)
	require.NoError(t, err)
	assert.Equal(t, "rk_cred", key)
	key, err = LoadResellerKey(env(map[string]string{"IDRIVE_RESELLER_API_KEY_FILE": "/tmp/rk.txt", "IDRIVE_RESELLER_API_KEY": "rk_env"}), read)
	require.NoError(t, err)
	assert.Equal(t, "rk_file", key)
	key, err = LoadResellerKey(env(map[string]string{"IDRIVE_RESELLER_API_KEY": "rk_env"}), read)
	require.NoError(t, err)
	assert.Equal(t, "rk_env", key)
	key, err = LoadResellerKey(env(map[string]string{}), read)
	require.NoError(t, err)
	assert.Empty(t, key)

	// Set but unreadable is an error (a refused boot), never "off".
	_, err = LoadResellerKey(env(map[string]string{"IDRIVE_RESELLER_API_KEY_FILE": "/gone"}), read)
	assert.ErrorIs(t, err, ErrResellerKeySource)
	broken := func(string) ([]byte, error) { return nil, errors.New("permission denied") }
	_, err = LoadResellerKey(env(map[string]string{"CREDENTIALS_DIRECTORY": "/creds"}), broken)
	assert.ErrorIs(t, err, ErrResellerKeySource)
}
