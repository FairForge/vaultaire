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
//
// Prompt 2a.3 H2: every invalidation moves a generation (per tenant for
// webhooks, per bucket for notification targets). A load stores what it read
// only when the generation it started at is still current — a load that
// raced the CRUD used to cache the old rows for 15 s — and a queued webhook
// job re-reads its endpoints when its tenant's generation moved
// (webhookJob). A lookup that FAILS is remembered for
// deliveryTargetsFailureTTL: the lookup sits in the request (2 s cap), and
// a slow database used to add it to every write.

const (
	deliveryTargetsTTL = 15 * time.Second
	// deliveryTargetsFailureTTL: a failed lookup is answered from the cache
	// (the same error) this long.
	deliveryTargetsFailureTTL = 2 * time.Second
	// deliveryTargetsMaxEntries: past this many entries a write sweeps the
	// expired ones.
	deliveryTargetsMaxEntries = 10000
)

type cachedEndpoints struct {
	list []webhookEndpoint
	err  error
	at   time.Time
	// known is the last list read successfully, kept on a failure entry:
	// the webhooks an event skipped on a failed lookup owe a failed row.
	known []webhookEndpoint
}

type cachedNotifyTargets struct {
	list []notifyTarget
	err  error
	at   time.Time
}

// fresh: a list within the TTL, a failure within the failure TTL.
func cacheFresh(at time.Time, err error, ttl time.Duration) bool {
	if err != nil {
		ttl = deliveryTargetsFailureTTL
	}
	return time.Since(at) < ttl
}

type notifyTargetsKey struct{ tenant, bucket string }

var deliveryTargets = struct {
	mu        sync.Mutex
	webhooks  map[string]cachedEndpoints
	notifies  map[notifyTargetsKey]cachedNotifyTargets
	lookupTTL time.Duration
	// The generations, moved by every invalidation (never removed: one
	// counter per tenant / bucket that ever changed its targets).
	webhookGens map[string]uint64
	notifyGens  map[notifyTargetsKey]uint64
}{
	webhooks:    map[string]cachedEndpoints{},
	notifies:    map[notifyTargetsKey]cachedNotifyTargets{},
	lookupTTL:   deliveryTargetsTTL,
	webhookGens: map[string]uint64{},
	notifyGens:  map[notifyTargetsKey]uint64{},
}

// webhookGeneration is the tenant's webhook generation (moved by the
// webhook CRUD of this process).
func webhookGeneration(tenantID string) uint64 {
	deliveryTargets.mu.Lock()
	defer deliveryTargets.mu.Unlock()
	return deliveryTargets.webhookGens[tenantID]
}

// tenantWebhookEndpoints is the tenant's enabled webhooks and the
// generation they were read at, from the cache when it is fresh (a failure
// is remembered for deliveryTargetsFailureTTL). On a failure the list is
// the last one read successfully (nil when there is none) — the webhooks
// the event will not reach, which owe a failed row.
func tenantWebhookEndpoints(ctx context.Context, db *sql.DB, logger *zap.Logger, tenantID string) ([]webhookEndpoint, uint64, error) {
	deliveryTargets.mu.Lock()
	c, ok := deliveryTargets.webhooks[tenantID]
	gen := deliveryTargets.webhookGens[tenantID]
	deliveryTargets.mu.Unlock()
	if ok && cacheFresh(c.at, c.err, deliveryTargets.lookupTTL) {
		if c.err != nil {
			return c.known, gen, c.err
		}
		return c.list, gen, nil
	}
	list, err := loadWebhookEndpoints(ctx, db, logger, tenantID)
	known := rememberWebhookEndpoints(tenantID, gen, list, err)
	if err != nil {
		return known, gen, err
	}
	return list, gen, nil
}

// rememberWebhookEndpoints caches what a load that started at generation
// gen read (or its failure, with the last list read successfully) — unless
// an invalidation came in between. It returns that last good list.
func rememberWebhookEndpoints(tenantID string, gen uint64, list []webhookEndpoint, loadErr error) (known []webhookEndpoint) {
	deliveryTargets.mu.Lock()
	prev, had := deliveryTargets.webhooks[tenantID]
	if had {
		known = prev.known
		if prev.err == nil {
			known = prev.list
		}
	}
	if deliveryTargets.webhookGens[tenantID] != gen {
		deliveryTargets.mu.Unlock()
		return known
	}
	if len(deliveryTargets.webhooks) > deliveryTargetsMaxEntries {
		for id, c := range deliveryTargets.webhooks {
			if !cacheFresh(c.at, c.err, deliveryTargets.lookupTTL) {
				delete(deliveryTargets.webhooks, id)
			}
		}
	}
	deliveryTargets.webhooks[tenantID] = cachedEndpoints{list: list, err: loadErr, at: time.Now(), known: known}
	deliveryTargets.mu.Unlock()
	return known
}

// bucketNotifyTargets is the bucket's enabled notification targets, from
// the cache when it is fresh.
func (d *NotificationDispatcher) bucketNotifyTargets(ctx context.Context, tenantID, bucket string) ([]notifyTarget, error) {
	k := notifyTargetsKey{tenantID, bucket}
	deliveryTargets.mu.Lock()
	c, ok := deliveryTargets.notifies[k]
	gen := deliveryTargets.notifyGens[k]
	deliveryTargets.mu.Unlock()
	if ok && cacheFresh(c.at, c.err, deliveryTargets.lookupTTL) {
		return c.list, c.err
	}
	list, err := d.Targets(ctx, tenantID, bucket)
	rememberNotifyTargets(tenantID, bucket, gen, list, err)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// notifyGeneration is the bucket's notification-target generation.
func notifyGeneration(tenantID, bucket string) uint64 {
	deliveryTargets.mu.Lock()
	defer deliveryTargets.mu.Unlock()
	return deliveryTargets.notifyGens[notifyTargetsKey{tenantID, bucket}]
}

// rememberNotifyTargets caches what a load that started at generation gen
// read (or its failure) — unless an invalidation came in between.
func rememberNotifyTargets(tenantID, bucket string, gen uint64, list []notifyTarget, loadErr error) {
	k := notifyTargetsKey{tenantID, bucket}
	deliveryTargets.mu.Lock()
	defer deliveryTargets.mu.Unlock()
	if deliveryTargets.notifyGens[k] != gen {
		return
	}
	if len(deliveryTargets.notifies) > deliveryTargetsMaxEntries {
		for k, c := range deliveryTargets.notifies {
			if !cacheFresh(c.at, c.err, deliveryTargets.lookupTTL) {
				delete(deliveryTargets.notifies, k)
			}
		}
	}
	deliveryTargets.notifies[k] = cachedNotifyTargets{list: list, err: loadErr, at: time.Now()}
}

// forgetWebhookEndpoints drops the tenant's cached webhooks and moves its
// generation (webhook CRUD): queued jobs re-read their endpoints.
func forgetWebhookEndpoints(tenantID string) {
	deliveryTargets.mu.Lock()
	delete(deliveryTargets.webhooks, tenantID)
	deliveryTargets.webhookGens[tenantID]++
	deliveryTargets.mu.Unlock()
}

// forgetNotifyTargets drops the bucket's cached notification targets and
// moves its generation (notification configuration, bucket delete).
func forgetNotifyTargets(tenantID, bucket string) {
	k := notifyTargetsKey{tenantID, bucket}
	deliveryTargets.mu.Lock()
	delete(deliveryTargets.notifies, k)
	deliveryTargets.notifyGens[k]++
	deliveryTargets.mu.Unlock()
}
