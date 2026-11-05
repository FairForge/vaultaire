package api

// POST /api/v1/edge/report (WP-LOG-2, D-LOG-2): a data-plane host's
// failures. A host keeps no log of a request after it ends — the control
// plane's rows are the record (plan 40.10) — so what went wrong on the
// Worker or a node arrives here as a report: closed kinds
// (dataplane.ReportKinds; any other is `other`), six fields, never a URL, a
// path, a key or a header. Each item is ONE Warn line and one increment of
// vaultaire_data_plane_host_reports_total{host,kind}; the host's
// rate-capped remainder (`suppressed`) adds to kind suppressed. No table, no
// database: the call answers 200 {} whatever the database's state. The
// gate is the edge gate's (edgeCaller), as for the heartbeat.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/dataplane"
)

const (
	edgeReportBodyMax  = 16 << 10
	edgeReportDetail   = 512
	edgeReportFieldMax = 128
	// edgeReportSuppressedMax bounds one report's suppressed count (the
	// host's cap is 20 a minute; a count past a day of failures is noise).
	edgeReportSuppressedMax = 1 << 20
)

var dataPlaneHostReports = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_data_plane_host_reports_total",
	Help: "Failures a data-plane host reported (POST /api/v1/edge/report) by host and kind: admit_failed, admit_unavailable, commit_failed, codec_unavailable, codec_failed, mirror_settle_failed, post_short_body, post_unread, refused, catalogue_unavailable, exception; other (a kind outside the list), suppressed (failures the host's rate cap did not send).",
}, []string{"host", "kind"})

var edgeReportKinds = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range dataplane.ReportKinds {
		m[k] = true
	}
	return m
}()

func init() {
	for _, k := range append(append([]string{}, dataplane.ReportKinds...), dataplane.ReportKindOther, dataplane.ReportKindSuppressed) {
		dataPlaneHostReports.WithLabelValues(edgeHost, k)
	}
}

func (s *Server) handleEdgeReport(w http.ResponseWriter, r *http.Request) {
	host, _, ok := s.edgeCaller(r)
	if !ok {
		s.edgeNotFound(w, r)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, edgeReportBodyMax+1))
	if err != nil || len(raw) > edgeReportBodyMax {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "too_large"})
		return
	}
	var rep dataplane.HostReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if len(rep.Reports) > dataplane.MaxReportItems {
		writeEdgeJSON(w, http.StatusBadRequest, map[string]string{"error": "too_many"})
		return
	}
	s.recordHostReport(host.Name, rep)
	writeEdgeJSON(w, http.StatusOK, struct{}{})
}

// recordHostReport writes a report's lines and counts (the route's body and
// the in-process control plane's).
func (s *Server) recordHostReport(host string, rep dataplane.HostReport) {
	for _, it := range rep.Reports {
		kind := it.Kind
		if !edgeReportKinds[kind] {
			kind = dataplane.ReportKindOther
		}
		s.log().Warn("edge host report", zap.String("host", host), zap.String("kind", kind),
			zap.String("request_id", cleanReportField(it.RequestID, edgeReportFieldMax)),
			zap.String("admit_id", cleanReportField(it.AdmitID, edgeReportFieldMax)),
			zap.String("op", cleanReportField(it.Op, 16)), zap.Int("status", it.Status),
			zap.String("detail", cleanReportField(it.Detail, edgeReportDetail)))
		dataPlaneHostReports.WithLabelValues(host, kind).Inc()
	}
	if n := rep.Suppressed; n > 0 {
		if n > edgeReportSuppressedMax {
			n = edgeReportSuppressedMax
		}
		dataPlaneHostReports.WithLabelValues(host, dataplane.ReportKindSuppressed).Add(float64(n))
	}
}

// cleanReportField removes control characters (a log line stays one line
// and holds no terminal escape) and cuts to max bytes on a character
// boundary.
func cleanReportField(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if len(s) <= max {
		return s
	}
	n := max
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
