package dashboard

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/produce"
)

// TestLegalLEAssets_AreWhatTheLayoutLoads: the trust host serves exactly
// the static files the pages' layout loads — base.html's {{asset}} URLs and
// the fonts its stylesheet names — so the closed list can neither miss a
// file (an unstyled page) nor carry one the pages do not use.
func TestLegalLEAssets_AreWhatTheLayoutLoads(t *testing.T) {
	base, err := Templates.ReadFile("templates/layouts/base.html")
	require.NoError(t, err)
	css, err := Static.ReadFile("static/css/style.css")
	require.NoError(t, err)
	var want []string
	for _, m := range regexp.MustCompile(`\{\{asset "([^"]+)"\}\}`).FindAllStringSubmatch(string(base), -1) {
		want = append(want, m[1])
	}
	for _, m := range regexp.MustCompile(`url\((/static/[^)]+)\)`).FindAllStringSubmatch(string(css), -1) {
		want = append(want, m[1])
	}
	got := append([]string(nil), LegalLEAssets...)
	sort.Strings(want)
	sort.Strings(got)
	assert.Equal(t, want, got)
}

// TestLegalLE_PagesFollowTheFlag: OFF = the given 404 for both pages and
// every asset; ON = 200 HTML (the assets only when mounted with them).
func TestLegalLE_PagesFollowTheFlag(t *testing.T) {
	for _, on := range []bool{false, true} {
		fl := flags.New(nil, zap.NewNop())
		fl.Register(produce.FlagLegalLEPages, on)
		r := chi.NewRouter()
		NewLegalLE(fl).Mount(r, LegalLENotFound, true)
		for _, p := range append([]string{"/legal/law-enforcement", "/legal/what-we-can-produce"}, LegalLEAssets...) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
			if on {
				assert.Equal(t, http.StatusOK, w.Code, p)
			} else {
				assert.Equal(t, http.StatusNotFound, w.Code, p)
				assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), p)
			}
		}
	}
	// Mounted without assets (the canonical origin serves /static/ itself).
	r := chi.NewRouter()
	NewLegalLE(nil).Mount(r, LegalLENotFound, false)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/legal/law-enforcement", nil))
	assert.Equal(t, http.StatusNotFound, w.Code, "nil flags = OFF")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, LegalLEAssets[0], nil))
	assert.Equal(t, http.StatusNotFound, w.Code, "no asset routes")
}
