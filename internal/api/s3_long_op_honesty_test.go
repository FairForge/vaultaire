package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	dbtestutil "github.com/FairForge/vaultaire/internal/testutil"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Long-op honesty (Prompt 2a, PR 3): a refusal must stay a real 4xx however
// long the cheap preconditions take — the keep-alive may commit 200 only once
// the operation has started its slow work (longOpBegin) — and an operation
// that fails after the 200 is committed must show somewhere other than a
// request log that says 200: vaultaire_s3_long_op_total{op,outcome}.

func outcomeCount(op, outcome string) float64 {
	return testutil.ToFloat64(longOpOutcomes.WithLabelValues(op, outcome))
}

// instantCommit makes the keep-alive commit the moment an operation begins
// its slow work: anything decided before longOpBegin keeps its own status,
// anything after is a 200 + body.
func instantCommit(srv *Server) {
	srv.longOpThreshold = time.Nanosecond
	srv.longOpInterval = time.Millisecond
}

func TestRunLongS3Op_ARefusalBeforeBeginKeepsItsStatusHoweverLongItTook(t *testing.T) {
	// Arrange: a 404 decided 120 ms into an operation whose threshold is 30 ms.
	srv := &Server{longOpThreshold: 30 * time.Millisecond, longOpInterval: 10 * time.Millisecond}
	before := outcomeCount("test", longOpErrorBeforeCommit)
	r := httptest.NewRequest("POST", "/b/k", nil)
	w := httptest.NewRecorder()

	// Act
	srv.runLongS3Op(w, r, longOpInfo{Op: "test"}, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(120 * time.Millisecond) // a slow lookup
		WriteS3Error(w, ErrNoSuchKey, "/b/k", "rid")
	})

	// Assert
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>NoSuchKey</Code>")
	assert.Equal(t, before+1, outcomeCount("test", longOpErrorBeforeCommit))
}

func TestRunLongS3Op_OutcomesAreCountedByWhereTheyHappened(t *testing.T) {
	srv := &Server{longOpThreshold: 20 * time.Millisecond, longOpInterval: 5 * time.Millisecond}
	run := func(op func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		srv.runLongS3Op(w, httptest.NewRequest("POST", "/b/k", nil), longOpInfo{Op: "test"}, op)
		return w
	}
	ok0, b0, a0 := outcomeCount("test", longOpOK), outcomeCount("test", longOpErrorBeforeCommit), outcomeCount("test", longOpErrorAfterCommit)

	// ok, fast
	w := run(func(w http.ResponseWriter, r *http.Request) { longOpBegin(r); _, _ = w.Write([]byte("<R/>")) })
	assert.Equal(t, http.StatusOK, w.Code)
	// ok, after the commit
	w = run(func(w http.ResponseWriter, r *http.Request) {
		longOpBegin(r)
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write([]byte("<R/>"))
	})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, strings.HasPrefix(w.Body.String(), " "), "committed: whitespace first")
	// error after the commit: the request log says 200 — the counter does not
	w = run(func(w http.ResponseWriter, r *http.Request) {
		longOpBegin(r)
		time.Sleep(60 * time.Millisecond)
		WriteS3Error(w, ErrServiceUnavailable, "/b/k", "rid")
	})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "<Error>")
	// a panic after the commit is an error after the commit too
	w = run(func(w http.ResponseWriter, r *http.Request) {
		longOpBegin(r)
		time.Sleep(60 * time.Millisecond)
		panic("boom")
	})
	assert.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, ok0+2, outcomeCount("test", longOpOK))
	assert.Equal(t, b0, outcomeCount("test", longOpErrorBeforeCommit))
	assert.Equal(t, a0+2, outcomeCount("test", longOpErrorAfterCommit))
}

func TestRunLongS3Op_AnOperationThatNeverBeginsStillCommitsBeforeCloudflareGivesUp(t *testing.T) {
	// A precondition stuck on a dead dependency: the keep-alive still goes
	// out before the edge's 100 s, after longOpPreludeMax.
	srv := &Server{longOpThreshold: time.Hour, longOpInterval: 5 * time.Millisecond, longOpPreludeMax: 30 * time.Millisecond}
	w := httptest.NewRecorder()
	srv.runLongS3Op(w, httptest.NewRequest("POST", "/b/k", nil), longOpInfo{Op: "test"}, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		WriteS3Error(w, ErrInternalError, "/b/k", "rid")
	})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, strings.HasPrefix(w.Body.String(), " "))
	assert.Contains(t, w.Body.String(), "<Code>InternalError</Code>")
}

// --- CopyObject -------------------------------------------------------------------

