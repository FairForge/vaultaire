package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/webhooksig"
)

var validEventTypes = []string{
	"object.created",
	"object.deleted",
	"object.downloaded",
	"bucket.created",
	"bucket.deleted",
	"key.created",
	"key.revoked",
	"sts.token_created",
	"webhook.test",
	"bandwidth.alert",
	"tenant.created", // WP-PLAT-1: a platform made a child tenant
}

func isValidEventType(t string) bool {
	for _, v := range validEventTypes {
		if t == v {
			return true
		}
	}
	return false
}

func matchesWebhookFilter(filter []string, eventType string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		if f == "*" || f == eventType {
			return true
		}
		if strings.HasSuffix(f, ".*") {
			prefix := strings.TrimSuffix(f, ".*")
			if strings.HasPrefix(eventType, prefix+".") {
				return true
			}
		}
	}
	return false
}

// emitEvent records the event and hands its delivery to the tenant's
// webhooks to the process-wide delivery pool (event_delivery.go) — only
// when the tenant has a webhook for it: the list comes from the targets
// cache (event_targets.go), so a tenant without one submits nothing and,
// once cached, queries nothing here.
func emitEvent(ctx context.Context, db *sql.DB, logger *zap.Logger, eventType, tenantID string, data map[string]interface{}) {
	if db == nil {
		return
	}
	ev, ok := recordEvent(ctx, db, logger, eventType, tenantID, data)
	if !ok {
		return
	}
	submitEventDelivery(ctx, db, logger, ev)
}

// eventAfterResponseTimeout bounds an event recorded after the response body
// was streamed (emitEventAfterResponse).
const eventAfterResponseTimeout = 5 * time.Second

// emitEventAfterResponse is emitEvent for an event recorded once the response
// body is out: the client may have gone on its last byte (aws-cli closes the
// connection at once), which cancels the request context — the insert then
// failed with "context canceled" and the event and its webhooks were lost.
// It runs on the request's values without its cancellation, bounded by
// eventAfterResponseTimeout.
func emitEventAfterResponse(ctx context.Context, db *sql.DB, logger *zap.Logger, eventType, tenantID string, data map[string]interface{}) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventAfterResponseTimeout)
	defer cancel()
	emitEvent(ctx, db, logger, eventType, tenantID, data)
}

// submitEventDelivery queues one recorded event for the tenant's matching
// webhooks; nothing when there are none.
func submitEventDelivery(ctx context.Context, db *sql.DB, logger *zap.Logger, ev recordedEvent) {
	eventID, eventType, tenantID := ev.id, ev.eventType, ev.tenantID
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	endpoints, gen, err := tenantWebhookEndpoints(lctx, db, logger, tenantID)
	if err != nil {
		// Skipped: counted, and every webhook known from the last good read
		// that the event matches gets its failed row — through the pool, off
		// the request (Prompt 2b 0.6: skipped events were invisible).
		deliveriesDropped.WithLabelValues(deliveryKindWebhook, "lookup_failed").Inc()
		logger.Error("query webhook endpoints for dispatch", zap.Error(err), zap.String("tenant_id", tenantID))
		if owed := owedRows(endpoints, eventID, eventType); len(owed) > 0 {
			eventDeliveries.submit(droppedRowsJob(db, logger, tenantID, owed, deliveryDroppedLookupFailed))
		}
		return
	}
	owed := owedRows(endpoints, eventID, eventType)
	if len(owed) == 0 {
		return
	}
	// The outbox rows first: the durable intent the retry job works from
	// (WP-R11-3); the pool job is attempt 1.
	recordWebhookIntents(lctx, db, logger, tenantID, owed)
	eventDeliveries.submit(webhookJob(db, logger, endpoints, gen, ev))
}

// recordedEvent is one events row as its deliveries carry it: the id, the
// type, the tenant, the data and the row's created_at — the one date every
// attempt of the event sends (RP18-2: a retry stamped time.Now() into the
// envelope, so each attempt of one X-Event-ID was a different body).
type recordedEvent struct {
	id, eventType, tenantID string
	data                    []byte
	createdAt               time.Time
	// test: an event of a sandbox (WP-DX-1), recorded under its parent —
	// "livemode": false in the body. Zero = live.
	test bool
}

