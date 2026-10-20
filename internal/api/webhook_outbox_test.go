package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// WP-R11-3 (R11-11): a webhook delivery was ONE attempt — retry_count was
// always 0, next_retry_at never written, the signature covered the body
// only, and a secret could not be rotated. These tests drive the outbox,
// the retry job, the signature and the rotate route against the migrated
// test database, with the outbox's clock and jitter injected: no sleeps
// stand for a backoff.

// webhookClock replaces the outbox's clock for one test; jitter is 1.0.
type webhookClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *webhookClock) get() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *webhookClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func useWebhookClock(t *testing.T) *webhookClock {
	t.Helper()
	c := &webhookClock{now: time.Now().UTC().Truncate(time.Second)}
	prevNow, prevJitter := webhookNow, webhookJitter
	webhookNow, webhookJitter = c.get, func() float64 { return 1 }
	t.Cleanup(func() { webhookNow, webhookJitter = prevNow, prevJitter })
	return c
}

// scriptedHook answers the statuses in order (the last one for ever) and
// keeps every request's signature header, body and event id.
type scriptedHook struct {
	srv      *httptest.Server
	mu       sync.Mutex
	statuses []int
	posts    atomic.Int32
	sigs     []string
	bodies   [][]byte
}

func newScriptedHook(t *testing.T, statuses ...int) *scriptedHook {
	t.Helper()
	h := &scriptedHook{statuses: statuses}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := int(h.posts.Add(1))
		h.mu.Lock()
		h.sigs = append(h.sigs, r.Header.Get("X-Webhook-Signature"))
		h.bodies = append(h.bodies, body)
		status := h.statuses[min(n, len(h.statuses))-1]
		h.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *scriptedHook) last() (sig string, body []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sigs[len(h.sigs)-1], h.bodies[len(h.bodies)-1]
}

// outboxFixture is a tenant with one webhook, a private delivery pool and
// the retry job scoped to the tenant.
type outboxFixture struct {
	*batchFanoutFixture
	hook    string
	pool    *deliveryPool
	retrier *WebhookRetrier
	clock   *webhookClock
}

