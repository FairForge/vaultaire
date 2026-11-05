package dataplane

// A host's failure report (WP-LOG-2, D-LOG-2): a data-plane host keeps no
// log of its own (plan 40.10: the control plane's rows are the record), so
// what went wrong on a host goes to the origin as a report —
// POST /api/v1/edge/report — where it is one Warn line and one counter.
// Closed kinds, six fields; never a URL, a path, a key or a header.

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// HostReport is the report call's body.
type HostReport struct {
	Reports []HostReportItem `json:"reports"`
	// Suppressed counts the failures the host's rate cap did not send since
	// its last report.
	Suppressed int `json:"suppressed,omitempty"`
}

// HostReportItem is one failure: exactly these six fields. Detail is the
// error's message (the host cuts it; the origin cuts it again to 512 bytes
// and removes control characters).
type HostReportItem struct {
	Kind      string `json:"kind"`
	RequestID string `json:"request_id"`
	AdmitID   string `json:"admit_id"`
	Op        string `json:"op"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
}

// The report kinds. A kind outside ReportKinds is counted as ReportKindOther;
// a report's Suppressed count as ReportKindSuppressed.
const (
	ReportAdmitFailed          = "admit_failed"
	ReportAdmitUnavailable     = "admit_unavailable"
	ReportCommitFailed         = "commit_failed"
	ReportCodecUnavailable     = "codec_unavailable"
	ReportCodecFailed          = "codec_failed"
	ReportMirrorSettleFailed   = "mirror_settle_failed"
	ReportPostShortBody        = "post_short_body"
	ReportPostUnread           = "post_unread"
	ReportRefused              = "refused"
	ReportCatalogueUnavailable = "catalogue_unavailable"
	ReportException            = "exception"

	ReportKindOther      = "other"
	ReportKindSuppressed = "suppressed"
)

// ReportKinds is the closed list of failure kinds.
var ReportKinds = []string{ReportAdmitFailed, ReportAdmitUnavailable, ReportCommitFailed, ReportCodecUnavailable, ReportCodecFailed,
	ReportMirrorSettleFailed, ReportPostShortBody, ReportPostUnread, ReportRefused, ReportCatalogueUnavailable, ReportException}

// MaxReportItems is the most items one report may carry.
const MaxReportItems = 32

// reportTimeout is one report call's clock; reportsPerMinute the host's cap.
const (
	reportTimeout    = 5 * time.Second
	reportsPerMinute = 20
	reportDetailMax  = 300
)

// Reporter sends a host's failures to the control plane: detached from the
// request (a report never delays an answer), 5 s a call, at most 20 a
// minute — the rest are counted and sent as Suppressed with the next one.
// A nil Reporter sends nothing.
type Reporter struct {
	cp     ControlPlane
	logger *zap.Logger
	now    func() time.Time

	mu         sync.Mutex
	window     time.Time
	sent       int
	suppressed int
	wg         sync.WaitGroup
}

// NewReporter reports to cp (nil cp = nil Reporter).
func NewReporter(cp ControlPlane, logger *zap.Logger) *Reporter {
	if cp == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Reporter{cp: cp, logger: logger, now: time.Now}
}

// Send queues one failure. It never blocks on the call.
func (r *Reporter) Send(item HostReportItem) {
	if r == nil {
		return
	}
	if len(item.Detail) > reportDetailMax {
		item.Detail = cutUTF8(item.Detail, reportDetailMax)
	}
	r.mu.Lock()
	now := r.now()
	if now.Sub(r.window) >= time.Minute {
		r.window, r.sent = now, 0
	}
	if r.sent >= reportsPerMinute {
		r.suppressed++
		r.mu.Unlock()
		return
	}
	r.sent++
	rep := HostReport{Reports: []HostReportItem{item}, Suppressed: r.suppressed}
	r.suppressed = 0
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		defer cancel()
		if err := r.cp.Report(ctx, rep); err != nil {
			// The report is lost: this host's own line is the only trace.
			r.logger.Debug("data plane: a failure report did not reach the control plane", zap.String("kind", item.Kind), zap.Error(err))
		}
	}()
}

// Wait waits for the reports in flight (tests, shutdown).
func (r *Reporter) Wait() {
	if r != nil {
		r.wg.Wait()
	}
}

// cutUTF8 cuts s to at most n bytes on a character boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
