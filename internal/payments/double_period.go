package payments

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
)

// OperatorNotifier takes a note for the operator (the Mailer: an e-mail to
// the support address).
type OperatorNotifier interface {
	NotifyOperator(ctx context.Context, subject, body string) error
}

// ReportDoublePeriod reports a settle the detector flagged (Settled.
// DoublePeriod), AFTER the caller's commit: vaultaire_payments_double_period
// _total{provider}, audit billing.double_period naming both rows, and a
// note to the operator when notifier is an OperatorNotifier (the refund is
// by hand — docs/PAYMENTS.md § 2.5 point 5). Nothing when there is nothing
// to report; the notifier's error is returned for the caller's log line.
//
// A designed overlap (Settled.Forfeit — RP31-2) is no double period: it is
// audited billing.prepaid_forfeited with the days the customer gave up,
// nothing counted, nobody mailed.
func ReportDoublePeriod(ctx context.Context, db *sql.DB, notifier any, tenantID string, s Settled) error {
	f := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	if d := s.Forfeit; d != nil && s.Newly {
		audit.Record(ctx, db, audit.Entry{TenantID: tenantID, EventType: "billing", Action: "billing.prepaid_forfeited",
			Resource: "payment:" + d.ThisID, Metadata: map[string]any{"tenant": tenantID,
				"days": math.Round(d.Days()*100) / 100, "until": f(d.OtherEnd),
				"payment": map[string]any{"id": d.ThisID, "provider": d.ThisProvider, "provider_id": d.ThisProviderID,
					"period_start": f(d.ThisStart), "period_end": f(d.ThisEnd)},
				"prepaid": map[string]any{"id": d.OtherID, "provider": d.OtherProvider, "provider_id": d.OtherProviderID,
					"period_start": f(d.OtherStart), "period_end": f(d.OtherEnd)}}})
	}
	d := s.DoublePeriod
	if d == nil || !s.Newly {
		return nil
	}
	doublePeriodTotal.WithLabelValues(d.ThisProvider).Inc()
	audit.Record(ctx, db, audit.Entry{TenantID: tenantID, EventType: "billing", Action: "billing.double_period", Severity: "error",
		Resource: "payment:" + d.ThisID, Metadata: map[string]any{"tenant": tenantID,
			"payment": map[string]any{"id": d.ThisID, "provider": d.ThisProvider, "provider_id": d.ThisProviderID,
				"period_start": f(d.ThisStart), "period_end": f(d.ThisEnd)},
			"other": map[string]any{"id": d.OtherID, "provider": d.OtherProvider, "provider_id": d.OtherProviderID,
				"period_start": f(d.OtherStart), "period_end": f(d.OtherEnd)}}})
	n, ok := notifier.(OperatorNotifier)
	if !ok || n == nil {
		return nil
	}
	mctx, cancel := Detached(ctx)
	defer cancel()
	body := fmt.Sprintf("Tenant %s paid twice for overlapping dates.\n\n"+
		"%s %s (ledger %s): %s → %s\n%s %s (ledger %s): %s → %s\n\n"+
		"The house was granted (paid is paid). Refund one of them by hand and record it with the admin refund form "+
		"(POST /api/v1/admin/tenants/%s/refunds). Audit: billing.double_period.\n",
		tenantID, d.ThisProvider, d.ThisProviderID, d.ThisID, f(d.ThisStart), f(d.ThisEnd),
		d.OtherProvider, d.OtherProviderID, d.OtherID, f(d.OtherStart), f(d.OtherEnd), tenantID)
	return n.NotifyOperator(mctx, "[Stored billing] Double period for tenant "+tenantID, body)
}