func TestCopyObject_ASlowSourceLookupStillAnswersNoSuchKeyAs404(t *testing.T) {
	// Arrange: the source's lookup takes 200 ms, the keep-alive threshold is 50 ms.
	srv, tnt, slow := newSlowMultipartServer(t, 0)
	slow.getDelay = 200 * time.Millisecond
	before := outcomeCount(longOpCopy, longOpErrorBeforeCommit)
	r := httptest.NewRequest("PUT", "/test-bucket/dest.bin", nil)
	r.Header.Set("x-amz-copy-source", "/test-bucket/missing.bin")
	r = r.WithContext(s3Ctx(r.Context(), tnt))
	w := httptest.NewRecorder()

	// Act
	srv.handleS3Request(w, r)

	// Assert: a real 404, not 200 + <Error>
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "<Code>NoSuchKey</Code>")
	assert.Equal(t, before+1, outcomeCount(longOpCopy, longOpErrorBeforeCommit))
}

func TestCopyObject_EveryRefusalPrecedesTheKeepAlive(t *testing.T) {
	srv, tnt, _ := newSlowMultipartServer(t, 0)
	instantCommit(srv)
	putW := doS3Request(srv, tnt, "PUT", "/test-bucket/src.bin", strings.NewReader("source bytes"))
	require.Equal(t, http.StatusOK, putW.Code)
	copyReq := func(src, dest string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/test-bucket/"+dest, nil)
		r.Header.Set("x-amz-copy-source", src)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		r = r.WithContext(s3Ctx(r.Context(), tnt))
		w := httptest.NewRecorder()
		srv.handleS3Request(w, r)
		return w
	}

	cases := []struct {
		name string
		w    *httptest.ResponseRecorder
		code int
		body string
	}{
		{"bad copy source", copyReq("no-leading-slash-or-key", "d1", nil), http.StatusBadRequest, "InvalidRequest"},
		{"missing source", copyReq("/test-bucket/nope.bin", "d2", nil), http.StatusNotFound, "NoSuchKey"},
	}
	for _, c := range cases {
		assert.Equal(t, c.code, c.w.Code, "%s: %s", c.name, c.w.Body.String())
		assert.Contains(t, c.w.Body.String(), c.body, c.name)
	}
	// and the copy itself still works on the committed path
	w := copyReq("/test-bucket/src.bin", "d4", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "<ETag>")
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/d4", nil)
	assert.Equal(t, "source bytes", getW.Body.String())
}

// --- CompleteMultipartUpload -----------------------------------------------------------

func TestCompleteMultipartUpload_EveryRefusalPrecedesTheKeepAlive(t *testing.T) {
	srv, tnt, _ := newSlowMultipartServer(t, 0)
	instantCommit(srv)
	uploadID, body := uploadTwoParts(t, srv, tnt, "pre.bin")
	post := func(path, body string, tn *tenant.Tenant) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		if tn != nil {
			r = r.WithContext(s3Ctx(r.Context(), tn))
		}
		w := httptest.NewRecorder()
		srv.handleCompleteMultipartUpload(w, r, "test-bucket", "pre.bin")
		return w
	}
	other := &tenant.Tenant{ID: "other-tenant", Namespace: "tenant/other-tenant/"}
	wrongETag := strings.Replace(body, "<ETag>", "<ETag>00", 1)
	outOfOrder := "<CompleteMultipartUpload><Part><PartNumber>2</PartNumber><ETag>x</ETag></Part><Part><PartNumber>1</PartNumber><ETag>y</ETag></Part></CompleteMultipartUpload>"

	cases := []struct {
		name string
		w    *httptest.ResponseRecorder
		code int
		body string
	}{
		{"no tenant", post("/test-bucket/pre.bin?uploadId="+uploadID, body, nil), http.StatusForbidden, "AccessDenied"},
		{"unknown upload", post("/test-bucket/pre.bin?uploadId=upload-ffffffffffffffffffffffffffffffff", body, tnt), http.StatusNotFound, "NoSuchUpload"},
		{"another tenant's upload", post("/test-bucket/pre.bin?uploadId="+uploadID, body, other), http.StatusNotFound, "NoSuchUpload"},
		{"malformed body", post("/test-bucket/pre.bin?uploadId="+uploadID, "<Complete", tnt), http.StatusBadRequest, "MalformedXML"},
		{"out-of-order parts", post("/test-bucket/pre.bin?uploadId="+uploadID, outOfOrder, tnt), http.StatusBadRequest, "InvalidPartOrder"},
		{"wrong etag", post("/test-bucket/pre.bin?uploadId="+uploadID, wrongETag, tnt), http.StatusBadRequest, "InvalidPart"},
	}
	for _, c := range cases {
		assert.Equal(t, c.code, c.w.Code, "%s: %s", c.name, c.w.Body.String())
		assert.Contains(t, c.w.Body.String(), c.body, c.name)
	}
	// the real complete still runs on the committed path
	w := post("/test-bucket/pre.bin?uploadId="+uploadID, body, tnt)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "<ETag>")
	getW := doS3Request(srv, tnt, "GET", "/test-bucket/pre.bin", nil)
	assert.Equal(t, "hello, slow world", getW.Body.String())
}