func setupOutboxFixture(t *testing.T, url string) *outboxFixture {
	t.Helper()
	f := &outboxFixture{batchFanoutFixture: setupBatchFanoutFixture(t)}
	webhookAllowPrivateTargets.Store(true)
	t.Cleanup(func() { webhookAllowPrivateTargets.Store(false) })
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM webhook_outbox WHERE tenant_id = $1`, f.tenantID)
		_, _ = f.db.Exec(`DELETE FROM audit_logs WHERE tenant_id = $1`, f.tenantID)
	})
	f.clock = useWebhookClock(t)
	f.pool = newDeliveryPool(4, 64)
	useDeliveryPool(t, f.pool)
	f.hook = f.webhook(url, "object.deleted")
	forgetWebhookEndpoints(f.tenantID)
	f.retrier = NewWebhookRetrier(f.db, zap.NewNop())
	f.retrier.pool = f.pool
	f.retrier.onlyTenant = f.tenantID
	return f
}

type outboxRow struct {
	state    string
	attempts int
	next     *time.Time
	done     *time.Time
	status   *int
}

func (f *outboxFixture) row(t *testing.T) outboxRow {
	t.Helper()
	var r outboxRow
	require.NoError(t, f.db.QueryRow(`SELECT state, attempts, next_attempt_at, delivered_at, last_status
		FROM webhook_outbox WHERE tenant_id = $1 AND endpoint_id = $2`, f.tenantID, f.hook).
		Scan(&r.state, &r.attempts, &r.next, &r.done, &r.status))
	return r
}

// settle waits until the pool has nothing queued or running.
func (f *outboxFixture) settle(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return f.pool.pending() == 0 }, 10*time.Second, 5*time.Millisecond)
}

func (f *outboxFixture) emit(t *testing.T) {
	t.Helper()
	emitEvent(context.Background(), f.db, zap.NewNop(), "object.deleted", f.tenantID, map[string]interface{}{"key": "k"})
	f.settle(t)
}

// retry runs the job once and waits for what it submitted.
func (f *outboxFixture) retry(t *testing.T) WebhookRetryResult {
	t.Helper()
	res, err := f.retrier.RunOnce(context.Background())
	require.NoError(t, err)
	f.settle(t)
	return res
}

func retriesOf(outcome string) float64 {
	return testutil.ToFloat64(webhookRetries.WithLabelValues(outcome))
}

func TestWebhookOutbox_AReceiverThat500sTwiceThen200sGetsTheEvent(t *testing.T) {
	// Arrange
	hook := newScriptedHook(t, 500, 500, 200)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	delivered0, retry0 := retriesOf("delivered"), retriesOf("retry")

	// Act: attempt 1 is the pool job emitEvent submits.
	f.emit(t)
	r := f.row(t)
	require.Equal(t, outboxPending, r.state)
	require.Equal(t, 1, r.attempts)
	require.NotNil(t, r.next)
	assert.Equal(t, f.clock.get().Add(webhookBackoff[0]), r.next.UTC(), "due 1 s after the first failure")

	// The backoff is honoured: not due before it, due after it.
	f.clock.advance(500 * time.Millisecond)
	assert.Equal(t, 0, f.retry(t).Due)
	f.clock.advance(600 * time.Millisecond)
	assert.Equal(t, 1, f.retry(t).Submitted)
	r = f.row(t)
	require.Equal(t, 2, r.attempts)
	assert.Equal(t, f.clock.get().Add(webhookBackoff[1]), r.next.UTC(), "then 5 s")
	f.clock.advance(webhookBackoff[1])
	assert.Equal(t, 1, f.retry(t).Submitted)

	// Assert
	r = f.row(t)
	assert.Equal(t, outboxDelivered, r.state)
	assert.Equal(t, 3, r.attempts)
	require.NotNil(t, r.done, "delivered_at set")
	assert.Nil(t, r.next)
	require.NotNil(t, r.status)
	assert.Equal(t, 200, *r.status)
	assert.Equal(t, int32(3), hook.posts.Load())
	assert.Equal(t, 0, f.retry(t).Due, "nothing left to retry")
	assert.Equal(t, delivered0+1, retriesOf("delivered"))
	assert.Equal(t, retry0+2, retriesOf("retry"))

	// The deliveries log: one row per attempt, retry_count 0, 1, 2; the
	// failed ones name when the next attempt was due.
	rows, err := f.db.Query(`SELECT status, retry_count, next_retry_at IS NOT NULL FROM webhook_deliveries
		WHERE webhook_id = $1 ORDER BY retry_count`, f.hook)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var status string
		var rc int
		var hasNext bool
		require.NoError(t, rows.Scan(&status, &rc, &hasNext))
		got = append(got, status+"/"+strconv.Itoa(rc)+"/"+strconv.FormatBool(hasNext))
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"failed/0/true", "failed/1/true", "delivered/2/false"}, got)
}

func TestWebhookOutbox_AReceiverDownForGoodGoesTerminalAtTheFifthAttempt(t *testing.T) {
	// Arrange
	hook := newScriptedHook(t, 503)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	exhausted0 := retriesOf("exhausted")

	// Act: attempt 1, then each retry exactly when its backoff ends.
	f.emit(t)
	for i, wait := range webhookBackoff {
		r := f.row(t)
		require.Equal(t, i+1, r.attempts)
		require.NotNil(t, r.next, "attempt %d is followed by another", i+1)
		assert.Equal(t, f.clock.get().Add(wait), r.next.UTC(), "the wait after attempt %d", i+1)
		f.clock.advance(wait)
		require.Equal(t, 1, f.retry(t).Submitted)
	}

	// Assert
	r := f.row(t)
	assert.Equal(t, outboxFailed, r.state)
	assert.Equal(t, webhookMaxAttempts, r.attempts)
	assert.Nil(t, r.next)
	assert.Nil(t, r.done)
	assert.Equal(t, int32(webhookMaxAttempts), hook.posts.Load())
	f.clock.advance(time.Hour)
	assert.Equal(t, 0, f.retry(t).Due, "a terminal row is never retried")
	assert.Equal(t, exhausted0+1, retriesOf("exhausted"))
	assert.Len(t, webhookBackoff, webhookMaxAttempts-1)
}

func TestWebhookOutbox_APoolDropLeavesTheRowDueWithoutSpendingAnAttempt(t *testing.T) {
	// Arrange: the pool is stopping — attempt 1 is dropped at submit.
	hook := newScriptedHook(t, 204)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	f.pool.drain(0)

	// Act
	f.emit(t)

	// Assert: a failed `dropped: shutdown` row as before, and the outbox row
	// due 30 s later with no attempt spent.
	assert.Equal(t, 1, webhookRowsFor(t, f.db, f.hook, "failed", deliveryDroppedShutdown))
	r := f.row(t)
	assert.Equal(t, outboxPending, r.state)
	assert.Equal(t, 0, r.attempts)
	require.NotNil(t, r.next)
	assert.Equal(t, f.clock.get().Add(webhookDroppedRetryAfter), r.next.UTC())

	// A retry the pool drops too stays due.
	f.clock.advance(webhookDroppedRetryAfter)
	assert.Equal(t, 1, f.retry(t).Submitted)
	r = f.row(t)
	assert.Equal(t, outboxPending, r.state)
	assert.Equal(t, 0, r.attempts)
	assert.Equal(t, f.clock.get().Add(webhookDroppedRetryAfter), r.next.UTC())

	// With a pool that runs it, the event is delivered.
	live := newDeliveryPool(4, 64)
	useDeliveryPool(t, live)
	f.pool, f.retrier.pool = live, live
	f.clock.advance(webhookDroppedRetryAfter)
	assert.Equal(t, 1, f.retry(t).Submitted)
	r = f.row(t)
	assert.Equal(t, outboxDelivered, r.state)
	assert.Equal(t, 1, r.attempts)
	assert.Equal(t, int32(1), hook.posts.Load())
}

func TestWebhookOutbox_AnAttemptThatNeverFinishedIsRetriedWhenItsLeaseEnds(t *testing.T) {
	// Arrange: the intent row is written (as emitEvent writes it) but no
	// attempt ever finishes — the process died with the job queued.
	hook := newScriptedHook(t, 204)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	ev := f.event()
	recordWebhookIntents(context.Background(), f.db, zap.NewNop(), f.tenantID, []droppedDelivery{{webhookID: f.hook, eventID: ev}})

	// Act + Assert: not due while the lease runs, delivered once it ends.
	f.clock.advance(webhookAttemptLease - time.Second)
	assert.Equal(t, 0, f.retry(t).Due)
	f.clock.advance(time.Second)
	assert.Equal(t, 1, f.retry(t).Submitted)
	assert.Equal(t, outboxDelivered, f.row(t).state)
	assert.Equal(t, int32(1), hook.posts.Load())
}

func TestWebhookOutbox_ADisabledOrDeletedEndpointsRowsAreSkipped(t *testing.T) {
	// Arrange: a failed first attempt leaves a row due.
	hook := newScriptedHook(t, 500)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	f.emit(t)
	require.Equal(t, outboxPending, f.row(t).state)

	// Act: the endpoint is disabled, the retry comes due.
	_, err := f.db.Exec(`UPDATE webhook_endpoints SET enabled = FALSE WHERE id = $1`, f.hook)
	require.NoError(t, err)
	f.clock.advance(time.Minute)
	res := f.retry(t)

	// Assert
	assert.Equal(t, 1, res.Skipped)
	assert.Equal(t, 0, res.Submitted)
	assert.Equal(t, outboxSkipped, f.row(t).state)
	assert.Equal(t, int32(1), hook.posts.Load())

	// A second endpoint's pending row is skipped by the endpoint's delete
	// itself, in its transaction (the FK then nulls endpoint_id).
	g := setupOutboxFixture(t, hook.srv.URL+"/hook")
	g.emit(t)
	n, err := (&Server{db: g.db, logger: zap.NewNop()}).deleteWebhookEndpoint(context.Background(), g.hook, g.tenantID)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	var state string
	var endpoint *string
	require.NoError(t, g.db.QueryRow(`SELECT state, endpoint_id FROM webhook_outbox WHERE tenant_id = $1`, g.tenantID).Scan(&state, &endpoint))
	assert.Equal(t, outboxSkipped, state)
	assert.Nil(t, endpoint)
	g.clock.advance(time.Hour)
	assert.Equal(t, 0, g.retry(t).Due)
}

func TestWebhookOutbox_AnEndpointDisabledWhileItsFirstAttemptIsQueuedEndsSkipped(t *testing.T) {
	// Arrange: attempt 1 waits behind a held worker.
	hook := newScriptedHook(t, 204)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	held, release := heldPool(t, f.tenantID)
	f.pool, f.retrier.pool = held, held
	emitEvent(context.Background(), f.db, zap.NewNop(), "object.deleted", f.tenantID, map[string]interface{}{"key": "k"})

	// Act: disabled meanwhile (the route moves the generation), then it runs.
	_, err := f.db.Exec(`UPDATE webhook_endpoints SET enabled = FALSE WHERE id = $1`, f.hook)
	require.NoError(t, err)
	forgetWebhookEndpoints(f.tenantID)
	close(release)
	f.settle(t)

	// Assert: no POST, and no retry owed (the lease would have re-sent it).
	assert.Equal(t, int32(0), hook.posts.Load())
	assert.Equal(t, outboxSkipped, f.row(t).state)
	f.clock.advance(webhookAttemptLease + time.Second)
	assert.Equal(t, 0, f.retry(t).Due)
}

func TestWebhookOutbox_RetentionPrunesTerminalRowsAfter30DaysOnly(t *testing.T) {
	// Arrange: a terminal row 40 days old, a terminal row 10 days old and
	// a (stuck) pending row 40 days old.
	f := setupOutboxFixture(t, "https://receiver.example/hook")
	old, fresh := time.Now().Add(-40*24*time.Hour), time.Now().Add(-10*24*time.Hour)
	for _, r := range []struct {
		state string
		at    time.Time
	}{{outboxFailed, old}, {outboxDelivered, fresh}, {outboxPending, old}} {
		_, err := f.db.Exec(`INSERT INTO webhook_outbox (id, tenant_id, endpoint_id, event_id, state, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			uuid.NewString(), f.tenantID, f.hook, f.event(), r.state, r.at)
		require.NoError(t, err)
	}
	job := NewRetentionJob(f.db, zap.NewNop())
	var policy retentionPolicy
	for _, p := range job.Policies {
		if p.Table == "webhook_outbox" {
			policy = p
		}
	}
	require.Equal(t, "webhook_outbox", policy.Table)
	policy.Extra = "tenant_id = '" + f.tenantID + "' AND " + policy.Extra
	job.Policies = []retentionPolicy{policy}

	// Act
	res, err := job.RunOnce(context.Background())

	// Assert
	require.NoError(t, err)
	assert.Equal(t, int64(1), res.Tables["webhook_outbox"])
	var states []string
	rows, err := f.db.Query(`SELECT state FROM webhook_outbox WHERE tenant_id = $1 ORDER BY state`, f.tenantID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		states = append(states, s)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{outboxDelivered, outboxPending}, states)
}

