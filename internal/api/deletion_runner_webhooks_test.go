package api

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountDeletionRunner_TheErasureMovesTheWebhookGeneration(t *testing.T) {
	// Prompt 2b 0.6: EraseRows deletes the tenant's webhook endpoints. Before:
	// the generation stayed (0 → 0), so webhook jobs queued at the erasure
	// still POSTed with the captured URL and secret, and the cached list was
	// served for up to 15 s.
	f := setupDeletionFixture(t)
	f.seed(time.Now().Add(-time.Hour))
	rememberWebhookEndpoints(f.tenantID, webhookGeneration(f.tenantID), []webhookEndpoint{{id: "cached"}}, nil)
	before := webhookGeneration(f.tenantID)

	_, err := f.sj.runWith(context.Background(), func(ctx context.Context) (jobReport, error) {
		_, runErr := f.runner.RunOnce(ctx)
		return jobReport{}, runErr
	})
	require.NoError(t, err)

	assert.Greater(t, webhookGeneration(f.tenantID), before)
	deliveryTargets.mu.Lock()
	_, cached := deliveryTargets.webhooks[f.tenantID]
	deliveryTargets.mu.Unlock()
	assert.False(t, cached)
}
