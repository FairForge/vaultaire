package api

// WP-LOG-2 (a data-plane host keeps no log; the control plane's rows are
// the record): the customer's logs are whole (a duration on every row, a row
// for a transfer whose commit never came — EdgeNoCommit), the commit's
// outcome and the pass-through's reason are counted, and a host reports its
// failures to POST /api/v1/edge/report (closed kinds, six fields, one Warn
// line, one counter, no table).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/FairForge/vaultaire/internal/dataplane"
)

// --- D-LOG-4: duration_ms ---

func TestRequestLog_DurationIsRecorded(t *testing.T) {
	f := setupRequestLogFixture(t)

	// An origin PUT.
	w := f.do(http.MethodPut, "/"+f.bucket+"/timed.txt", []byte("timed"), f.ak, f.sk, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	code, _, rows := f.manage("")
	require.Equal(t, http.StatusOK, code)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].DurationMS, "an origin request has its duration")
	assert.GreaterOrEqual(t, *rows[0].DurationMS, int64(0))
	var access sql.NullInt64
	require.NoError(t, f.db.QueryRow(`SELECT duration_ms FROM s3_access_log WHERE tenant_id = $1 AND object_key = 'timed.txt'`, f.tenantID).Scan(&access))
	assert.True(t, access.Valid, "the bucket log's row has it too")

	// A management call.
	h := f.srv.logManagementRequest(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/manage/usage", nil)
	r = r.WithContext(context.WithValue(r.Context(), tenantIDKey, f.tenantID))
	h.ServeHTTP(httptest.NewRecorder(), r)
	f.tracker.Flush()
	_, _, rows = f.manage("")
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.NotNil(t, row.DurationMS, "%s has its duration", row.Op)
		assert.GreaterOrEqual(t, *row.DurationMS, int64(0))
	}

	// A row written without it (the previous build's insert) reads back
	// with the field absent.
	f.exec(`INSERT INTO request_log (tenant_id, key_id, logged_at, op, bucket, object_key, status, error_code, request_id, bytes_in, bytes_out, host)
		VALUES ($1, $2, NOW(), 'GetObject', $3, 'old.txt', 200, '', 'rid-old', 0, 5, 'origin')`, f.tenantID, f.ak, f.bucket)
	_, _, rows = f.manage("request_id=rid-old")
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].DurationMS)
	raw, err := json.Marshal(rows[0])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "duration_ms", "an unmeasured row carries no duration field")
}

func TestAccessLogLine_DurationIsTheLastField(t *testing.T) {
	at := time.Date(2026, 11, 5, 10, 11, 12, 0, time.UTC)
	e := s3AccessEvent{tenantID: "t1", bucket: "b", objectKey: "k", operation: "PutObject", statusCode: 200,
		bytesSent: 0, bytesReceived: 42, sourceIP: "203.0.113.9", userAgent: "aws-cli/2", requestID: "rid1", loggedAt: at, durationMS: 1234}
	// Today's line, byte for byte, then the duration.
	before := `t1 b [05/Nov/2026:10:11:12 +0000] 203.0.113.9 PutObject k 200 - 0 42 rid1 "aws-cli/2"`
	assert.Equal(t, before+" 1234", formatAccessLogLine(e))
	e.durationMS = -1
	assert.Equal(t, before+" -", formatAccessLogLine(e), "unknown = -")
	e.durationMS = 0
	assert.Equal(t, before+" 0", formatAccessLogLine(e), "0 ms is a measurement")
}

