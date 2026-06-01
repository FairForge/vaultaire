package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/email"
	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v75"
	"go.uber.org/zap"
)

// Per-TB storage prices for the two metered tiers (USD/TB/month). Fixed-price
// Vault packs and the free tier are never metered.
const (
	pricePerTBStandard    = 3.99
	pricePerTBPerformance = 6.00
	bytesPerTB            = 1024.0 * 1024 * 1024 * 1024
)

// MeteredRatePerTB returns the per-TB storage price for a metered tier, or 0 for
// non-metered tiers (vault*/free).
func MeteredRatePerTB(tier string) float64 {
	switch tier {
	case "standard":
		return pricePerTBStandard
	case "performance":
		return pricePerTBPerformance
	default:
		return 0
	}
}

// AccruedCents estimates the current-month metered charge in cents from a storage
// gauge (bytes) and month-to-date egress (bytes). Egress is currently free on
// metered tiers, so only storage contributes — but the signature carries egress
// so the rate can change without touching callers.
func AccruedCents(tier string, storageBytes, egressBytes int64) int64 {
	storageTB := float64(storageBytes) / bytesPerTB
	dollars := storageTB * MeteredRatePerTB(tier)
	// egress is $0/TB on metered tiers today; reported to Stripe for tracking only.
	return int64(math.Round(dollars * 100))
}

// meterSender sends a single Stripe Billing Meter event. Abstracted so tests can
// substitute a fake for the live HTTP call.
type meterSender interface {
	send(ctx context.Context, eventName, customerID string, value int64, identifier string, ts time.Time) (string, error)
}

// MeteredReporter reports daily storage and egress usage for metered-tier tenants
// to Stripe Billing Meters, and alerts on optional per-tenant spending caps.
type MeteredReporter struct {
	stripe       *StripeService
	db           *sql.DB
	logger       *zap.Logger
	storageMeter string
	egressMeter  string
	sender       meterSender
	emailer      email.Sender
}

// NewMeteredReporter constructs a reporter. The live sender reads the Stripe API
// key from the SDK global set by NewStripeService — v75 predates the Billing
// Meter Events API, so events are POSTed raw to the stable /v1/billing/meter_events
// endpoint. Nil-safe on stripe/db (StartMeteredReporting then no-ops).
func NewMeteredReporter(stripeSvc *StripeService, db *sql.DB, logger *zap.Logger, storageMeter, egressMeter string) *MeteredReporter {
	return &MeteredReporter{
		stripe:       stripeSvc,
		db:           db,
		logger:       logger,
		storageMeter: storageMeter,
		egressMeter:  egressMeter,
		sender: &httpMeterSender{
			apiKey:  stripe.Key,
			client:  &http.Client{Timeout: 15 * time.Second},
			baseURL: "https://api.stripe.com",
		},
	}
}

// SetEmailSender wires the sender used for spending-cap alerts. Optional and
// nil-safe — cap alerts are still recorded as events without it.
func (r *MeteredReporter) SetEmailSender(s email.Sender) { r.emailer = s }

// StartMeteredReporting launches a goroutine that reports the previous day's usage
// once per UTC day (at hour 0) and checks spending caps hourly. It mirrors
// CDNAnalyticsTracker.StartRollup / auth.StartSTSCleanup. Nil-safe.
func (r *MeteredReporter) StartMeteredReporting(ctx context.Context) {
	if r == nil || r.db == nil || r.stripe == nil {
		return
	}
	go func() {
		// Catch-up on startup: yesterday's usage is complete and the report is
		// idempotent, so this safely backfills a missed midnight run.
		r.runDaily(ctx)
		r.checkSpendingCaps(ctx)

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Now().UTC().Hour() == 0 {
					r.runDaily(ctx)
				}
				r.checkSpendingCaps(ctx)
			}
		}
	}()
}

// runDaily reports the previous (complete) UTC day's usage. Reporting whole-day
// values keyed by date — not deltas — is what makes the job safe to re-run.
func (r *MeteredReporter) runDaily(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	yesterday := time.Now().UTC().AddDate(0, 0, -1)
	if err := r.ReportDaily(cctx, yesterday); err != nil {
		r.logger.Error("metered daily report", zap.Error(err))
	}
}

