package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

const maxNotificationBodyBytes = 65536

// S3 notification XML types

type NotificationConfiguration struct {
	XMLName xml.Name             `xml:"NotificationConfiguration"`
	Xmlns   string               `xml:"xmlns,attr,omitempty"`
	Topics  []TopicConfiguration `xml:"TopicConfiguration,omitempty"`
}

type TopicConfiguration struct {
	ID     string   `xml:"Id,omitempty"`
	Topic  string   `xml:"Topic"`
	Events []string `xml:"Event"`
}

func (s *Server) handleGetBucketNotification(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(xml.Header))
		_, _ = w.Write([]byte(`<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`))
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, event_filter, target_url
		FROM bucket_notifications
		WHERE tenant_id = $1 AND bucket = $2
		ORDER BY created_at ASC
	`, t.ID, req.Bucket)
	if err != nil {
		s.logger.Error("query bucket notifications", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	defer func() { _ = rows.Close() }()

	topicMap := make(map[string]*TopicConfiguration)
	var topicOrder []string

	for rows.Next() {
		var id, eventFilter, targetURL string
		if err := rows.Scan(&id, &eventFilter, &targetURL); err != nil {
			s.logger.Error("scan notification row", zap.Error(err))
			continue
		}
		key := targetURL
		tc, ok := topicMap[key]
		if !ok {
			tc = &TopicConfiguration{
				ID:    id,
				Topic: targetURL,
			}
			topicMap[key] = tc
			topicOrder = append(topicOrder, key)
		}
		tc.Events = append(tc.Events, eventFilter)
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("notification rows iteration", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	resp := NotificationConfiguration{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/",
	}
	for _, key := range topicOrder {
		resp.Topics = append(resp.Topics, *topicMap[key])
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePutBucketNotification(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}

	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxNotificationBodyBytes))
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	var config NotificationConfiguration
	if err := xml.Unmarshal(body, &config); err != nil {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		s.logger.Error("begin tx for notification config", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(r.Context(),
		`DELETE FROM bucket_notifications WHERE tenant_id = $1 AND bucket = $2`,
		t.ID, req.Bucket)
	if err != nil {
		s.logger.Error("clear notification config", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	for _, tc := range config.Topics {
		if tc.Topic == "" || len(tc.Events) == 0 {
			continue
		}
		for _, event := range tc.Events {
			if !isValidS3EventFilter(event) {
				WriteS3Error(w, ErrInvalidRequest, r.URL.Path, generateRequestID())
				return
			}
			_, err = tx.ExecContext(r.Context(), `
				INSERT INTO bucket_notifications (tenant_id, bucket, event_filter, target_type, target_url)
				VALUES ($1, $2, $3, 'webhook', $4)
			`, t.ID, req.Bucket, event, tc.Topic)
			if err != nil {
				s.logger.Error("insert notification config", zap.Error(err))
				WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
				return
			}
		}
	}

	if err := tx.Commit(); err != nil {
		s.logger.Error("commit notification config", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}

	s.logger.Info("bucket notification config updated",
		zap.String("tenant_id", t.ID),
		zap.String("bucket", req.Bucket),
		zap.Int("topics", len(config.Topics)))

	w.WriteHeader(http.StatusOK)
}

func isValidS3EventFilter(event string) bool {
	valid := []string{
		"s3:ObjectCreated:*",
		"s3:ObjectCreated:Put",
		"s3:ObjectCreated:Post",
		"s3:ObjectCreated:Copy",
		"s3:ObjectCreated:CompleteMultipartUpload",
		"s3:ObjectRemoved:*",
		"s3:ObjectRemoved:Delete",
		"s3:ObjectRemoved:DeleteMarkerCreated",
		"s3:*",
	}
	for _, v := range valid {
		if event == v {
			return true
		}
	}
	return false
}

// NotificationDispatcher fires S3 event notifications asynchronously.
type NotificationDispatcher struct {
	db     *sql.DB
	logger *zap.Logger
	client *http.Client
}

func NewNotificationDispatcher(db *sql.DB, logger *zap.Logger) *NotificationDispatcher {
	if db == nil {
		return nil
	}
	return &NotificationDispatcher{
		db:     db,
		logger: logger,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// S3Event is the S3-compatible notification payload.
type S3Event struct {
	Records []S3EventRecord `json:"Records"`
}

type S3EventRecord struct {
	EventVersion string          `json:"eventVersion"`
	EventSource  string          `json:"eventSource"`
	EventName    string          `json:"eventName"`
	EventTime    string          `json:"eventTime"`
	S3           S3EventS3       `json:"s3"`
	UserIdentity S3EventIdentity `json:"userIdentity"`
}

type S3EventS3 struct {
	Bucket S3EventBucket `json:"bucket"`
	Object S3EventObject `json:"object"`
}

type S3EventBucket struct {
	Name string `json:"name"`
}

type S3EventObject struct {
	Key  string `json:"key"`
	Size int64  `json:"size,omitempty"`
	ETag string `json:"eTag,omitempty"`
}

type S3EventIdentity struct {
	PrincipalID string `json:"principalId"`
}

// Fire dispatches a notification event asynchronously.
func (d *NotificationDispatcher) Fire(tenantID, bucket, eventName, objectKey string, size int64, etag string) {
	if d == nil {
		return
	}

	go d.dispatch(tenantID, bucket, eventName, objectKey, size, etag)
}

func (d *NotificationDispatcher) dispatch(tenantID, bucket, eventName, objectKey string, size int64, etag string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := d.db.QueryContext(ctx, `
		SELECT target_url, event_filter
		FROM bucket_notifications
		WHERE tenant_id = $1 AND bucket = $2 AND enabled = TRUE
	`, tenantID, bucket)
	if err != nil {
		d.logger.Error("query notifications for dispatch",
			zap.Error(err),
			zap.String("tenant_id", tenantID),
			zap.String("bucket", bucket))
		return
	}
	defer func() { _ = rows.Close() }()

	type target struct {
		url    string
		filter string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.url, &t.filter); err != nil {
			d.logger.Error("scan notification target", zap.Error(err))
			continue
		}
		targets = append(targets, t)
	}

	payload := S3Event{
		Records: []S3EventRecord{{
			EventVersion: "2.1",
			EventSource:  "stored.ge",
			EventName:    eventName,
			EventTime:    time.Now().UTC().Format(time.RFC3339),
			S3: S3EventS3{
				Bucket: S3EventBucket{Name: bucket},
				Object: S3EventObject{Key: objectKey, Size: size, ETag: etag},
			},
			UserIdentity: S3EventIdentity{PrincipalID: tenantID},
		}},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		d.logger.Error("marshal notification payload", zap.Error(err))
		return
	}

	for _, t := range targets {
		if !matchesEventFilter(t.filter, eventName) {
			continue
		}
		d.deliver(ctx, t.url, body, eventName)
	}
}

func matchesEventFilter(filter, eventName string) bool {
	if filter == "s3:*" {
		return true
	}
	if filter == eventName {
		return true
	}
	if strings.HasSuffix(filter, ":*") {
		prefix := strings.TrimSuffix(filter, ":*")
		if strings.HasPrefix(eventName, prefix+":") {
			return true
		}
	}
	return false
}

func (d *NotificationDispatcher) deliver(ctx context.Context, url string, body []byte, eventName string) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		d.logger.Error("create notification request",
			zap.Error(err),
			zap.String("url", url))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-S3-Event", eventName)

	resp, err := d.client.Do(req)
	if err != nil {
		d.logger.Warn("notification delivery failed",
			zap.Error(err),
			zap.String("url", url),
			zap.String("event", eventName))
		return
	}
	_ = resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		d.logger.Debug("notification delivered",
			zap.String("url", url),
			zap.String("event", eventName),
			zap.Int("status", resp.StatusCode))
	} else {
		d.logger.Warn("notification delivery non-2xx",
			zap.String("url", url),
			zap.String("event", eventName),
			zap.Int("status", resp.StatusCode))
	}
}

// FireSync dispatches a notification event synchronously (for testing).
func (d *NotificationDispatcher) FireSync(tenantID, bucket, eventName, objectKey string, size int64, etag string) {
	if d == nil {
		return
	}
	d.dispatch(tenantID, bucket, eventName, objectKey, size, etag)
}

// SetHTTPClient replaces the HTTP client (for testing).
func (d *NotificationDispatcher) SetHTTPClient(c *http.Client) {
	if d == nil {
		return
	}
	d.client = c
}