// recordEvent inserts the events row; false when nothing was recorded.
func recordEvent(ctx context.Context, db *sql.DB, logger *zap.Logger, eventType, tenantID string, data map[string]interface{}) (recordedEvent, bool) {
	ev := recordedEvent{id: uuid.New().String(), eventType: eventType, tenantID: tenantID}
	// WP-DX-1: a sandbox's event is its parent's, with livemode false — the
	// parent's webhooks receive it and its event log lists it.
	if parent := sandboxParent(ctx, db, tenantID); parent != "" {
		ev.tenantID, ev.test = parent, true
		tenantID = parent
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		logger.Error("marshal event data", zap.Error(err))
		return recordedEvent{}, false
	}
	ev.data = dataJSON

	err = db.QueryRowContext(ctx, `
		INSERT INTO events (id, type, tenant_id, data, livemode)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at`,
		ev.id, eventType, tenantID, dataJSON, !ev.test).Scan(&ev.createdAt)
	if err != nil {
		logger.Error("insert event",
			zap.Error(err),
			zap.String("type", eventType),
			zap.String("tenant_id", tenantID))
		return recordedEvent{}, false
	}
	return ev, true
}

// webhookEndpoint is one enabled webhook_endpoints row.
type webhookEndpoint struct {
	id     string
	url    string
	filter []string
	secret string
}

// loadWebhookEndpoints reads the tenant's enabled webhook endpoints.
func loadWebhookEndpoints(ctx context.Context, db *sql.DB, logger *zap.Logger, tenantID string) ([]webhookEndpoint, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, url, event_filter, secret
		FROM webhook_endpoints
		WHERE tenant_id = $1 AND enabled = TRUE`,
		tenantID)
	if err != nil {
		return nil, fmt.Errorf("query webhook endpoints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var endpoints []webhookEndpoint
	for rows.Next() {
		var ep webhookEndpoint
		if err := rows.Scan(&ep.id, &ep.url, pq.Array(&ep.filter), &ep.secret); err != nil {
			logger.Error("scan webhook endpoint", zap.Error(err))
			continue
		}
		endpoints = append(endpoints, ep)
	}
	if err := rows.Err(); err != nil {
		logger.Warn("iterate rows", zap.Error(err))
	}
	return endpoints, nil
}

// deliverToEndpoints posts the event to every endpoint whose filter
// matches it, recording each delivery. The body is a function of the
// events row alone (its created_at, never the attempt's clock), so every
// attempt of one event sends the same bytes (RP18-2).
func deliverToEndpoints(ctx context.Context, db *sql.DB, logger *zap.Logger, endpoints []webhookEndpoint, ev recordedEvent) {
	if len(endpoints) == 0 {
		return
	}
	eventID, eventType := ev.id, ev.eventType
	eventPayload := map[string]interface{}{
		"id":         eventID,
		"type":       eventType,
		"tenant_id":  ev.tenantID,
		"data":       json.RawMessage(ev.data),
		"created_at": ev.createdAt.UTC().Format(time.RFC3339),
		"livemode":   !ev.test, // WP-DX-1: false for a sandbox's event
	}
	body, err := json.Marshal(eventPayload)
	if err != nil {
		logger.Error("marshal webhook payload", zap.Error(err))
		return
	}

	// The guarded client from s3_notifications.go: no redirects, no proxy,
	// resolved addresses re-checked against the private ranges at dial
	// time (Review R11-04 — the default client followed redirects and
	// dialled anything the stored URL named).
	client := sharedWebhookClient()
	for _, ep := range endpoints {
		if !matchesWebhookFilter(ep.filter, eventType) {
			continue
		}
		deliverWebhook(ctx, db, logger, client, ep.id, ep.url, ep.secret, eventID, body)
	}
}

func deliverWebhook(ctx context.Context, db *sql.DB, logger *zap.Logger, client *http.Client, webhookID, url, secret, eventID string, body []byte) {
	deliveryID := uuid.New().String()
	start := time.Now()

	sig := webhookSignature(body, secret, webhookNow())

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		logger.Error("create webhook request", zap.Error(err), zap.String("url", url))
		finishWebhookAttempt(ctx, db, logger, deliveryID, webhookID, eventID, "failed", 0, err.Error(), 0)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Vaultaire-Webhooks/1.0")
	req.Header.Set("X-Webhook-Signature", sig)
	req.Header.Set("X-Event-ID", eventID)

	resp, err := client.Do(req)
	latencyMs := int(time.Since(start).Milliseconds())
	if err != nil {
		logger.Warn("webhook delivery failed",
			zap.Error(err),
			zap.String("webhook_id", webhookID),
			zap.String("url", url))
		finishWebhookAttempt(ctx, db, logger, deliveryID, webhookID, eventID, "failed", 0, err.Error(), latencyMs)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	var respBody strings.Builder
	respBody.Grow(1024)
	buf := make([]byte, 1024)
	n, _ := resp.Body.Read(buf)
	respBody.Write(buf[:n])

	// Only a 2xx delivers (a 3xx is not followed — the client refuses
	// redirects — and is not a delivery either).
	status := "delivered"
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status = "failed"
	}

	finishWebhookAttempt(ctx, db, logger, deliveryID, webhookID, eventID, status, resp.StatusCode, respBody.String(), latencyMs)
}

func recordDelivery(ctx context.Context, db *sql.DB, logger *zap.Logger, deliveryID, webhookID, eventID, status string, responseCode int, responseBody string, latencyMs int) {
	// Detached: the delivery's own deadline (or the shutdown cut) is what
	// usually ended it, and the row must still be written.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, `
		INSERT INTO webhook_deliveries (id, webhook_id, event_id, status, response_code, response_body, latency_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		deliveryID, webhookID, eventID, status, responseCode, responseBody, latencyMs)
	if err != nil {
		logger.Error("record webhook delivery",
			zap.Error(err),
			zap.String("delivery_id", deliveryID))
	}
}