func TestEdgeCommit_RecordsTheHostsDuration(t *testing.T) {
	f := setupRequestLogFixture(t)
	body := testBytes(4096)
	resp := f.admit(f.signed("/"+f.bucket+"/dur.bin", nil, body, nil, ""))
	require.True(t, resp.Edge, "%+v", resp)
	etag := f.drv.workerPut(f.tenantID, f.container(), resp.StorageKey, body)
	req := okCommit(resp.AdmitID, body, etag)
	req.DurationMS = 1234
	relay, code := f.commit(req)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, http.StatusOK, relay.Status, relay.Body)
	f.tracker.Flush()
	_, _, rows := f.manage("")
	require.Len(t, rows, 1)
	assert.Equal(t, "edge", rows[0].Host)
	require.NotNil(t, rows[0].DurationMS)
	assert.Equal(t, int64(1234), *rows[0].DurationMS, "the host's duration")
	var access sql.NullInt64
	require.NoError(t, f.db.QueryRow(`SELECT duration_ms FROM s3_access_log WHERE tenant_id = $1 AND object_key = 'dur.bin'`, f.tenantID).Scan(&access))
	assert.Equal(t, int64(1234), access.Int64)
}

// --- D-LOG-3: EdgeNoCommit ---

func TestEdgeReaper_ExpiredAdmitWritesEdgeNoCommit(t *testing.T) {
	f := setupRequestLogFixture(t)
	f.putObject("gone.bin", testBytes(9000), nil)
	f.wireEgress(1<<30, 0, nil)
	f.tracker.Flush()
	f.exec(`DELETE FROM request_log WHERE tenant_id = $1`, f.tenantID) // the origin PUT's row
	resp := f.admit(f.signedGet("/"+f.bucket+"/gone.bin", nil, map[string]string{"Range": "bytes=0-6999"}))
	require.True(t, resp.Edge, "%+v", resp)

	f.exec(`UPDATE edge_admits SET expires_at = NOW() - interval '1 minute' WHERE id = $1`, resp.AdmitID)
	_, err := f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, "expired", f.admitState(resp.AdmitID))
	f.tracker.Flush()
	_, _, rows := f.manage("")
	require.Len(t, rows, 1, "one row for the transfer whose commit never came")
	got := rows[0]
	assert.Equal(t, 0, got.Status)
	assert.Equal(t, ErrEdgeNoCommit, got.ErrorCode)
	assert.Equal(t, int64(7000), got.BytesOut, "what the expiry metered: the admitted size")
	assert.Equal(t, "GetObject", got.Op)
	assert.Equal(t, "gone.bin", got.Key)
	assert.Equal(t, "edge", got.Host)
	assert.Equal(t, f.ak, got.KeyID)
	assert.NotEmpty(t, got.RequestID)
	assert.Equal(t, requestSuggestion(ErrEdgeNoCommit), got.Suggestion)
	assert.Nil(t, got.DurationMS, "no host reported a duration")
	// The bucket log keeps it as an error row (an error code whatever its status).
	var n int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM s3_access_log WHERE tenant_id = $1 AND error_code = $2`, f.tenantID, ErrEdgeNoCommit).Scan(&n))
	assert.Equal(t, 1, n)

	// A host=slc admit writes none: the origin's dispatch wrote the request's row.
	resp2 := f.admit(f.signedGet("/"+f.bucket+"/gone.bin", nil, map[string]string{"Range": "bytes=0-6999"}))
	require.True(t, resp2.Edge)
	f.exec(`UPDATE edge_admits SET host = 'slc', expires_at = NOW() - interval '1 minute' WHERE id = $1`, resp2.AdmitID)
	_, err = f.srv.edgeReaper.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, "expired", f.admitState(resp2.AdmitID))
	f.tracker.Flush()
	_, _, rows = f.manage("")
	assert.Len(t, rows, 1, "no row for the slc admit")
}

func TestEdgeNoCommit_IsAnErrorRowWhateverItsStatus(t *testing.T) {
	at := NewS3AccessLogTracker(nil)
	at.enabledLoaded = true // no bucket is logging-enabled
	assert.True(t, at.shouldRecord(s3AccessEvent{tenantID: "t", bucket: "b", statusCode: 0, errorCode: ErrEdgeNoCommit}))
	assert.False(t, at.shouldRecord(s3AccessEvent{tenantID: "t", bucket: "b", statusCode: 200}))
	assert.True(t, at.shouldRecord(s3AccessEvent{tenantID: "t", bucket: "b", statusCode: 404, errorCode: ErrNoSuchKey}))
}

// --- the counters ---

func commitsCount(op, outcome string) float64 {
	return promtestutil.ToFloat64(dataPlaneCommits.WithLabelValues(edgeHost, op, outcome))
}

func TestEdgeCommit_CountsOutcome(t *testing.T) {
	f := setupEdgeFixture(t)
	body := testBytes(2048)

	ok0, be0, other0, replay0 := commitsCount("put", "ok"), commitsCount("put", "backend_error"), commitsCount("put", "other"), commitsCount("put", "replay")
	r1 := f.admit(f.signed("/"+f.bucket+"/c1.bin", nil, body, nil, ""))
	require.True(t, r1.Edge)
	etag := f.drv.workerPut(f.tenantID, f.container(), r1.StorageKey, body)
	_, code := f.commit(okCommit(r1.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, ok0+1, commitsCount("put", "ok"))

	r2 := f.admit(f.signed("/"+f.bucket+"/c2.bin", nil, body, nil, ""))
	require.True(t, r2.Edge)
	_, code = f.commit(edgeCommitRequest{AdmitID: r2.AdmitID, Outcome: edgeOutcomeBackendError, BackendStatus: 500, BackendCode: "InternalError"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, be0+1, commitsCount("put", "backend_error"))

	r3 := f.admit(f.signed("/"+f.bucket+"/c3.bin", nil, body, nil, ""))
	require.True(t, r3.Edge)
	_, code = f.commit(edgeCommitRequest{AdmitID: r3.AdmitID, Outcome: "something_new", BackendStatus: 200})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, other0+1, commitsCount("put", "other"), "an outcome outside the closed list")

	// A replay of a terminal row.
	_, code = f.commit(okCommit(r1.AdmitID, body, etag))
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, replay0+1, commitsCount("put", "replay"))
	assert.Equal(t, ok0+1, commitsCount("put", "ok"), "a replay is not a second ok")

	// An unknown admit is no accepted commit.
	before := commitsCount("put", "other")
	_, code = f.commit(edgeCommitRequest{AdmitID: "nope", Outcome: edgeOutcomeOK})
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, before, commitsCount("put", "other"))
}

func TestEdgeAdmit_PassThroughReasonIsCounted(t *testing.T) {
	f := setupEdgeFixture(t)
	require.NoError(t, f.fl.Set(context.Background(), flagEdgeIngest, f.tenantID, false, "test"))
	require.NoError(t, f.fl.Refresh(context.Background()))
	before := promtestutil.ToFloat64(dataPlanePassThrough.WithLabelValues(edgeHost, "put", "flag_off"))
	resp := f.admit(f.signed("/"+f.bucket+"/p.bin", nil, testBytes(1024), nil, ""))
	require.False(t, resp.Edge)
	require.Equal(t, "flag_off", resp.Reason)
	assert.Equal(t, before+1, promtestutil.ToFloat64(dataPlanePassThrough.WithLabelValues(edgeHost, "put", "flag_off")))

	// The small pass-through is counted by its reason too.
	small := promtestutil.ToFloat64(dataPlanePassThrough.WithLabelValues(edgeHost, "put", edgeReasonSmall))
	countEdgeAdmit(edgeHost, "put", edgeAdmitResponse{Reason: edgeReasonSmall}, http.StatusOK)
	assert.Equal(t, small+1, promtestutil.ToFloat64(dataPlanePassThrough.WithLabelValues(edgeHost, "put", edgeReasonSmall)))
	// An admitted request and a refusal are not pass-throughs.
	unknown := promtestutil.ToFloat64(dataPlanePassThrough.WithLabelValues(edgeHost, "put", "unknown"))
	countEdgeAdmit(edgeHost, "put", edgeAdmitResponse{Edge: true}, http.StatusOK)
	countEdgeAdmit(edgeHost, "put", edgeAdmitResponse{}, http.StatusOK)
	assert.Equal(t, unknown+1, promtestutil.ToFloat64(dataPlanePassThrough.WithLabelValues(edgeHost, "put", "unknown")), "an empty reason is unknown")
	// A reason naming a backend is labelled by its family (the label stays bounded).
	assert.Equal(t, "backend", passThroughLabel("backend:lyve"))
	assert.Equal(t, "placement_chunked", passThroughLabel("placement_chunked"))
}

// Every pass-through reason is built from constants: a string literal, a
// package constant, a literal + a Kind's String(), a literal + the lower-case
// of a method name, a literal ending in ":" (the label drops what follows),
// or the reason edgeStreamingOf returns (literals only).
func TestEdgePassThroughReasonsAreConstants(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	consts := map[string]bool{}
	var parsed []*ast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)
		parsed = append(parsed, file)
		for _, d := range file.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
				for _, s := range g.Specs {
					for _, n := range s.(*ast.ValueSpec).Names {
						consts[n.Name] = true
					}
				}
			}
		}
	}
	isLit := func(e ast.Expr) (string, bool) {
		b, ok := e.(*ast.BasicLit)
		if !ok || b.Kind != token.STRING {
			return "", false
		}
		return strings.Trim(b.Value, "\"`"), true
	}
	calls := 0
	for _, file := range parsed {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "passThrough" || len(call.Args) != 1 {
				return true
			}
			calls++
			pos := fset.Position(call.Pos()).String()
			switch a := call.Args[0].(type) {
			case *ast.BasicLit:
				_, ok := isLit(a)
				assert.True(t, ok, "%s: a string literal", pos)
			case *ast.Ident:
				assert.True(t, consts[a.Name] || a.Name == "why", "%s: %s is no constant", pos, a.Name)
			case *ast.BinaryExpr:
				lit, ok := isLit(a.X)
				require.True(t, ok && a.Op == token.ADD, "%s: a literal + …", pos)
				switch y := a.Y.(type) {
				case *ast.CallExpr:
					if sel, ok := y.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "String" && len(y.Args) == 0 {
						break // placement.Kind.String(): a closed enum
					}
					sel, ok := y.Fun.(*ast.SelectorExpr)
					assert.True(t, ok && sel.Sel.Name == "ToLower" && len(y.Args) == 1, "%s: an unbounded suffix", pos)
					if ok && len(y.Args) == 1 {
						_, isIdent := y.Args[0].(*ast.Ident)
						assert.True(t, isIdent, "%s: ToLower of a method name", pos)
					}
				default:
					assert.True(t, strings.HasSuffix(lit, ":"), "%s: a variable suffix only after a family prefix ending in ':' (the label drops it)", pos)
				}
			default:
				t.Errorf("%s: a pass-through reason that is not built from constants", pos)
			}
			return true
		})
	}
	assert.Greater(t, calls, 20, "the walk found the call sites")

	// `why` is edgeStreamingOf's: every reason it returns is a literal.
	for _, file := range parsed {
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "edgeStreamingOf" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if ret, ok := n.(*ast.ReturnStmt); ok && len(ret.Results) == 2 {
					_, lit := isLit(ret.Results[1])
					assert.True(t, lit, "%s: edgeStreamingOf returns a literal reason", fset.Position(ret.Pos()))
				}
				return true
			})
		}
	}
}

