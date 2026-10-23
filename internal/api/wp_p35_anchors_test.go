package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
	"github.com/FairForge/vaultaire/internal/translog/ots"
	"github.com/FairForge/vaultaire/internal/translog/ots/otstest"
)

// K4c-3a (Stage 1a Prompt 35; plan 43.6 / F-16 "signed statements +
// OpenTimestamps + a Rekor mirror"): every signed checkpoint gets public,
// independent timestamps nobody has to approve. The translog_anchor job
// submits each one to the calendars of TRANSLOG_ANCHORS and upgrades the
// pending proofs; a calendar that is down never fails the job.

type anchorLab struct {
	store *logstore.Store
	job   *translogAnchorer
	a, b  *otstest.Calendar
}

// newAnchorLab is a private log with a signing key and two calendars.
func newAnchorLab(t *testing.T) *anchorLab {
	t.Helper()
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	store := logstore.New(logstoretest.PrivateLog(t), signer)
	a, b := otstest.New(t), otstest.New(t)
	anchors := []translogAnchor{{Kind: logstore.AnchorKindOTS, Service: a.URL}, {Kind: logstore.AnchorKindOTS, Service: b.URL}}
	logstore.SetAnchors(anchorServices(anchors))
	job := newTranslogAnchorer(store, anchors, zap.NewNop())
	job.JobName = "translog_anchor_test"
	job.upgradeAfter = 0
	return &anchorLab{store: store, job: job, a: a, b: b}
}

// sign appends n entries and signs a checkpoint of them.
func (l *anchorLab) sign(t *testing.T, n int) logstore.Stored {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		_, _, err := l.store.Append(ctx, translog.KeyCeremony{Action: translog.ActionKeyInstalled, KeyRole: translog.KeyTranslogSigningKey,
			KeyFingerprint: "sha256:" + strings.Repeat("3", 64), Participants: i + 1})
		require.NoError(t, err)
	}
	res, err := l.store.Checkpoint(ctx)
	require.NoError(t, err)
	require.True(t, res.Signed, "%+v", res)
	cp, ok, err := l.store.LatestCheckpoint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	return cp
}

func (l *anchorLab) run(t *testing.T) anchorResult {
	t.Helper()
	rep, err := l.job.spec().Run(context.Background())
	require.NoError(t, err, "a calendar never fails the job")
	res, ok := rep.Result.(anchorResult)
	require.True(t, ok)
	return res
}

func TestAnchorJob_SubmitsEachSignedCheckpointOnce(t *testing.T) {
	// Arrange: two signed checkpoints.
	l := newAnchorLab(t)
	cp3 := l.sign(t, 3)
	cp5 := l.sign(t, 2)

	// Act: two runs.
	first := l.run(t)
	second := l.run(t)

	// Assert: each checkpoint went to each calendar once; the digest is
	// SHA-256 of the note exactly as the log signed it.
	assert.Equal(t, int64(2), l.a.Submits.Load())
	assert.Equal(t, int64(2), l.b.Submits.Load())
	assert.Equal(t, 2, first.Anchors[l.a.URL].Submitted)
	assert.Equal(t, logstore.AnchorOK, first.Anchors[l.a.URL].Outcome)
	assert.Zero(t, second.Anchors[l.a.URL].Submitted, "nothing new")
	for _, cp := range []logstore.Stored{cp3, cp5} {
		rows, err := l.store.Anchors(context.Background(), cp.Size, logstore.AnchorKindOTS)
		require.NoError(t, err)
		require.Len(t, rows, 2, "checkpoint %d", cp.Size)
		want := sha256.Sum256([]byte(cp.Note))
		for _, r := range rows {
			assert.Equal(t, logstore.AnchorPending, r.State)
			assert.Equal(t, want[:], r.Digest)
			d, _, err := ots.Describe(r.Proof)
			require.NoError(t, err)
			assert.Equal(t, want[:], d, "the proof is of the note's digest")
		}
	}
}