// webhookSignature is the X-Webhook-Signature value (WP-R11-3 B.2, the
// Stripe shape): `t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>." + body)>` and
// nothing else. A receiver checks v1 and refuses a t more than 5 minutes
// from its clock (docs/API.md). The legacy body-only `sha256=` element —
// nothing in it dated the request, so one captured delivery could be
// replayed for ever — rode along for one release after #791 and was dropped
// in Prompt 18 Part 0 (RP17-6).
func webhookSignature(body []byte, secret string, at time.Time) string {
	return webhooksig.Sign(body, secret, at) // one signature for deliveries and `stored listen`
}

func generateWebhookSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate webhook secret: %w", err)
	}
	return "whsec_" + hex.EncodeToString(b), nil
}

func isValidEventFilter(events []string) bool {
	for _, e := range events {
		if e == "*" {
			continue
		}
		if strings.HasSuffix(e, ".*") {
			prefix := strings.TrimSuffix(e, ".*")
			validPrefix := false
			for _, v := range validEventTypes {
				if strings.HasPrefix(v, prefix+".") {
					validPrefix = true
					break
				}
			}
			if !validPrefix {
				return false
			}
			continue
		}
		if !isValidEventType(e) {
			return false
		}
	}
	return true
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	tenantID, _ := r.Context().Value(tenantIDKey).(string)
	if tenantID == "" {
		writeManagementError(w, ErrTypeAuthentication, "missing_tenant", "tenant not found in token", "")
		return
	}

	if s.db == nil {
		writeListResponse(w, nil, false, "", 0)
		return
	}

	if r.URL.Query().Has("after") {
		s.listEventsAfter(w, r, tenantID) // WP-DX-1: the forward cursor `stored listen` reads
		return
	}

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	cursor := r.URL.Query().Get("cursor")
	typeFilter := r.URL.Query().Get("type")

	var rows *sql.Rows
	var err error

	if typeFilter != "" && cursor != "" {
		rows, err = s.db.QueryContext(r.Context(), `
			SELECT id, type, data, created_at, livemode FROM events
			WHERE tenant_id = $1 AND type = $2 AND created_at < $3
			ORDER BY created_at DESC LIMIT $4`,
			tenantID, typeFilter, cursor, limit+1)
	} else if typeFilter != "" {
		rows, err = s.db.QueryContext(r.Context(), `
			SELECT id, type, data, created_at, livemode FROM events
			WHERE tenant_id = $1 AND type = $2
			ORDER BY created_at DESC LIMIT $3`,
			tenantID, typeFilter, limit+1)
	} else if cursor != "" {
		rows, err = s.db.QueryContext(r.Context(), `
			SELECT id, type, data, created_at, livemode FROM events
			WHERE tenant_id = $1 AND created_at < $2
			ORDER BY created_at DESC LIMIT $3`,
			tenantID, cursor, limit+1)
	} else {
		rows, err = s.db.QueryContext(r.Context(), `
			SELECT id, type, data, created_at, livemode FROM events
			WHERE tenant_id = $1
			ORDER BY created_at DESC LIMIT $2`,
			tenantID, limit+1)
	}
	if err != nil {
		s.logger.Error("list events", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list events", "")
		return
	}
	defer func() { _ = rows.Close() }()

	var items []interface{}
	for rows.Next() {
		var id, eventType string
		var data []byte
		var createdAt time.Time
		var livemode bool
		if err := rows.Scan(&id, &eventType, &data, &createdAt, &livemode); err != nil {
			s.logger.Error("scan event row", zap.Error(err))
			continue
		}
		items = append(items, map[string]interface{}{
			"object":     "event",
			"id":         id,
			"type":       eventType,
			"data":       json.RawMessage(data),
			"created_at": createdAt.Format(time.RFC3339),
			"livemode":   livemode,
		})
	}
	if err := rows.Err(); err != nil {
		s.logger.Warn("iterate rows", zap.Error(err))
	}
	if err := rows.Err(); err != nil {
		s.logger.Warn("iterate rows", zap.Error(err))
	}
	if err := rows.Err(); err != nil {
		s.logger.Warn("iterate rows", zap.Error(err))
	}
	if err := rows.Err(); err != nil {
		s.logger.Warn("iterate rows", zap.Error(err))
	}

	hasMore := len(items) > limit
	nextCursor := ""
	if hasMore {
		items = items[:limit]
		last := items[limit-1].(map[string]interface{})
		nextCursor = last["created_at"].(string)
	}

	var total int
	if typeFilter != "" {
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM events WHERE tenant_id = $1 AND type = $2`,
			tenantID, typeFilter).Scan(&total)
	} else {
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM events WHERE tenant_id = $1`,
			tenantID).Scan(&total)
	}

	writeListResponse(w, items, hasMore, nextCursor, total)
}