// ReportDaily reports storage (a point-in-time gauge) and egress (a daily sum) for
// every metered-tier tenant to Stripe, keyed by date. Idempotent: an existing row
// in metered_usage_reports for (tenant, meter, date) means it was already sent, so
// the Stripe call is skipped. Tenants without a Stripe customer are skipped.
func (r *MeteredReporter) ReportDaily(ctx context.Context, date time.Time) error {
	if r.db == nil {
		return nil
	}
	day := date.UTC().Format("2006-01-02")

	rows, err := r.db.QueryContext(ctx, `
		SELECT tq.tenant_id, tq.storage_used_bytes, COALESCE(t.stripe_customer_id, '')
		FROM tenant_quotas tq
		JOIN tenants t ON t.id = tq.tenant_id
		WHERE tq.tier IN ('standard', 'performance')`)
	if err != nil {
		return fmt.Errorf("query metered tenants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type tenantUsage struct {
		id           string
		storageBytes int64
		customerID   string
	}
	var tenants []tenantUsage
	for rows.Next() {
		var tu tenantUsage
		if err := rows.Scan(&tu.id, &tu.storageBytes, &tu.customerID); err != nil {
			r.logger.Error("scan metered tenant", zap.Error(err))
			continue
		}
		tenants = append(tenants, tu)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate metered tenants: %w", err)
	}

	for _, tu := range tenants {
		if tu.customerID == "" {
			continue // no Stripe customer — nothing to bill
		}

		// Storage: report the current gauge value (Stripe meter aggregates "last").
		r.reportMeter(ctx, tu.customerID, tu.id, r.storageMeter, tu.storageBytes, date, day)

		// Egress: sum the day's bytes (Stripe meter aggregates "sum").
		var egress int64
		if err := r.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(egress_bytes), 0)
			FROM bandwidth_usage_daily
			WHERE tenant_id = $1 AND date = $2`, tu.id, day).Scan(&egress); err != nil {
			r.logger.Error("sum egress", zap.String("tenant", tu.id), zap.Error(err))
			continue
		}
		r.reportMeter(ctx, tu.customerID, tu.id, r.egressMeter, egress, date, day)
	}
	return nil
}

// reportMeter sends one meter event then records it. The existence check skips the
// Stripe call on re-runs; the Stripe identifier is a second idempotency layer so a
// crash between send and record cannot double-bill (re-send dedupes by identifier).
func (r *MeteredReporter) reportMeter(ctx context.Context, customerID, tenantID, meter string, value int64, ts time.Time, day string) {
	if meter == "" {
		return
	}

	var existing int
	err := r.db.QueryRowContext(ctx,
		`SELECT 1 FROM metered_usage_reports WHERE tenant_id = $1 AND meter = $2 AND period_date = $3`,
		tenantID, meter, day).Scan(&existing)
	if err == nil {
		return // already reported for this day
	}
	if !errors.Is(err, sql.ErrNoRows) {
		r.logger.Error("check metered report", zap.String("tenant", tenantID), zap.Error(err))
		return
	}

	identifier := fmt.Sprintf("%s-%s-%s", tenantID, meter, day)
	eventID, sendErr := r.sender.send(ctx, meter, customerID, value, identifier, ts)
	if sendErr != nil {
		// No row written → the next run retries; nothing was billed.
		r.logger.Error("send meter event",
			zap.String("tenant", tenantID), zap.String("meter", meter), zap.Error(sendErr))
		return
	}

	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO metered_usage_reports (tenant_id, meter, period_date, value, stripe_event_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, meter, period_date) DO NOTHING`,
		tenantID, meter, day, value, eventID); err != nil {
		r.logger.Error("record metered report", zap.String("tenant", tenantID), zap.Error(err))
	}
}

// checkSpendingCaps alerts tenants whose month-to-date accrued charge crosses 80%
// or 95% of their optional spending cap. Each threshold fires once per month,
// guarded by a synthetic row in metered_usage_reports.
func (r *MeteredReporter) checkSpendingCaps(ctx context.Context) {
	if r.db == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	monthStart := firstOfMonthUTC(time.Now().UTC())

	rows, err := r.db.QueryContext(cctx, `
		SELECT tenant_id, tier, spending_cap_cents
		FROM tenant_quotas
		WHERE tier IN ('standard', 'performance') AND spending_cap_cents > 0`)
	if err != nil {
		r.logger.Error("query spending caps", zap.Error(err))
		return
	}
	defer func() { _ = rows.Close() }()

	type capRow struct {
		tenantID string
		tier     string
		capCents int64
	}
	var caps []capRow
	for rows.Next() {
		var c capRow
		if err := rows.Scan(&c.tenantID, &c.tier, &c.capCents); err != nil {
			r.logger.Error("scan spending cap", zap.Error(err))
			continue
		}
		caps = append(caps, c)
	}
	if err := rows.Err(); err != nil {
		r.logger.Error("iterate spending caps", zap.Error(err))
		return
	}

	for _, c := range caps {
		accrued := r.accruedCents(cctx, c.tenantID, c.tier, monthStart)
		for _, th := range []struct {
			pct   int64
			label string
		}{{80, "alert:80"}, {95, "alert:95"}} {
			// accrued >= cap * pct/100, rearranged to avoid float rounding.
			if accrued*100 >= c.capCents*th.pct {
				r.fireCapAlert(cctx, c.tenantID, th.label, th.pct, accrued, c.capCents, monthStart)
			}
		}
	}
}

