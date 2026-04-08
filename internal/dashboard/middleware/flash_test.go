package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetFlash(t *testing.T) {
	w := httptest.NewRecorder()
	SetFlash(w, "success", "Settings saved.")

	cookies := w.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, "flash", cookies[0].Name)
	assert.Contains(t, cookies[0].Value, "Settings+saved")
}

func TestFlashMiddleware_InjectsAndClears(t *testing.T) {
	var captured string

	handler := Flash(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = GetFlash(r.Context(), "success")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{
		Name:  "flash",
		Value: "success=Settings+saved.",
	})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, "Settings saved.", captured)

	// Flash cookie should be cleared.
	cookies := w.Result().Cookies()
	var found bool
	for _, c := range cookies {
		if c.Name == "flash" {
			found = true
			assert.Equal(t, -1, c.MaxAge)
		}
	}
	assert.True(t, found, "flash cookie should be cleared")
}

func TestFlashMiddleware_NoFlash(t *testing.T) {
	var captured string

	handler := Flash(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = GetFlash(r.Context(), "success")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Empty(t, captured)
}

func TestGetFlash_MissingKey(t *testing.T) {
	var captured string

	handler := Flash(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = GetFlash(r.Context(), "error")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{
		Name:  "flash",
		Value: "success=Saved.",
	})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Empty(t, captured)
}

func TestGetFlashMap(t *testing.T) {
	handler := Flash(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := GetFlashMap(r.Context())
		assert.Equal(t, "Done.", m["success"])
		assert.Equal(t, "Oops.", m["error"])
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{
		Name:  "flash",
		Value: "success=Done.&error=Oops.",
	})

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
}

func TestFlash_NilContext(t *testing.T) {
	assert.Empty(t, GetFlash(context.Background(), "success"))
	assert.Empty(t, GetFlashMap(context.Background()))
}
