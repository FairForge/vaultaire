package handlers

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
)

// "Access to your account" (Stage 1a Prompt 34b): the tenant's own entries,
// in words, behind translog_customer_view.

// accessPage is the real template under the real layout.
func accessPage(t *testing.T) *template.Template {
	t.Helper()
	base := template.Must(template.New("").Funcs(TemplateFuncs()).ParseFS(os.DirFS("../templates"), "layouts/base.html"))
	tmpl := template.Must(base.Clone())
	template.Must(tmpl.ParseFS(os.DirFS("../templates"), "customer/access.html"))
	return tmpl
}

// accessStore is an adminread store on a private log (103 + 104) and that
// log, which signs.
func accessStore(t *testing.T) (*adminread.Store, *logstore.Store) {
	t.Helper()
	db := logstoretest.PrivateLog(t)
	_, self, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(self), "..", "..", "database", "migrations", "104_translog_subjects.sql")) // #nosec G304 -- the repository's own migration
	require.NoError(t, err)
	_, err = db.Exec(string(raw))
	require.NoError(t, err)
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	log := logstore.New(db, signer)
	return adminread.New(db, log), log
}

func only(tenants ...string) func(string) bool {
	return func(id string) bool {
		for _, t := range tenants {
			if t == id {
				return true
			}
		}
		return false
	}
}

func getAccess(t *testing.T, h http.HandlerFunc, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, injectSessionWithTenant(httptest.NewRequest(http.MethodGet, "/dashboard/settings/access", nil), tenant))
	return w
}

func TestAccessPage_ListsOnlyTheTenantsOwnRows(t *testing.T) {
	// Arrange: A read twice (one before a checkpoint, one after), B once.
	ctx := context.Background()
	s, log := accessStore(t)
	a, b := "tenant-a-"+uuid.NewString(), "tenant-b-"+uuid.NewString()
	iA1, _, err := s.Record(ctx, "op-1", translog.ScopeTenantPayments, a)
	require.NoError(t, err)
	iB, _, err := s.Record(ctx, "op-1", translog.ScopeSupportCase, b)
	require.NoError(t, err)
	_, err = s.RecordPolicy(ctx, translog.FlagTranslogAdminReads, translog.PolicyActionSet, true, a)
	require.NoError(t, err)
	res, err := log.Checkpoint(ctx) // covers the two reads and the switch
	require.NoError(t, err)
	require.True(t, res.Signed)
	signedAt := res.Size
	iA2, _, err := s.Record(ctx, "op-2", translog.ScopeTenantAuditCSV, a)
	require.NoError(t, err)
	h := HandleAccess(accessPage(t), only(a, b), only(a), s, "https://trust.example/translog/checkpoint", func() bool { return true }, zap.NewNop())

	// Act
	w := getAccess(t, h, a)

	// Assert: A sees its three entries in words, newest first; B's is not there.
	require.Equal(t, http.StatusOK, w.Code)
	page := w.Body.String()
	assert.Contains(t, page, "Your plan and payments")
	assert.Contains(t, page, "Recording of operator reads turned on for your account")
	assert.Contains(t, page, "Your account&#39;s audit trail, exported as a file")
	assert.NotContains(t, page, "Your support view", "B's read is not A's")
	assert.NotContains(t, page, fmt.Sprintf("entry %d,", iB))
	assert.Contains(t, page, fmt.Sprintf(`entry %d, included from <a href="https://trust.example/translog/checkpoint" rel="noopener">checkpoint %d</a>`, iA1, signedAt))
	assert.Contains(t, page, fmt.Sprintf("entry %d, not yet signed", iA2))
	assert.Less(t, strings.Index(page, fmt.Sprintf("entry %d,", iA2)), strings.Index(page, fmt.Sprintf("entry %d,", iA1)), "newest first")
	assert.Contains(t, page, "before the records are shown", "recording is on for A")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	// B sees its own one, and the warning: recording is off for B.
	pb := getAccess(t, h, b).Body.String()
	assert.Contains(t, pb, "Your support view")
	assert.NotContains(t, pb, "Your plan and payments")
	assert.Contains(t, pb, "are not being recorded in the log yet")
}