// accruedCents computes month-to-date accrued charge in cents from what has been
// reported to Stripe (the billed-to-date source): the latest storage gauge plus
// the summed egress for the current month.
func (r *MeteredReporter) accruedCents(ctx context.Context, tenantID, tier string, monthStart time.Time) int64 {
	month := monthStart.Format("2006-01-02")
	var storageBytes, egressBytes int64
	err := r.db.QueryRowContext(ctx, `
		SELECT
			COALESCE((SELECT value FROM metered_usage_reports
			          WHERE tenant_id = $1 AND meter = $2 AND period_date >= $3
			          ORDER BY period_date DESC LIMIT 1), 0),
			COALESCE((SELECT SUM(value) FROM metered_usage_reports
			          WHERE tenant_id = $1 AND meter = $4 AND period_date >= $3), 0)`,
		tenantID, r.storageMeter, month, r.egressMeter).Scan(&storageBytes, &egressBytes)
	if err != nil {
		r.logger.Error("accrued charges", zap.String("tenant", tenantID), zap.Error(err))
		return 0
	}
	return AccruedCents(tier, storageBytes, egressBytes)
}

// fireCapAlert records a once-per-month guard row and, only if newly inserted,
// emits a billing event and sends an email (if an emailer is wired).
func (r *MeteredReporter) fireCapAlert(ctx context.Context, tenantID, label string, pct, accrued, capCents int64, monthStart time.Time) {
	month := monthStart.Format("2006-01-02")
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO metered_usage_reports (tenant_id, meter, period_date, value)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, meter, period_date) DO NOTHING`,
		tenantID, label, month, accrued)
	if err != nil {
		r.logger.Error("guard cap alert", zap.String("tenant", tenantID), zap.Error(err))
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // already alerted this month
	}

	r.insertEvent(ctx, "billing.spending_cap_alert", tenantID, map[string]interface{}{
		"threshold_pct": pct,
		"accrued_cents": accrued,
		"cap_cents":     capCents,
	})

	if r.emailer == nil {
		return
	}
	to := r.tenantEmail(ctx, tenantID)
	if to == "" {
		return
	}
	subject := fmt.Sprintf("You've used %d%% of your stored.ge spending cap", pct)
	body := fmt.Sprintf(
		"Your stored.ge usage has reached $%.2f of your $%.2f monthly spending cap (%d%%).",
		float64(accrued)/100, float64(capCents)/100, pct)
	if err := r.emailer.Send(ctx, to, subject, body, body); err != nil {
		r.logger.Warn("send cap alert email", zap.String("tenant", tenantID), zap.Error(err))
	}
}

// insertEvent records an event row directly (billing cannot import the api package
// where emitEvent lives — that would be an import cycle). The events table has no
// type constraint, so a billing-specific type is valid; it is not webhook-dispatched.
func (r *MeteredReporter) insertEvent(ctx context.Context, eventType, tenantID string, data map[string]interface{}) {
	if r.db == nil {
		return
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		r.logger.Error("marshal event data", zap.Error(err))
		return
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO events (id, type, tenant_id, data) VALUES ($1, $2, $3, $4)`,
		uuid.New().String(), eventType, tenantID, dataJSON); err != nil {
		r.logger.Error("insert billing event", zap.String("type", eventType), zap.Error(err))
	}
}

func (r *MeteredReporter) tenantEmail(ctx context.Context, tenantID string) string {
	var e sql.NullString
	if err := r.db.QueryRowContext(ctx,
		`SELECT email FROM tenants WHERE id = $1`, tenantID).Scan(&e); err != nil {
		return ""
	}
	if e.Valid {
		return e.String
	}
	return ""
}

func firstOfMonthUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// httpMeterSender POSTs meter events to the stable /v1/billing/meter_events REST
// endpoint (the v75 SDK has no typed support for it).
type httpMeterSender struct {
	apiKey  string
	client  *http.Client
	baseURL string
}

func (h *httpMeterSender) send(ctx context.Context, eventName, customerID string, value int64, identifier string, ts time.Time) (string, error) {
	form := url.Values{}
	form.Set("event_name", eventName)
	form.Set("identifier", identifier)
	form.Set("timestamp", strconv.FormatInt(ts.UTC().Unix(), 10))
	form.Set("payload[stripe_customer_id]", customerID)
	form.Set("payload[value]", strconv.FormatInt(value, 10))

	endpoint := strings.TrimRight(h.baseURL, "/") + "/v1/billing/meter_events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build meter event request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+h.apiKey)

	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("post meter event: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("stripe meter event %s: status %d: %s",
			eventName, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Identifier string `json:"identifier"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Identifier != "" {
		return parsed.Identifier, nil
	}
	return identifier, nil // event sent; fall back to our identifier
}
