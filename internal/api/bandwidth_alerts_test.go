package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeBandwidthEmailSender struct {
	mu   sync.Mutex
	sent []bwEmail
}

type bwEmail struct {
	to, subject, html, text string
}

func (f *fakeBandwidthEmailSender) Send(_ context.Context, to, subject, html, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, bwEmail{to, subject, html, text})
	return nil
}

func (f *fakeBandwidthEmailSender) emails() []bwEmail {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]bwEmail, len(f.sent))
	copy(cp, f.sent)
	return cp
}

func TestCheckBandwidthAlerts_FiresAt80(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.MatchExpectationsInOrder(false)

	// 1. Query tenants with bandwidth limits.
	mock.ExpectQuery(`SELECT tenant_id, bandwidth_limit_bytes FROM tenant_quotas`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "bandwidth_limit_bytes"}).
			AddRow("tenant-1", int64(1000)))

	// 2. Current month egress = 800 (80% of 1000).
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(egress_bytes\), 0\)`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(800)))

	// 3. Enabled alerts for tenant.
	mock.ExpectQuery(`SELECT id, threshold_pct, alert_type, last_fired_at FROM bandwidth_alerts`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "threshold_pct", "alert_type", "last_fired_at"}).
			AddRow("alert-1", 80, "email", nil))

	// 4. emitEvent INSERT.
	mock.ExpectExec(`INSERT INTO events`).
		WithArgs(sqlmock.AnyArg(), "bandwidth.alert", "tenant-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 5. Async dispatchWebhooks query — return empty.
	mock.ExpectQuery(`SELECT id, url, event_filter, secret FROM webhook_endpoints`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "url", "event_filter", "secret"}))

	// 6. Tenant email lookup.
	mock.ExpectQuery(`SELECT email FROM tenants WHERE id`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("user@example.com"))

	// 7. Update last_fired_at.
	mock.ExpectExec(`UPDATE bandwidth_alerts SET last_fired_at`).
		WithArgs("alert-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	emailer := &fakeBandwidthEmailSender{}
	alerter := NewBandwidthAlerter(db, zap.NewNop())
	alerter.SetEmailSender(emailer)

	alerter.checkBandwidthAlerts(context.Background())

	// Wait for async dispatchWebhooks goroutine.
	time.Sleep(100 * time.Millisecond)

	emails := emailer.emails()
	require.Len(t, emails, 1)
	assert.Equal(t, "user@example.com", emails[0].to)
	assert.Contains(t, emails[0].subject, "80%")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCheckBandwidthAlerts_BelowThreshold_NoFire(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT tenant_id, bandwidth_limit_bytes FROM tenant_quotas`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "bandwidth_limit_bytes"}).
			AddRow("tenant-1", int64(1000)))

	mock.ExpectQuery(`SELECT COALESCE\(SUM\(egress_bytes\), 0\)`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(700)))

	mock.ExpectQuery(`SELECT id, threshold_pct, alert_type, last_fired_at FROM bandwidth_alerts`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "threshold_pct", "alert_type", "last_fired_at"}).
			AddRow("alert-1", 80, "email", nil))

	emailer := &fakeBandwidthEmailSender{}
	alerter := NewBandwidthAlerter(db, zap.NewNop())
	alerter.SetEmailSender(emailer)

	alerter.checkBandwidthAlerts(context.Background())

	assert.Empty(t, emailer.emails())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCheckBandwidthAlerts_AlreadyFiredThisMonth_NoRefire(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT tenant_id, bandwidth_limit_bytes FROM tenant_quotas`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "bandwidth_limit_bytes"}).
			AddRow("tenant-1", int64(1000)))

	mock.ExpectQuery(`SELECT COALESCE\(SUM\(egress_bytes\), 0\)`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(int64(900)))

	// last_fired_at is this month — should not refire.
	firedThisMonth := time.Now().UTC()
	mock.ExpectQuery(`SELECT id, threshold_pct, alert_type, last_fired_at FROM bandwidth_alerts`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "threshold_pct", "alert_type", "last_fired_at"}).
			AddRow("alert-1", 80, "email", firedThisMonth))

	emailer := &fakeBandwidthEmailSender{}
	alerter := NewBandwidthAlerter(db, zap.NewNop())
	alerter.SetEmailSender(emailer)

	alerter.checkBandwidthAlerts(context.Background())

	assert.Empty(t, emailer.emails())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCheckBandwidthAlerts_NoLimit_Skipped(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// No tenants with bandwidth limits.
	mock.ExpectQuery(`SELECT tenant_id, bandwidth_limit_bytes FROM tenant_quotas`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "bandwidth_limit_bytes"}))

	emailer := &fakeBandwidthEmailSender{}
	alerter := NewBandwidthAlerter(db, zap.NewNop())
	alerter.SetEmailSender(emailer)

	alerter.checkBandwidthAlerts(context.Background())

	assert.Empty(t, emailer.emails())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSeedDefaultAlerts_Idempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT tenant_id FROM tenant_quotas WHERE bandwidth_limit_bytes > 0`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).
			AddRow("tenant-1"))

	mock.ExpectExec(`INSERT INTO bandwidth_alerts`).
		WithArgs("tenant-1", 80).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectExec(`INSERT INTO bandwidth_alerts`).
		WithArgs("tenant-1", 95).
		WillReturnResult(sqlmock.NewResult(0, 1))

	alerter := NewBandwidthAlerter(db, zap.NewNop())
	alerter.seedDefaultAlerts(context.Background())

	require.NoError(t, mock.ExpectationsWereMet())
}