// eventsWaitMax bounds a long poll of the forward cursor.
const eventsWaitMax = 25 * time.Second

// eventsMaxWaitsPerTenant bounds one tenant's long polls waiting at once
// (RP80-4: each waiting request asks the database every 500 ms, and the
// routes' rate limit counts requests, not waits). The fifth is 429.
const eventsMaxWaitsPerTenant = 4

// eventsSettleHorizon: an event is answered by the forward cursor only once
// it is this old (RP80-3). created_at is the INSERT's NOW() and the id a
// random uuid, so two events can commit out of their stamps' order; a
// cursor that moved past a later stamp would skip the earlier one for ever.
// An event's single-statement INSERT commits well inside the horizon.
var eventsSettleHorizon = 2 * time.Second

// eventWaits counts each tenant's long polls waiting now.
var eventWaits = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

// acquireEventWait takes one of the tenant's wait slots; false when all
// eventsMaxWaitsPerTenant are taken. The caller releases a slot it got.
func acquireEventWait(tenantID string) bool {
	eventWaits.Lock()
	defer eventWaits.Unlock()
	if eventWaits.n[tenantID] >= eventsMaxWaitsPerTenant {
		return false
	}
	eventWaits.n[tenantID]++
	return true
}

func releaseEventWait(tenantID string) {
	eventWaits.Lock()
	defer eventWaits.Unlock()
	if eventWaits.n[tenantID] <= 1 {
		delete(eventWaits.n, tenantID)
		return
	}
	eventWaits.n[tenantID]--
}

