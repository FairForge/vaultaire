package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// RP56-4 (the WP-P39 review): while signups are closed /register redirected
// to "/", the top of the landing — so /signup → /register landed a launch
// reader on the hero, not on the waitlist the landing's own CTAs use
// (href="#waitlist"). The redirect goes to /#waitlist; open, the page is
// served. The SignupsEnabled gate itself is unchanged.
func TestRegister_ClosedSignupsGoToTheWaitlist(t *testing.T) {
	for _, open := range []bool{false, true} {
		r, authSvc, _ := setupTestRouter(t)
		authSvc.SetSignupsEnabled(open)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/register", nil))
		if open {
			assert.Equal(t, http.StatusOK, w.Code, "open: the register page")
			assert.Contains(t, w.Body.String(), "Create Account")
			continue
		}
		assert.Equal(t, http.StatusSeeOther, w.Code, "closed")
		assert.Equal(t, "/#waitlist", w.Header().Get("Location"), "closed: the waitlist the landing's CTAs use")
	}
}