func TestAccessPage_FlagOff_404(t *testing.T) {
	// Arrange: a finder that must never be asked.
	f := &countingFinder{}
	for name, enabled := range map[string]func(string) bool{
		"no flags":           nil,
		"off for the tenant": only("someone-else"),
	} {
		h := HandleAccess(accessPage(t), enabled, nil, f, "https://trust.example/translog/checkpoint", nil, zap.NewNop())

		// Act
		w := getAccess(t, h, "tenant-x")

		// Assert
		assert.Equal(t, http.StatusNotFound, w.Code, name)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"), name)
		assert.NotContains(t, w.Body.String(), "Access to your account", name)
	}
	assert.Zero(t, f.calls, "the log is not read for a tenant with the page off")
	// No session: the login page.
	w := httptest.NewRecorder()
	HandleAccess(accessPage(t), only("t"), nil, f, "", nil, zap.NewNop()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dashboard/settings/access", nil))
	assert.Equal(t, http.StatusSeeOther, w.Code)
}

func TestAccessPage_EscapesHTML(t *testing.T) {
	// Arrange: an entry the words table does not know, carrying markup, and
	// a checkpoint URL with a quote in it (an operator's TRUST_PUBLIC_HOST).
	size := int64(9)
	f := &countingFinder{found: []adminread.Found{
		{Index: 7, Kind: translog.KindAdminRead, Scope: `<script>alert("scope")</script>`, CheckpointSize: &size},
	}}
	h := HandleAccess(accessPage(t), only("t"), only("t"), f, `https://trust.example/x"><script>alert(1)</script>`, func() bool { return true }, zap.NewNop())

	// Act
	w := getAccess(t, h, "t")

	// Assert: the fixed sentence, never the raw value; the URL attribute escaped.
	require.Equal(t, http.StatusOK, w.Code)
	page := w.Body.String()
	assert.Contains(t, page, adminread.Unknown)
	assert.NotContains(t, page, `<script>alert`)
	assert.NotContains(t, page, `alert("scope")`)
	assert.Contains(t, page, `href="https://trust.example/x%22%3e%3cscript%3ealert%281%29%3c/script%3e"`)
}

func TestAccessPage_LogUnreadable_SaysSo(t *testing.T) {
	f := &countingFinder{err: errors.New("database down")}
	w := getAccess(t, HandleAccess(accessPage(t), only("t"), only("t"), f, "", nil, zap.NewNop()), "t")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "cannot be read right now")
	assert.NotContains(t, w.Body.String(), "has been recorded", "never 'no reads' when the log could not be read")
}

type countingFinder struct {
	calls int
	found []adminread.Found
	err   error
}

func (f *countingFinder) Find(context.Context, string) ([]adminread.Found, error) {
	f.calls++
	return f.found, f.err
}

// RP46-3 (rule 8: claim nothing we do not do): while the log is not
// published — translog_public OFF globally, or no trust URL — the page says
// the entries are in our transparency log, not yet published, and links
// nowhere. RP46-4: a signed entry is "included from checkpoint M".
func TestAccessPage_Unpublished_NoPublicClaim(t *testing.T) {
	// Arrange: one signed entry, one not.
	size := int64(9)
	f := &countingFinder{found: []adminread.Found{
		{Index: 7, Kind: translog.KindAdminRead, Scope: translog.ScopeTenantPayments, CheckpointSize: &size},
		{Index: 11, Kind: translog.KindAdminRead, Scope: translog.ScopeTenantAudit},
	}}
	for name, published := range map[string]func() bool{
		"no predicate":        nil,
		"translog_public off": func() bool { return false },
	} {
		h := HandleAccess(accessPage(t), only("t"), only("t"), f, "https://trust.example/translog/checkpoint", published, zap.NewNop())

		// Act
		w := getAccess(t, h, "t")

		// Assert
		require.Equal(t, http.StatusOK, w.Code, name)
		page := w.Body.String()
		assert.NotContains(t, strings.ToLower(page), "public log", name)
		assert.NotContains(t, page, "trust.example", "%s: no link to a log that is not served", name)
		assert.NotContains(t, page, "translog-verify", "%s: nothing to verify against yet", name)
		assert.Contains(t, page, adminread.Where(false).Column, name)
		assert.Contains(t, page, "entry 7, included from checkpoint 9", name)
		assert.Contains(t, page, "entry 11, not yet signed", name)
	}
}

func TestAccessPage_Published_LinksTheCheckpoint(t *testing.T) {
	// Arrange
	size := int64(9)
	f := &countingFinder{found: []adminread.Found{
		{Index: 7, Kind: translog.KindAdminRead, Scope: translog.ScopeTenantPayments, CheckpointSize: &size},
	}}
	h := HandleAccess(accessPage(t), only("t"), only("t"), f, "https://trust.example/translog/checkpoint", func() bool { return true }, zap.NewNop())

	// Act
	w := getAccess(t, h, "t")

	// Assert: the link is the newest checkpoint — the only one served — so
	// it says "included from" M, and how to prove one entry.
	require.Equal(t, http.StatusOK, w.Code)
	page := w.Body.String()
	assert.Contains(t, page, adminread.Where(true).Column)
	assert.Contains(t, page, `entry 7, included from <a href="https://trust.example/translog/checkpoint" rel="noopener">checkpoint 9</a>`)
	assert.Contains(t, page, "translog-verify -url https://trust.example/translog")
	assert.Contains(t, page, "-entry N")
}