func TestAnchorJob_ServiceDown_JobOK_Counted(t *testing.T) {
	// Arrange: b is down.
	l := newAnchorLab(t)
	l.sign(t, 2)
	l.b.Down.Store(true)
	before := promtest.ToFloat64(logstore.AnchorCounter(logstore.AnchorKindOTS, l.b.URL, logstore.AnchorUnreachable))

	// Act
	rep, err := l.job.spec().Run(context.Background())

	// Assert: the run is ok; b's outcome is in the result, the note and the counter.
	require.NoError(t, err)
	res := rep.Result.(anchorResult)
	assert.Equal(t, logstore.AnchorUnreachable, res.Anchors[l.b.URL].Outcome)
	assert.Equal(t, 1, res.Anchors[l.a.URL].Submitted, "a is not held back by b")
	assert.Contains(t, rep.Note, l.b.URL)
	assert.Contains(t, rep.Note, "unreachable")
	assert.Equal(t, before+1, promtest.ToFloat64(logstore.AnchorCounter(logstore.AnchorKindOTS, l.b.URL, logstore.AnchorUnreachable)))

	// b back: the next run submits what it missed.
	l.b.Down.Store(false)
	res = l.run(t)
	assert.Equal(t, 1, res.Anchors[l.b.URL].Submitted)
	assert.Zero(t, res.Anchors[l.a.URL].Submitted)
}

func TestAnchorJob_OTSUpgrade_NewRowNeverUpdate(t *testing.T) {
	// Arrange: anchored, pending at both calendars.
	ctx := context.Background()
	l := newAnchorLab(t)
	cp := l.sign(t, 2)
	l.run(t)
	pending, err := l.store.Anchors(ctx, cp.Size, logstore.AnchorKindOTS)
	require.NoError(t, err)

	// Act: a's aggregate reaches a block; two runs.
	l.a.Confirm()
	first := l.run(t)
	second := l.run(t)

	// Assert: a's complete proof is a NEW row; the pending rows are as they were.
	assert.Equal(t, 1, first.Anchors[l.a.URL].Upgraded)
	assert.Zero(t, second.Anchors[l.a.URL].Upgraded, "complete: never asked again")
	assert.Equal(t, 1, second.Anchors[l.b.URL].Pending)
	all, err := l.store.AllAnchors(ctx, cp.Size)
	require.NoError(t, err)
	require.Len(t, all, 3, "two pending, one complete")
	for _, p := range pending {
		assert.Contains(t, all, p, "a pending row is never rewritten")
	}
	served, err := l.store.AnchorOTS(ctx, cp.Size)
	require.NoError(t, err)
	_, atts, err := ots.Describe(served)
	require.NoError(t, err)
	assert.ElementsMatch(t, []ots.Attestation{{BitcoinHeight: 900000}, {Calendar: l.b.URL}}, atts,
		"served: a's complete path and b's pending one")
}

func TestParseTranslogAnchors_AllOrNone(t *testing.T) {
	good := "ots https://alice.btc.calendar.opentimestamps.org\nots https://bob.btc.calendar.opentimestamps.org/"
	got, err := parseTranslogAnchors(good)
	require.NoError(t, err)
	assert.Equal(t, []translogAnchor{
		{Kind: "ots", Service: "https://alice.btc.calendar.opentimestamps.org"},
		{Kind: "ots", Service: "https://bob.btc.calendar.opentimestamps.org"},
	}, got)
	none, err := parseTranslogAnchors("  ")
	require.NoError(t, err)
	assert.Empty(t, none)
	for name, v := range map[string]string{
		"one calendar":        "ots https://alice.btc.calendar.opentimestamps.org",
		"http":                "ots https://a.example; ots http://b.example",
		"a duplicate":         "ots https://a.example; ots https://a.example/",
		"an unknown kind":     "ots https://a.example; ots https://b.example; tsa https://c.example",
		"rekor (not built)":   "ots https://a.example; ots https://b.example; rekor https://log2025-1.rekor.sigstore.dev",
		"a field too many":    "ots https://a.example x; ots https://b.example",
		"a query":             "ots https://a.example?x=1; ots https://b.example",
		"credentials in it":   "ots https://u:p@a.example; ots https://b.example",
		"one bad of three":    "ots https://a.example; ots https://b.example; ots ftp://c.example",
		"more than the bound": manyCalendars(maxTranslogAnchors + 1),
	} {
		got, err := parseTranslogAnchors(v)
		assert.Error(t, err, name)
		assert.Nil(t, got, "%s: all or none", name)
	}
	_, err = parseTranslogAnchors("ots https://a.example; ots https://b.example; rekor https://log2025-1.rekor.sigstore.dev")
	assert.Contains(t, err.Error(), "D-P35-2")
}