// verifyWebhookSignature is what a receiver does (docs/API.md): parse
// t and v1, recompute HMAC-SHA256(secret, t + "." + body), refuse a t more
// than tolerance from its clock.
func verifyWebhookSignature(header string, body []byte, secret string, now time.Time, tolerance time.Duration) error {
	var ts, v1 string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || v1 == "" {
		return errors.New("malformed signature header")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return errors.New("timestamp outside the tolerance")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(v1)) {
		return errors.New("signature mismatch")
	}
	return nil
}

func TestWebhookSignature_IsTimestampedAndKeepsTheLegacyElement(t *testing.T) {
	// Arrange
	body := []byte(`{"id":"ev","type":"object.created"}`)
	at := time.Unix(1_800_000_000, 0)

	// Act
	sig := webhookSignature(body, "whsec_x", at)

	// Assert
	assert.True(t, strings.HasPrefix(sig, "t=1800000000,v1="), sig)
	assert.True(t, strings.HasSuffix(sig, ","+generateWebhookSignature(body, "whsec_x")), "the legacy body-only HMAC rides along")
	assert.NoError(t, verifyWebhookSignature(sig, body, "whsec_x", at.Add(4*time.Minute), 5*time.Minute))
	assert.Error(t, verifyWebhookSignature(sig, body, "whsec_x", at.Add(6*time.Minute), 5*time.Minute), "a 6-minute-old t is a replay")
	assert.Error(t, verifyWebhookSignature(sig, append(body, ' '), "whsec_x", at, 5*time.Minute), "the body is covered")
	assert.Error(t, verifyWebhookSignature(sig, body, "whsec_other", at, 5*time.Minute))
	assert.Error(t, verifyWebhookSignature(strings.Replace(sig, "t=1800000000", "t=1800000300", 1), body, "whsec_x", at.Add(5*time.Minute), 5*time.Minute),
		"t is covered: moving it forward breaks v1")
}

