package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Prompt 2b Part 0 (0.6): an event skipped because its targets could not be
// read is counted, and a webhook known from the last good read gets its
// failed row. "Before" numbers are the same scenario run against 0b376cf.

func TestEventTargets_AnEventSkippedOnAFailedLookupIsCountedAndRecorded(t *testing.T) {
	// Arrange: the tenant's webhook was read once; then a lookup fails (the
	// failure is answered from the cache for 2 s). Before: the event and the
	// notification were skipped with no metric and no delivery row.
	f := setupBatchFanoutFixture(t)
	hook := f.webhook("https://hooks.example.invalid/x", "*")
	eventID := f.event()
	_, gen, err := tenantWebhookEndpoints(context.Background(), f.db, zap.NewNop(), f.tenantID)
	require.NoError(t, err)
	rememberWebhookEndpoints(f.tenantID, gen, nil, errors.New("database: connection refused"))
	notify := NewNotificationDispatcher(f.db, zap.NewNop())
	rememberNotifyTargets(f.tenantID, "b", notifyGeneration(f.tenantID, "b"), nil, errors.New("database: connection refused"))
	webhooks := func() float64 {
		return testutil.ToFloat64(deliveriesDropped.WithLabelValues(deliveryKindWebhook, "lookup_failed"))
	}
	notifications := func() float64 {
		return testutil.ToFloat64(deliveriesDropped.WithLabelValues(deliveryKindNotification, "lookup_failed"))
	}
	w0, n0 := webhooks(), notifications()

	// Act
	submitEventDelivery(context.Background(), f.db, zap.NewNop(), eventID, "object.deleted", f.tenantID, []byte(`{}`))
	notify.Fire(f.tenantID, "b", "s3:ObjectRemoved:Delete", "k", 0, "")

	// Assert
	assert.Equal(t, w0+1, webhooks())
	assert.Equal(t, n0+1, notifications())
	assert.Eventually(t, func() bool {
		return webhookRowsFor(t, f.db, hook, "failed", deliveryDroppedLookupFailed) == 1
	}, 5*time.Second, 20*time.Millisecond, "the known webhook gets its failed row")
}
