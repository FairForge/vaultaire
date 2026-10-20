package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"go.uber.org/zap"
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
}

// recordEvent inserts the events row; false when nothing was recorded.
func recordEvent(ctx context.Context, db *sql.DB, logger *zap.Logger, eventType, tenantID string, data map[string]interface{}) (recordedEvent, bool) {
	ev := recordedEvent{id: uuid.New().String(), eventType: eventType, tenantID: tenantID}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		logger.Error("marshal event data", zap.Error(err))
		return recordedEvent{}, false
	}
	ev.data = dataJSON

	err = db.QueryRowContext(ctx, `
		INSERT INTO events (id, type, tenant_id, data)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at`,
		ev.id, eventType, tenantID, dataJSON).Scan(&ev.createdAt)
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

// generateWebhookSignature is the legacy signature: HMAC-SHA256 of the body
// alone — nothing in it dates the request, so one captured delivery could
// be replayed for ever.
func generateWebhookSignature(payload []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}

// webhookSignature is the X-Webhook-Signature value (WP-R11-3 B.2, the
// Stripe shape): `t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>." + body)>`,
// followed for one release by the legacy `sha256=<hex HMAC(body)>` element
// so a receiver that verifies the body-only HMAC keeps working while it
// moves to v1. A receiver checks v1 and refuses a t more than 5 minutes
// from its clock (docs/API.md).
func webhookSignature(body []byte, secret string, at time.Time) string {
	t := strconv.FormatInt(at.Unix(), 10)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(t))
	h.Write([]byte("."))
	h.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(h.Sum(nil)) + "," + generateWebhookSignature(body, secret)
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
			SELECT id, type, data, created_at FROM events
			WHERE tenant_id = $1 AND type = $2 AND created_at < $3
			ORDER BY created_at DESC LIMIT $4`,
			tenantID, typeFilter, cursor, limit+1)
	} else if typeFilter != "" {
		rows, err = s.db.QueryContext(r.Context(), `
			SELECT id, type, data, created_at FROM events
			WHERE tenant_id = $1 AND type = $2
			ORDER BY created_at DESC LIMIT $3`,
			tenantID, typeFilter, limit+1)
	} else if cursor != "" {
		rows, err = s.db.QueryContext(r.Context(), `
			SELECT id, type, data, created_at FROM events
			WHERE tenant_id = $1 AND created_at < $2
			ORDER BY created_at DESC LIMIT $3`,
			tenantID, cursor, limit+1)
	} else {
		rows, err = s.db.QueryContext(r.Context(), `
			SELECT id, type, data, created_at FROM events
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
		if err := rows.Scan(&id, &eventType, &data, &createdAt); err != nil {
			s.logger.Error("scan event row", zap.Error(err))
			continue
		}
		items = append(items, map[string]interface{}{
			"object":     "event",
			"id":         id,
			"type":       eventType,
			"data":       json.RawMessage(data),
			"created_at": createdAt.Format(time.RFC3339),
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