// webhookAPI is the webhook routes on the fixture's database, authenticated
// as the fixture's tenant.
func (f *outboxFixture) webhookAPI() *chi.Mux {
	s := &Server{db: f.db, logger: zap.NewNop()}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Request-Id", uuid.NewString())
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), tenantIDKey, f.tenantID)))
		})
	})
	r.Post("/api/v1/webhooks/{id}/rotate-secret", s.handleRotateWebhookSecret)
	r.Get("/api/v1/webhooks/{id}/deliveries", s.handleListDeliveries)
	return r
}

func TestRotateWebhookSecret_TheOldSecretStopsAndTheNewOneSigns(t *testing.T) {
	// Arrange
	hook := newScriptedHook(t, 204)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	api := f.webhookAPI()
	f.emit(t)
	sig, body := hook.last()
	require.NoError(t, verifyWebhookSignature(sig, body, "whsec_test", f.clock.get(), 5*time.Minute))

	// Act
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+f.hook+"/rotate-secret", nil))

	// Assert: the new secret, once, and it is the one that signs now.
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	secret, _ := resp["secret"].(string)
	require.True(t, strings.HasPrefix(secret, "whsec_"), secret)
	assert.NotEqual(t, "whsec_test", secret)
	assert.Equal(t, f.hook, resp["id"])
	f.emit(t)
	sig, body = hook.last()
	assert.NoError(t, verifyWebhookSignature(sig, body, secret, f.clock.get(), 5*time.Minute))
	assert.Error(t, verifyWebhookSignature(sig, body, "whsec_test", f.clock.get(), 5*time.Minute), "the old secret verifies nothing")
	var audits int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'webhook.secret_rotated'`,
		f.tenantID).Scan(&audits))
	assert.Equal(t, 1, audits)

	// Another tenant's webhook (and a missing one) is 404 and unchanged.
	other := setupBatchFanoutFixture(t)
	foreign := other.webhook("https://x.example/hook", "*")
	for _, id := range []string{foreign, "no-such-webhook"} {
		rec = httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+id+"/rotate-secret", nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, id)
	}
	var foreignSecret string
	require.NoError(t, f.db.QueryRow(`SELECT secret FROM webhook_endpoints WHERE id = $1`, foreign).Scan(&foreignSecret))
	assert.Equal(t, "whsec_test", foreignSecret)
}

func TestRotateWebhookSecret_AQueuedJobPostsWithTheNewSecret(t *testing.T) {
	// Arrange: the first attempt waits in the queue behind a held worker.
	hook := newScriptedHook(t, 204)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	held, release := heldPool(t, f.tenantID)
	f.pool, f.retrier.pool = held, held
	emitEvent(context.Background(), f.db, zap.NewNop(), "object.deleted", f.tenantID, map[string]interface{}{"key": "k"})
	require.Equal(t, 1, held.queuedLen())

	// Act: rotate while it waits, then let it run.
	rec := httptest.NewRecorder()
	f.webhookAPI().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+f.hook+"/rotate-secret", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	close(release)
	f.settle(t)

	// Assert
	sig, body := hook.last()
	assert.NoError(t, verifyWebhookSignature(sig, body, resp["secret"].(string), f.clock.get(), 5*time.Minute))
}

func TestListDeliveries_RetryCountIsTheAttemptsFromTheOutbox(t *testing.T) {
	// Arrange: two failures, then a 2xx.
	hook := newScriptedHook(t, 500, 502, 204)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")
	f.emit(t)
	for _, wait := range webhookBackoff[:2] {
		f.clock.advance(wait)
		f.retry(t)
	}

	// Act
	rec := httptest.NewRecorder()
	f.webhookAPI().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/"+f.hook+"/deliveries", nil))

	// Assert: newest first — the delivered third attempt is retry 2.
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Data []struct {
			Status      string  `json:"status"`
			RetryCount  int     `json:"retry_count"`
			NextRetryAt *string `json:"next_retry_at"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 3)
	assert.Equal(t, "delivered", resp.Data[0].Status)
	assert.Equal(t, 2, resp.Data[0].RetryCount)
	assert.Nil(t, resp.Data[0].NextRetryAt)
	assert.Equal(t, 1, resp.Data[1].RetryCount)
	assert.NotNil(t, resp.Data[1].NextRetryAt)
	assert.Equal(t, 0, resp.Data[2].RetryCount)
}