// listEventsAfter is GET /api/v1/events?after=<event id>&wait=<s> (WP-DX-1):
// the tenant's events after that one, OLDEST first (created_at, then id), at
// most limit; next_cursor is the last one's id (the given one when none).
// after=now starts at the newest event: nothing is replayed, its id is the
// cursor. With nothing to answer, wait (≤ 25 s) holds the request and asks
// again every 500 ms — the long poll `stored listen` reads.
func (s *Server) listEventsAfter(w http.ResponseWriter, r *http.Request, tenantID string) {
	q := r.URL.Query()
	after := q.Get("after")
	limit := 100
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l <= 100 {
		limit = l
	}
	var wait time.Duration
	if n, err := strconv.Atoi(q.Get("wait")); err == nil && n > 0 {
		wait = min(time.Duration(n)*time.Second, eventsWaitMax)
	}
	if wait > 0 {
		if !acquireEventWait(tenantID) {
			w.Header().Set("Retry-After", "5")
			writeManagementError(w, ErrTypeRateLimit, "too_many_listeners",
				fmt.Sprintf("at most %d long polls of this account may wait at once", eventsMaxWaitsPerTenant), "wait")
			return
		}
		defer releaseEventWait(tenantID)
	}
	ctx := r.Context()
	if after == "now" {
		var id string
		err := s.db.QueryRowContext(ctx, `SELECT id FROM events WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, tenantID).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.logger.Error("list events after", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list events", "")
			return
		}
		after = id
	}
	if after != "" {
		var known bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id = $1 AND tenant_id = $2)`, after, tenantID).Scan(&known); err != nil {
			s.logger.Error("list events after", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list events", "")
			return
		}
		if !known {
			// Not this tenant's, or past the 90-day retention: start again
			// from after=now.
			writeManagementError(w, ErrTypeInvalidRequest, "unknown_cursor", "no event of this account has that id (expired after 90 days?); start again with after=now", "after")
			return
		}
	}
	deadline := time.Now().Add(wait)
	for {
		items, err := s.eventsAfter(ctx, tenantID, after, limit)
		if err != nil {
			s.logger.Error("list events after", zap.Error(err))
			writeManagementError(w, ErrTypeAPI, "internal_error", "failed to list events", "")
			return
		}
		if len(items) > 0 || !time.Now().Before(deadline) {
			next := after
			if len(items) > 0 {
				next = items[len(items)-1]["id"].(string)
			}
			out := make([]interface{}, len(items))
			for i, it := range items {
				out[i] = it
			}
			writeListResponse(w, out, len(items) == limit, next, len(items))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// eventsAfter reads the events after the one with id after ("" = from the
// first), oldest first, older than the settle horizon (RP80-3).
func (s *Server) eventsAfter(ctx context.Context, tenantID, after string, limit int) ([]map[string]interface{}, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.type, e.data, e.created_at, e.livemode FROM events e
		WHERE e.tenant_id = $1
		  AND ($2 = '' OR (e.created_at, e.id) > (SELECT a.created_at, a.id FROM events a WHERE a.id = $2 AND a.tenant_id = $1))
		  AND e.created_at <= now() - make_interval(secs => $4)
		ORDER BY e.created_at, e.id LIMIT $3`, tenantID, after, limit, eventsSettleHorizon.Seconds())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]interface{}
	for rows.Next() {
		var id, eventType string
		var data []byte
		var createdAt time.Time
		var livemode bool
		if err := rows.Scan(&id, &eventType, &data, &createdAt, &livemode); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{"object": "event", "id": id, "type": eventType, "data": json.RawMessage(data),
			"created_at": createdAt.UTC().Format(time.RFC3339Nano), "livemode": livemode})
	}
	return out, rows.Err()
}
