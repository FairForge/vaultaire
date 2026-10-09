package api

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Where a request's notification and event go is read through a short-TTL
// cache (deliveryTargetsTTL): the tenant's enabled webhook_endpoints and the
// (tenant, bucket)'s enabled bucket_notifications. A tenant or bucket with
// none submits nothing to the delivery pool and runs no query in the
// request once the lists are cached — before (post-merge review of
// d589da6) every PUT and GET submitted a job that read them, so one
// tenant's blackholed webhook filled the shared queue with every other
// tenant's lookups. Changes made through this process (webhook CRUD,
// PutBucketNotificationConfiguration, DeleteBucket) invalidate at once; the
// other slot sees them within the TTL.

const (
	deliveryTargetsTTL = 15 * time.Second
	// deliveryTargetsMaxEntries: past this many entries a write sweeps the
	// expired ones.
	deliveryTargetsMaxEntries = 10000
)

type cachedEndpoints struct {
	list []webhookEndpoint
	at   time.Time
}

type cachedNotifyTargets struct {
	list []notifyTarget
	at   time.Time
}

type notifyTargetsKey struct{ tenant, bucket string }

var deliveryTargets = struct {
	mu        sync.Mutex
	webhooks  map[string]cachedEndpoints
	notifies  map[notifyTargetsKey]cachedNotifyTargets
	lookupTTL time.Duration
}{
	webhooks:  map[string]cachedEndpoints{},
	notifies:  map[notifyTargetsKey]cachedNotifyTargets{},
	lookupTTL: deliveryTargetsTTL,
}

// tenantWebhookEndpoints is the tenant's enabled webhooks, from the cache
// when it is fresh. A load that fails is not cached.
func tenantWebhookEndpoints(ctx context.Context, db *sql.DB, logger *zap.Logger, tenantID string) ([]webhookEndpoint, error) {
	deliveryTargets.mu.Lock()
	c, ok := deliveryTargets.webhooks[tenantID]
	deliveryTargets.mu.Unlock()
	if ok && time.Since(c.at) < deliveryTargets.lookupTTL {
		return c.list, nil
	}
	list, err := loadWebhookEndpoints(ctx, db, logger, tenantID)
	if err != nil {
		return nil, err
	}
	rememberWebhookEndpoints(tenantID, list)
	return list, nil
}

func rememberWebhookEndpoints(tenantID string, list []webhookEndpoint) {
	deliveryTargets.mu.Lock()
	if len(deliveryTargets.webhooks) > deliveryTargetsMaxEntries {
		for id, c := range deliveryTargets.webhooks {
			if time.Since(c.at) >= deliveryTargets.lookupTTL {
				delete(deliveryTargets.webhooks, id)
			}
		}
	}
	deliveryTargets.webhooks[tenantID] = cachedEndpoints{list: list, at: time.Now()}
	deliveryTargets.mu.Unlock()
}

// bucketNotifyTargets is the bucket's enabled notification targets, from
// the cache when it is fresh.
func (d *NotificationDispatcher) bucketNotifyTargets(ctx context.Context, tenantID, bucket string) ([]notifyTarget, error) {
	k := notifyTargetsKey{tenantID, bucket}
	deliveryTargets.mu.Lock()
	c, ok := deliveryTargets.notifies[k]
	deliveryTargets.mu.Unlock()
	if ok && time.Since(c.at) < deliveryTargets.lookupTTL {
		return c.list, nil
	}
	list, err := d.Targets(ctx, tenantID, bucket)
	if err != nil {
		return nil, err
	}
	rememberNotifyTargets(tenantID, bucket, list)
	return list, nil
}

func rememberNotifyTargets(tenantID, bucket string, list []notifyTarget) {
	deliveryTargets.mu.Lock()
	if len(deliveryTargets.notifies) > deliveryTargetsMaxEntries {
		for k, c := range deliveryTargets.notifies {
			if time.Since(c.at) >= deliveryTargets.lookupTTL {
				delete(deliveryTargets.notifies, k)
			}
		}
	}
	deliveryTargets.notifies[notifyTargetsKey{tenantID, bucket}] = cachedNotifyTargets{list: list, at: time.Now()}
	deliveryTargets.mu.Unlock()
}

// forgetWebhookEndpoints drops the tenant's cached webhooks (webhook CRUD).
func forgetWebhookEndpoints(tenantID string) {
	deliveryTargets.mu.Lock()
	delete(deliveryTargets.webhooks, tenantID)
	deliveryTargets.mu.Unlock()
}

// forgetNotifyTargets drops the bucket's cached notification targets
// (notification configuration, bucket delete).
func forgetNotifyTargets(tenantID, bucket string) {
	deliveryTargets.mu.Lock()
	delete(deliveryTargets.notifies, notifyTargetsKey{tenantID, bucket})
	deliveryTargets.mu.Unlock()
}
