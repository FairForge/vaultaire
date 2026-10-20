package api

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The webhook outbox (WP-R11-3, Stage 1a Prompt 16 Part B; R11-11; migration
// 095). A delivery was ONE attempt: retry_count was always 0, next_retry_at
// never written, and a receiver down for a deploy lost the event for good.
//
// One webhook_outbox row per (event, endpoint) is the durable intent. It is
// written where the delivery pool job is enqueued (submitEventDelivery, a
// batch's deliverInOrder) with next_attempt_at = now + webhookAttemptLease:
// the pool job is attempt 1, and the lease is what retries an attempt a
// crashed process never finished (and keeps the retry job from doubling one
// that is queued or running). Every attempt that ends — 2xx, non-2xx, a
// transport error, the job's deadline, the shutdown cut — goes through
// finishWebhookAttempt: ONE statement moves the outbox row (attempts + 1;
// delivered, or due again on the backoff, or failed after the fifth
// attempt) and writes the webhook_deliveries row with that attempt's
// retry_count and next_retry_at. An attempt the pool never ran (overloaded,
// shutdown, lookup failed) is not an attempt: insertDroppedDeliveries writes
// its failed deliveries row as before and makes the outbox row due again
// (webhookDroppedRetryAfter), attempts unchanged. A webhook disabled or
// deleted while a row waits makes the row `skipped` (the endpoint delete
// marks its rows before the FK nulls endpoint_id).
//
// The `webhook_retry` job (every 30 s, one scheduler) takes due rows with
// FOR UPDATE SKIP LOCKED, leases them, and hands them to the SAME pool —
// per-tenant fairness, per-job deadline and drop accounting apply to a
// retry exactly as to attempt 1.

const (
	// webhookMaxAttempts is how many POSTs an event gets per endpoint.
	webhookMaxAttempts = 5
	// webhookAttemptLease: how long a queued or running attempt holds its
	// row before the retry job may take it again (a crashed process's
	// attempt). Longer than any queue wait the fair pool allows in practice
	// plus webhookDeliveryTimeout; a duplicate past it is at-least-once,
	// which receivers handle by X-Event-ID.
	webhookAttemptLease = 15 * time.Minute
	// webhookDroppedRetryAfter: a dropped attempt (the pool was full or
	// stopping) is due again this much later, without spending an attempt.
	webhookDroppedRetryAfter = 30 * time.Second
	// webhookRetryEvery is the retry job's interval; webhookRetryBatch the
	// rows one run hands to the pool.
	webhookRetryEvery = 30 * time.Second
	webhookRetryBatch = 500
	// webhookRetryJobName is the job's name in job_runs and on the metrics.
	webhookRetryJobName = "webhook_retry"
	// webhookLastErrorMax bounds what the outbox keeps of an answer.
	webhookLastErrorMax = 500
)

// webhookBackoff is the wait after attempt n (index n-1) before attempt
// n+1, jittered ±20 %: 1 s, 5 s, 25 s, 125 s. There are webhookMaxAttempts-1
// waits; the prompt's fifth step (625 s) would follow the fifth attempt,
// which is the last — so it is not used.
var webhookBackoff = []time.Duration{1 * time.Second, 5 * time.Second, 25 * time.Second, 125 * time.Second}

// webhookNow and webhookJitter are the outbox's clock and jitter (tests
// replace them: no sleeps). Every outbox statement takes its time from
// webhookNow, never NOW().
var (
	webhookNow    = time.Now
	webhookJitter = func() float64 { return 0.8 + 0.4*rand.Float64() } // #nosec G404 -- retry spread, not a secret
)

// Outbox states.
const (
	outboxPending   = "pending"
	outboxDelivered = "delivered"
	outboxFailed    = "failed"
	outboxSkipped   = "skipped"
)

// webhookRetries counts how attempts with an outbox row ended: delivered on
// a retry (attempt ≥ 2), retry (failed, another attempt scheduled),
// exhausted (the last attempt failed).
var webhookRetries = func() *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_webhook_retries_total",
		Help: "Webhook attempts that have an outbox row, by how they ended: delivered (a retry, attempt 2 or later, got a 2xx), retry (the attempt failed and another is scheduled on the 1/5/25/125 s backoff), exhausted (the fifth attempt failed; the row is terminal failed).",
	}, []string{"outcome"})
	for _, o := range []string{"delivered", "retry", "exhausted"} {
		c.WithLabelValues(o)
	}
	return c
}()

