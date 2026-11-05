package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-LOG-2: a host's failures go to the control plane as reports — detached,
// at most 20 a minute, the rest counted as suppressed on the next one.
func TestReporter_CapsAndCountsSuppressed(t *testing.T) {
	cp := &fakeCP{}
	r := NewReporter(cp, nil)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	for i := 0; i < 25; i++ {
		r.Send(HostReportItem{Kind: ReportCommitFailed, Detail: strings.Repeat("x", 400)})
	}
	r.Wait()
	require.Len(t, cp.reports, 20, "20 a minute")
	for _, rep := range cp.reports {
		assert.Zero(t, rep.Suppressed)
		require.Len(t, rep.Reports, 1)
		assert.Len(t, rep.Reports[0].Detail, reportDetailMax, "detail cut")
	}
	now = now.Add(61 * time.Second)
	r.Send(HostReportItem{Kind: ReportAdmitFailed})
	r.Wait()
	require.Len(t, cp.reports, 21)
	assert.Equal(t, 5, cp.reports[20].Suppressed, "the five the cap held back")
	var nilR *Reporter
	nilR.Send(HostReportItem{Kind: ReportException}) // a nil reporter sends nothing
}

// The admit failure of a node's write is reported as admit_failed.
func TestHost_AdmitFailureIsReported(t *testing.T) {
	cp := &failingAdmitCP{fakeCP: &fakeCP{}}
	h := &Host{CP: cp, Reports: NewReporter(cp, nil)}
	req := httptest.NewRequest(http.MethodPut, "http://node.test/b/k", strings.NewReader("abc"))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AK/20261105/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=00")
	_, err := h.Write(context.Background(), req, req.Body, 3)
	require.Error(t, err)
	h.Reports.Wait()
	require.Len(t, cp.reports, 1)
	it := cp.reports[0].Reports[0]
	assert.Equal(t, ReportAdmitFailed, it.Kind)
	assert.Equal(t, OpPut, it.Op)
	assert.Equal(t, http.StatusBadGateway, it.Status)
	assert.NotContains(t, it.Detail, "/b/k", "never the path")
}

type failingAdmitCP struct{ *fakeCP }

func (c *failingAdmitCP) Admit(context.Context, AdmitRequest) (AdmitResponse, error) {
	return AdmitResponse{}, &StatusError{Status: http.StatusBadGateway, Msg: "bad gateway"}
}

// The HTTP control plane posts the report to /api/v1/edge/report with the token.
func TestHTTPControlPlane_Report(t *testing.T) {
	var got HostReport
	var path, token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, token = r.URL.Path, r.Header.Get(TokenHeader)
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cp := NewHTTPControlPlane(srv.URL, []byte("tok"), nil)
	require.NoError(t, cp.Report(context.Background(), HostReport{Reports: []HostReportItem{{Kind: ReportException, Detail: "d"}}, Suppressed: 2}))
	assert.Equal(t, "/api/v1/edge/report", path)
	assert.Equal(t, "tok", token)
	assert.Equal(t, 2, got.Suppressed)
	assert.Equal(t, ReportException, got.Reports[0].Kind)
	// A route a build without it answers 404: the host's error, ignored by the reporter.
	srv404 := httptest.NewServer(http.NotFoundHandler())
	defer srv404.Close()
	err := NewHTTPControlPlane(srv404.URL, nil, nil).Report(context.Background(), HostReport{})
	var se *StatusError
	assert.True(t, errors.As(err, &se) && se.Status == http.StatusNotFound)
}