func TestStartBandwidthAlerts_NilSafe(t *testing.T) {
	// Nil receiver.
	var nilAlerter *BandwidthAlerter
	nilAlerter.StartBandwidthAlerts(context.Background()) // must not panic

	// Non-nil receiver, nil db.
	alerter := &BandwidthAlerter{logger: zap.NewNop()}
	alerter.StartBandwidthAlerts(context.Background()) // must not panic
}

func TestBandwidthAlert_EmailContent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.MatchExpectationsInOrder(false)

	limitBytes := int64(100 * 1024 * 1024 * 1024) // 100 GB
	usedBytes := int64(80 * 1024 * 1024 * 1024)   // 80 GB

	mock.ExpectQuery(`SELECT tenant_id, bandwidth_limit_bytes FROM tenant_quotas`).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id", "bandwidth_limit_bytes"}).
			AddRow("tenant-1", limitBytes))

	mock.ExpectQuery(`SELECT COALESCE\(SUM\(egress_bytes\), 0\)`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(usedBytes))

	mock.ExpectQuery(`SELECT id, threshold_pct, alert_type, last_fired_at FROM bandwidth_alerts`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "threshold_pct", "alert_type", "last_fired_at"}).
			AddRow("alert-1", 80, "email", nil))

	mock.ExpectExec(`INSERT INTO events`).
		WithArgs(sqlmock.AnyArg(), "bandwidth.alert", "tenant-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(`SELECT id, url, event_filter, secret FROM webhook_endpoints`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "url", "event_filter", "secret"}))

	mock.ExpectQuery(`SELECT email FROM tenants WHERE id`).
		WithArgs("tenant-1").
		WillReturnRows(sqlmock.NewRows([]string{"email"}).AddRow("admin@acme.com"))

	mock.ExpectExec(`UPDATE bandwidth_alerts SET last_fired_at`).
		WithArgs("alert-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	emailer := &fakeBandwidthEmailSender{}
	alerter := NewBandwidthAlerter(db, zap.NewNop())
	alerter.SetEmailSender(emailer)

	alerter.checkBandwidthAlerts(context.Background())

	time.Sleep(100 * time.Millisecond)

	emails := emailer.emails()
	require.Len(t, emails, 1)
	assert.Equal(t, "admin@acme.com", emails[0].to)
	assert.Contains(t, emails[0].subject, "80%")
	assert.True(t, strings.Contains(emails[0].text, "80.0 GB"), "body should contain used bytes: %s", emails[0].text)
	assert.True(t, strings.Contains(emails[0].text, "100.0 GB"), "body should contain limit bytes: %s", emails[0].text)
	assert.Contains(t, emails[0].text, "80%")
	require.NoError(t, mock.ExpectationsWereMet())
}