func backoffSeconds() []float64 {
	out := make([]float64, len(webhookBackoff))
	for i, d := range webhookBackoff {
		out[i] = d.Seconds()
	}
	return out
}

// recordWebhookIntents writes the outbox rows of deliveries about to be
// submitted (attempt 1 leased), in one statement; a row that exists is
// left alone. A failure is logged: attempt 1 still goes out, it just has no
// retry behind it.
func recordWebhookIntents(ctx context.Context, db *sql.DB, logger *zap.Logger, tenantID string, rows []droppedDelivery) {
	if db == nil || len(rows) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	ids, hooks, events := make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows))
	for i, r := range rows {
		ids[i], hooks[i], events[i] = uuid.New().String(), r.webhookID, r.eventID
	}
	now := webhookNow()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO webhook_outbox (id, tenant_id, endpoint_id, event_id, state, attempts, next_attempt_at, created_at, updated_at)
		SELECT DISTINCT ON (u.hook, u.event) u.id, $4, u.hook, u.event, 'pending', 0, $5::timestamptz, $6::timestamptz, $6::timestamptz
		FROM unnest($1::text[], $2::text[], $3::text[]) AS u(id, hook, event)
		ON CONFLICT (event_id, endpoint_id) DO NOTHING`,
		pq.Array(ids), pq.Array(hooks), pq.Array(events), tenantID, now.Add(webhookAttemptLease), now); err != nil {
		logger.Warn("webhook outbox rows not written; the first attempt has no retry behind it",
			zap.Error(err), zap.String("tenant_id", tenantID), zap.Int("rows", len(rows)))
	}
}

// finishWebhookAttempt records one finished attempt: the outbox row moves
// (when the attempt has one — rows from before 095 or a stray re-send have
// none) and the webhook_deliveries row is written with that attempt's
// retry_count (attempt - 1) and next_retry_at, in one statement.
func finishWebhookAttempt(ctx context.Context, db *sql.DB, logger *zap.Logger, deliveryID, webhookID, eventID, status string, responseCode int, responseBody string, latencyMs int) {
	// Detached: the delivery's own deadline (or the shutdown cut) is what
	// usually ended it, and the rows must still be written.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	lastErr := responseBody
	if len(lastErr) > webhookLastErrorMax {
		lastErr = lastErr[:webhookLastErrorMax]
	}
	var attempts sql.NullInt64
	var state sql.NullString
	err := db.QueryRowContext(ctx, `
		WITH o AS (
			UPDATE webhook_outbox SET
				attempts = attempts + 1,
				state = CASE WHEN $4 = 'delivered' THEN 'delivered'
				             WHEN attempts + 1 >= $9 THEN 'failed' ELSE 'pending' END,
				next_attempt_at = CASE WHEN $4 = 'delivered' OR attempts + 1 >= $9 THEN NULL
				             ELSE $8::timestamptz + make_interval(secs => ($10::float8[])[attempts + 1] * $11) END,
				delivered_at = CASE WHEN $4 = 'delivered' THEN $8::timestamptz END,
				last_status = $5, last_error = $12, updated_at = $8
			WHERE event_id = $3 AND endpoint_id = $2 AND state = 'pending'
			RETURNING attempts, state, next_attempt_at
		), d AS (
			INSERT INTO webhook_deliveries (id, webhook_id, event_id, status, response_code, response_body, latency_ms, retry_count, next_retry_at, created_at)
			SELECT $1, $2, $3, $4, $5, $6, $7, COALESCE(o.attempts - 1, 0), o.next_attempt_at, $8
			FROM (SELECT 1) one LEFT JOIN o ON TRUE
			RETURNING 1
		)
		SELECT o.attempts, o.state FROM (SELECT 1) one LEFT JOIN o ON TRUE`,
		deliveryID, webhookID, eventID, status, responseCode, responseBody, latencyMs,
		webhookNow(), webhookMaxAttempts, pq.Array(backoffSeconds()), webhookJitter(), lastErr).Scan(&attempts, &state)
	if err != nil {
		logger.Error("record webhook delivery", zap.Error(err), zap.String("delivery_id", deliveryID),
			zap.String("webhook_id", webhookID), zap.String("event_id", eventID))
		return
	}
	if !state.Valid {
		return
	}
	switch state.String {
	case outboxDelivered:
		if attempts.Int64 >= 2 {
			webhookRetries.WithLabelValues("delivered").Inc()
		}
	case outboxPending:
		webhookRetries.WithLabelValues("retry").Inc()
	case outboxFailed:
		webhookRetries.WithLabelValues("exhausted").Inc()
		logger.Warn("webhook delivery gave up after its last attempt",
			zap.String("webhook_id", webhookID), zap.String("event_id", eventID),
			zap.Int64("attempts", attempts.Int64), zap.Int("last_status", responseCode))
	}
}

// skipOutboxRows makes the pending outbox row of (endpoint, event) terminal
// `skipped`: the endpoint was disabled while its attempt waited. (A deleted
// endpoint's rows are skipped by deleteWebhookEndpoint.)
func skipOutboxRows(ctx context.Context, db *sql.DB, webhookID, eventID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, `
		UPDATE webhook_outbox SET state = 'skipped', next_attempt_at = NULL, last_error = $3, updated_at = $4
		WHERE endpoint_id = $1 AND event_id = $2 AND state = 'pending'`,
		webhookID, eventID, endpointSkipped, webhookNow())
	if err != nil {
		return fmt.Errorf("skip outbox rows of webhook %s: %w", webhookID, err)
	}
	return nil
}

// WebhookRetrier is the `webhook_retry` job: due outbox rows back to the
// delivery pool.
type WebhookRetrier struct {
	db     *sql.DB
	logger *zap.Logger
	// pool is the delivery pool (eventDeliveries; tests give their own).
	pool *deliveryPool
	// BatchSize bounds the rows one run takes.
	BatchSize int
	// JobName is the job_runs name (tests use their own: the table is shared).
	JobName string
	// onlyTenant scopes a run to one tenant (tests: the table is shared).
	onlyTenant string
}

// WebhookRetryResult is one run's outcome (job_runs.result).
type WebhookRetryResult struct {
	Due       int `json:"due"`
	Submitted int `json:"submitted"`
	Skipped   int `json:"skipped"`
}

// NewWebhookRetrier builds the job; nil without a DB.
func NewWebhookRetrier(db *sql.DB, logger *zap.Logger) *WebhookRetrier {
	if db == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &WebhookRetrier{db: db, logger: logger, pool: eventDeliveries, BatchSize: webhookRetryBatch, JobName: webhookRetryJobName}
}

func (w *WebhookRetrier) spec() jobSpec {
	return jobSpec{Name: w.JobName, Every: webhookRetryEvery, BootDelay: 25 * time.Second, MaxRunTime: 25 * time.Second,
		Run: func(ctx context.Context) (jobReport, error) {
			res, err := w.RunOnce(ctx)
			return jobReport{Rows: int64(res.Submitted), Result: res}, err
		}}
}

// dueOutboxRow is a due row with what its attempt needs.
type dueOutboxRow struct {
	id, tenantID, eventID string
	ep                    webhookEndpoint
	epOK, enabled         bool
	eventType             string
	data                  []byte
	eventOK               bool
	createdAt             time.Time // the events row's: the one date in every attempt's body (RP18-2)
}

// RunOnce takes up to BatchSize due rows (SKIP LOCKED: two slots never take
// the same row), leases them in the same transaction, makes the ones whose
// endpoint or event is gone `skipped`, and submits the rest to the pool —
// one job per row, attempt 1's job shape.
func (w *WebhookRetrier) RunOnce(ctx context.Context) (WebhookRetryResult, error) {
	var res WebhookRetryResult
	now := webhookNow()
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("webhook retry: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT o.id, o.tenant_id, o.event_id,
		       COALESCE(w.id, ''), COALESCE(w.url, ''), COALESCE(w.event_filter, '{}'), COALESCE(w.secret, ''), COALESCE(w.enabled, FALSE),
		       COALESCE(e.type, ''), COALESCE(e.data, '{}'::jsonb), e.id IS NOT NULL, COALESCE(e.created_at, o.created_at)
		FROM webhook_outbox o
		LEFT JOIN webhook_endpoints w ON w.id = o.endpoint_id
		LEFT JOIN events e ON e.id = o.event_id
		WHERE o.state = 'pending' AND o.next_attempt_at <= $1 AND ($3 = '' OR o.tenant_id = $3)
		ORDER BY o.next_attempt_at
		LIMIT $2
		FOR UPDATE OF o SKIP LOCKED`, now, w.BatchSize, w.onlyTenant)
	if err != nil {
		return res, fmt.Errorf("webhook retry: due rows: %w", err)
	}
	var due []dueOutboxRow
	for rows.Next() {
		var r dueOutboxRow
		var filter pq.StringArray
		if err := rows.Scan(&r.id, &r.tenantID, &r.eventID, &r.ep.id, &r.ep.url, &filter, &r.ep.secret, &r.enabled,
			&r.eventType, &r.data, &r.eventOK, &r.createdAt); err != nil {
			_ = rows.Close()
			return res, fmt.Errorf("webhook retry: scan: %w", err)
		}
		r.ep.filter = []string(filter)
		r.epOK = r.ep.id != ""
		due = append(due, r)
	}
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("webhook retry: rows: %w", err)
	}
	_ = rows.Close()
	res.Due = len(due)
	if len(due) == 0 {
		return res, nil
	}
	var leased, skipped []string
	for _, r := range due {
		if r.epOK && r.enabled && r.eventOK && matchesWebhookFilter(r.ep.filter, r.eventType) {
			leased = append(leased, r.id)
		} else {
			skipped = append(skipped, r.id)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE webhook_outbox SET next_attempt_at = $2, updated_at = $3 WHERE id = ANY($1)`,
		pq.Array(leased), now.Add(webhookAttemptLease), now); err != nil {
		return res, fmt.Errorf("webhook retry: lease: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE webhook_outbox SET state = 'skipped', next_attempt_at = NULL, last_error = $2, updated_at = $3 WHERE id = ANY($1)`,
		pq.Array(skipped), endpointSkipped, now); err != nil {
		return res, fmt.Errorf("webhook retry: skip: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("webhook retry: commit: %w", err)
	}
	res.Skipped = len(skipped)
	jobs := make([]deliveryJob, 0, len(leased))
	for _, r := range due {
		if !(r.epOK && r.enabled && r.eventOK && matchesWebhookFilter(r.ep.filter, r.eventType)) {
			continue
		}
		jobs = append(jobs, webhookJob(w.db, w.logger, []webhookEndpoint{r.ep}, webhookGeneration(r.tenantID),
			recordedEvent{id: r.eventID, eventType: r.eventType, tenantID: r.tenantID, data: r.data, createdAt: r.createdAt}))
	}
	res.Submitted = len(jobs)
	w.pool.submit(jobs...)
	return res, nil
}