func manyCalendars(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("ots https://c" + string(rune('a'+i)) + ".example;")
	}
	return b.String()
}

// Unset: one Info line, the job a no-op that says so and has no anchors key.
func TestAnchorJob_Unset_NoOp(t *testing.T) {
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	store := logstore.New(logstoretest.PrivateLog(t), signer)
	job := newTranslogAnchorer(store, translogAnchorsFromEnv(zap.NewNop(), func(string) string { return "" }), zap.NewNop())
	job.JobName = "translog_anchor_unset_test"

	rep, err := job.spec().Run(context.Background())

	require.NoError(t, err)
	res := rep.Result.(anchorResult)
	assert.Nil(t, res.Anchors)
	assert.Contains(t, rep.Note, "no anchors configured")
}

func TestTrustHost_ServesOTSProofOnlyWhenPublic(t *testing.T) {
	// Arrange: a signed log (size 1) with an anchor, behind the top-level router.
	ctx := context.Background()
	s, fl, _ := trustServer(t, prodHosts())
	cp, ok, err := s.translog.LatestCheckpoint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	cal := otstest.New(t)
	digest := sha256.Sum256([]byte(cp.Note))
	proof, err := ots.NewClient("test").Submit(ctx, cal.URL, digest)
	require.NoError(t, err)
	require.NoError(t, s.translog.AddAnchor(ctx, logstore.Anchor{Size: cp.Size, Kind: logstore.AnchorKindOTS, Service: cal.URL,
		State: logstore.AnchorPending, Proof: proof, Digest: digest[:]}))
	// A row whose digest is not the note's is never served.
	cal2 := otstest.New(t)
	other := sha256.Sum256([]byte("another note"))
	wrong, err := ots.NewClient("test").Submit(ctx, cal2.URL, other)
	require.NoError(t, err)
	require.NoError(t, s.translog.AddAnchor(ctx, logstore.Anchor{Size: cp.Size, Kind: logstore.AnchorKindOTS, Service: cal2.URL,
		State: logstore.AnchorPending, Proof: wrong, Digest: other[:]}))
	h := TrustHostRouter(s.hosts, s.newTrustRouter(), s.router)
	get := func(host, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, hostRequest(http.MethodGet, host, path, false, false))
		return w
	}
	path := "/translog/anchor/" + strconv.FormatInt(cp.Size, 10) + ".ots"

	// Act + Assert: OFF = 404 no-store.
	off := get("trust.stored.cloud", path)
	assert.Equal(t, http.StatusNotFound, off.Code)
	assert.Equal(t, "no-store", off.Header().Get("Cache-Control"))

	// ON: the .ots file of the note's digest.
	require.NoError(t, fl.Set(ctx, flagTranslogPublic, "*", true, "test"))
	t.Cleanup(func() { _ = fl.Unset(context.Background(), flagTranslogPublic, "*") })
	on := get("trust.stored.cloud", path)
	require.Equal(t, http.StatusOK, on.Code, on.Body.String())
	assert.Equal(t, "application/vnd.opentimestamps.v1", on.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", on.Header().Get("Cache-Control"), "a pending proof is upgraded later")
	body, _ := io.ReadAll(on.Body)
	d, atts, err := ots.Describe(body)
	require.NoError(t, err)
	assert.Equal(t, digest[:], d)
	assert.Equal(t, []ots.Attestation{{Calendar: cal.URL}}, atts)
	// A checkpoint with no anchor, a size never signed, a malformed name: 404.
	for _, p := range []string{"/translog/anchor/99.ots", "/translog/anchor/0.ots", "/translog/anchor/x.ots", "/translog/anchor/1"} {
		assert.Equal(t, http.StatusNotFound, get("trust.stored.cloud", p).Code, p)
	}
	// Signed: 404 on the trust host; the canonical host's /translog/anchor is the S3 API's.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, hostRequest(http.MethodGet, "trust.stored.cloud", path, true, false))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, http.StatusTeapot, get("stored.cloud", path).Code, "the S3 API's on any other host")
}