func TestWebhookRetries_EveryOutcomeExistsAtZeroFromBoot(t *testing.T) {
	// The vector is built with its three series (RP14-9: a first retry is a
	// rise from 0 that increase() sees); /metrics registers it.
	assert.Equal(t, 3, testutil.CollectAndCount(webhookRetries))
}

// RP18-2 (Stage 1a Prompt 17 Part 0): one event is one body. The envelope's
// created_at is the events row's, so attempt 1 and every retry of one
// X-Event-ID send byte-equal bodies (the signature's t is what dates the
// request). deliverToEndpoints stamped time.Now() into it.
func TestWebhookOutbox_ARetrySendsTheSameBodyAsTheFirstAttempt(t *testing.T) {
	// Arrange
	hook := newScriptedHook(t, 500, 200)
	f := setupOutboxFixture(t, hook.srv.URL+"/hook")

	// Act: attempt 1, then a retry after the wall clock has crossed a
	// second boundary (RFC 3339 is second-precise: a body stamped "now"
	// twice inside one second would pass by luck).
	f.emit(t)
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(1100 * time.Millisecond)))
	f.clock.advance(webhookBackoff[0] + time.Second)
	require.Equal(t, 1, f.retry(t).Submitted)

	// Assert
	hook.mu.Lock()
	defer hook.mu.Unlock()
	require.Len(t, hook.bodies, 2)
	assert.Equal(t, string(hook.bodies[0]), string(hook.bodies[1]), "attempt 1 and the retry: one body")
	var env struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
	}
	require.NoError(t, json.Unmarshal(hook.bodies[0], &env))
	var rowCreated time.Time
	require.NoError(t, f.db.QueryRow(`SELECT created_at FROM events WHERE id = $1`, env.ID).Scan(&rowCreated))
	assert.Equal(t, rowCreated.UTC().Format(time.RFC3339), env.CreatedAt, "the events row's created_at")
}