// --- POST /api/v1/edge/report ---

func reportCount(host, kind string) float64 {
	return promtestutil.ToFloat64(dataPlaneHostReports.WithLabelValues(host, kind))
}

func (f *edgeFixture) report(body []byte, token string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/edge/report", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set(edgeTokenHeader, token)
	}
	w := httptest.NewRecorder()
	f.srv.handleEdgeReport(w, r)
	return w
}

func TestEdgeReport_NotTheEdgeCallerIs404(t *testing.T) {
	f := setupEdgeFixture(t)
	assert.Equal(t, http.StatusNotFound, f.report([]byte(`{"reports":[]}`), "").Code)
	assert.Equal(t, http.StatusNotFound, f.report([]byte(`{"reports":[]}`), "wrong-token-0123456789abcdef").Code)
}

func TestEdgeReport_KindsDetailAndTheWarnLine(t *testing.T) {
	f := setupEdgeFixture(t)
	core, logs := observer.New(zap.WarnLevel)
	f.srv.logger = zap.New(core)

	admitFailed, other, suppressed := reportCount(edgeHost, "admit_failed"), reportCount(edgeHost, "other"), reportCount(edgeHost, "suppressed")
	long := strings.Repeat("é", 400) + "\x00\x1b[31m\n"
	body, err := json.Marshal(dataplane.HostReport{Suppressed: 7, Reports: []dataplane.HostReportItem{
		{Kind: "admit_failed", RequestID: "rid-1", AdmitID: "a1", Op: "put", Status: 502, Detail: "the origin answered 502"},
		{Kind: "rm -rf", RequestID: "rid-2", Op: "get", Detail: long},
	}})
	require.NoError(t, err)
	w := f.report(body, f.token)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{}`, w.Body.String())
	assert.Equal(t, admitFailed+1, reportCount(edgeHost, "admit_failed"))
	assert.Equal(t, other+1, reportCount(edgeHost, "other"), "a kind outside the closed list")
	assert.Equal(t, suppressed+7, reportCount(edgeHost, "suppressed"))

	lines := logs.FilterMessage("edge host report").All()
	require.Len(t, lines, 2)
	allowed := map[string]bool{"host": true, "kind": true, "request_id": true, "admit_id": true, "op": true, "status": true, "detail": true}
	for _, l := range lines {
		for _, fld := range l.Context {
			assert.True(t, allowed[fld.Key], "field %q is not one of the seven", fld.Key)
		}
	}
	ctx := lines[1].ContextMap()
	assert.Equal(t, "other", ctx["kind"])
	assert.Equal(t, edgeHost, ctx["host"])
	detail, _ := ctx["detail"].(string)
	assert.LessOrEqual(t, len(detail), 512, "cut to 512 bytes")
	assert.True(t, strings.HasPrefix(detail, "é"), "cut on a character boundary")
	for _, r := range detail {
		assert.False(t, r < 0x20 || r == 0x7f, "control characters removed: %q", r)
	}
	assert.Equal(t, int64(502), lines[0].ContextMap()["status"])
}

func TestEdgeReport_TooManyItemsIs400(t *testing.T) {
	f := setupEdgeFixture(t)
	items := make([]dataplane.HostReportItem, 33)
	for i := range items {
		items[i] = dataplane.HostReportItem{Kind: "exception"}
	}
	before := reportCount(edgeHost, "exception")
	body, err := json.Marshal(dataplane.HostReport{Reports: items})
	require.NoError(t, err)
	w := f.report(body, f.token)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "too_many")
	assert.Equal(t, before, reportCount(edgeHost, "exception"), "nothing counted")
	// Over 16 KiB is refused too.
	big := []byte(`{"reports":[{"kind":"exception","detail":"` + strings.Repeat("x", 17<<10) + `"}]}`)
	assert.Equal(t, http.StatusBadRequest, f.report(big, f.token).Code)
}

func TestEdgeReport_NeedsNoDatabase(t *testing.T) {
	f := setupEdgeFixture(t)
	f.srv.db = nil
	w := f.report([]byte(`{"reports":[{"kind":"exception","detail":"x"}]}`), f.token)
	assert.Equal(t, http.StatusOK, w.Code)
}

// The in-process control plane takes a report as the route does (host=slc).
func TestEdgeReport_InProcessControlPlane(t *testing.T) {
	f := setupEdgeFixture(t)
	before := reportCount(dataplane.HostSLC, "commit_failed")
	require.NoError(t, f.srv.DataPlane().Report(context.Background(), dataplane.HostReport{Reports: []dataplane.HostReportItem{{Kind: "commit_failed"}}}))
	assert.Equal(t, before+1, reportCount(dataplane.HostSLC, "commit_failed"))
}