// --- DeleteObjects -------------------------------------------------------------------

func TestDeleteObjects_EveryRefusalPrecedesTheKeepAlive(t *testing.T) {
	srv, tnt, _ := newSlowMultipartServer(t, 0)
	instantCommit(srv)
	post := func(body string, tn *tenant.Tenant) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/test-bucket?delete", strings.NewReader(body))
		if tn != nil {
			r = r.WithContext(s3Ctx(r.Context(), tn))
		}
		w := httptest.NewRecorder()
		srv.handleDeleteObjects(w, r, &S3Request{Bucket: "test-bucket"})
		return w
	}
	var tooMany strings.Builder
	tooMany.WriteString("<Delete>")
	for i := 0; i < 1001; i++ {
		fmt.Fprintf(&tooMany, "<Object><Key>k%d</Key></Object>", i)
	}
	tooMany.WriteString("</Delete>")
	one := "<Delete><Object><Key>a</Key></Object></Delete>"

	cases := []struct {
		name string
		w    *httptest.ResponseRecorder
		code int
		body string
	}{
		{"no tenant", post(one, nil), http.StatusForbidden, "AccessDenied"},
		{"malformed xml", post("<Delete><Object>", tnt), http.StatusBadRequest, "MalformedXML"},
		{"1001 keys", post(tooMany.String(), tnt), http.StatusBadRequest, "MalformedXML"},
	}
	for _, c := range cases {
		assert.Equal(t, c.code, c.w.Code, "%s: %s", c.name, c.w.Body.String())
		assert.Contains(t, c.w.Body.String(), c.body, c.name)
	}
	// the batch itself runs on the committed path
	w := post(one, tnt)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "<Deleted>")
}

// --- the rule file ------------------------------------------------------------------

func TestLongOpRuleFile_MatchesTheExportedSeries(t *testing.T) {
	file, raw := readRuleFile(t, "vaultaire-longops.yml")
	names := map[string]bool{}
	for _, g := range file.Groups {
		for _, r := range g.Rules {
			names[r.Alert] = true
		}
	}
	assert.True(t, names["LongOpFailingAfterCommit"], "a burst of error_after_commit pages")
	assert.True(t, names["LongOpsAbandonedAtShutdown"], "an abandoned operation is reported")
	assert.True(t, names["LongOpsFailedDuringDrain"], "a failure on a stopping slot is reported")
	assert.Contains(t, raw, "vaultaire_s3_long_ops_drain_errors_total")
	assert.Contains(t, raw, `outcome="error_after_commit"`)

	s := &Server{}
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, op := range []string{longOpComplete, longOpCopy, longOpBatch} {
		for _, outcome := range []string{longOpOK, longOpErrorBeforeCommit, longOpErrorAfterCommit} {
			series := fmt.Sprintf(`vaultaire_s3_long_op_total{op="%s",outcome="%s"}`, op, outcome)
			assert.True(t, strings.Contains(body, "\n"+series+" "), "exported from boot: %s", series)
		}
		// The incident counts come from the table: no series while it has
		// never been read (unknown is not zero — Prompt 2a.3 H3).
		assert.NotContains(t, body, fmt.Sprintf("\nvaultaire_s3_long_ops_abandoned_total{op=\"%s\"} ", op))
	}

	// Read once, every op has both series.
	db, err := sql.Open("postgres", dbtestutil.DSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	if db.Ping() != nil {
		t.Skip("test database unreachable")
	}
	w = httptest.NewRecorder()
	(&Server{db: db, logger: zap.NewNop()}).handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	body = w.Body.String()
	for _, op := range []string{longOpComplete, longOpCopy, longOpBatch} {
		assert.True(t, strings.Contains(body, fmt.Sprintf("\nvaultaire_s3_long_ops_abandoned_total{op=\"%s\"} ", op)))
		assert.True(t, strings.Contains(body, fmt.Sprintf("\nvaultaire_s3_long_ops_drain_errors_total{op=\"%s\"} ", op)))
	}
}
