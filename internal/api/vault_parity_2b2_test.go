package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Prompt 2b.2 C4 — the parity job's leftovers from the review of #651.
// "Before" numbers: these tests run against 2b8adc3.

func (f *parityFixture) age(key string, by time.Duration) {
	f.t.Helper()
	_, err := f.db.Exec(`UPDATE object_head_cache SET updated_at = NOW() - $3::interval WHERE tenant_id = $1 AND object_key = $2`,
		f.tenantID, key, fmt.Sprintf("%d seconds", int(by.Seconds())))
	require.NoError(f.t, err)
}

func TestVaultParity_ATooLargeObjectNeverTakesANewerObjectsSlot(t *testing.T) {
	// Arrange: one object larger than a whole run can protect, older than a
	// small one; two slots per run. Before: the large one stayed at the
	// head of `ORDER BY updated_at LIMIT n` with no row and took a slot
	// every run — with one slot, the newer object was never protected.
	f := setupParityFixture(t)
	f.svc.MaxObjectsPerRun = 1
	f.svc.MaxRunTime = time.Second
	f.svc.ProtectRate = 4 * int64(f.stripe) // the large one's estimate: > 1 s
	f.object("huge.bin", 8*f.stripe)
	f.age("huge.bin", time.Hour)
	f.object("small.bin", f.stripe/2)

	// Act
	var res VaultParityResult
	for i := 0; i < 2; i++ {
		res = f.run()
	}

	// Assert
	state, _, _, _ := f.row("small.bin")
	assert.Equal(t, "complete", state, "the newer object is protected")
	huge, _, _, _ := f.row("huge.bin")
	assert.Equal(t, "", huge, "never started, no attempt spent")
	assert.Equal(t, 1, res.TooLarge, "%+v", res)
	assert.Equal(t, float64(1), testutil.ToFloat64(vaultParityUnprotectedObjects.WithLabelValues("too_large")))
	assert.Equal(t, float64(8*f.stripe), testutil.ToFloat64(vaultParityUnprotectedBytes))
}

// A candidate the job skips without protecting it — a chunked object (a
// broken invariant) or a tenant whose flag is off — must not take a slot
// either: they are skipped by the same pass.
func TestVaultParity_ASkippedCandidateNeverTakesANewerObjectsSlot(t *testing.T) {
	f := setupParityFixture(t)
	f.svc.MaxObjectsPerRun = 1
	f.object("chunked.bin", f.stripe)
	_, err := f.db.Exec(`UPDATE object_head_cache SET is_chunked = true WHERE tenant_id = $1 AND object_key = 'chunked.bin'`, f.tenantID)
	require.NoError(t, err)
	f.age("chunked.bin", time.Hour)
	f.object("fresh.bin", f.stripe/2)

	f.run()
	f.run()

	state, _, _, _ := f.row("fresh.bin")
	assert.Equal(t, "complete", state)
}

func TestVaultParity_AFlagOffTenantIsSkippedWithoutASlot(t *testing.T) {
	f := setupParityFixture(t)
	f.svc.MaxObjectsPerRun = 1
	f.object("a.bin", f.stripe/2)
	f.svc.flags = stubFlags{on: map[string]bool{flagVaultParity + "/" + f.tenantID: false}}

	res := f.run()

	assert.Equal(t, 1, res.FlagOff, "%+v", res)
	state, _, _, _ := f.row("a.bin")
	assert.Equal(t, "", state)
}

func TestVaultParity_AFinishNeverCompletesARowADeleteTouchedMeanwhile(t *testing.T) {
	// Arrange: a protect has written its four shards; before it records the
	// row, the object is deleted (the API erases shards p0, p1, then the leg
	// answers 502 — the row stays) and re-uploaded with the same bytes
	// (same etag). Before: finishRow checked only the etag and marked the
	// row `complete` over the two shards the delete had just removed.
	f := setupParityFixture(t)
	size := f.stripe + 3
	_, etag := f.object("race.bin", size)
	f.svc.beforeFinish = func(c parityCandidate) {
		f.svc.beforeFinish = nil
		f.eng.AddDriver("permafrost", &failDeletesAfterLeg{flakyLegDriver: f.leg, after: 2})
		f.dropHead("race.bin")
		f.svc.OnObjectDeleted(context.Background(), f.tenantID, f.bucket, "race.bin")
		f.headRow("race.bin", etag, size)
		f.eng.AddDriver("permafrost", f.leg)
	}

	// Act
	f.run()

	// Assert: never complete over missing shards …
	state, _, _, _ := f.row("race.bin")
	if state == "complete" {
		assert.Len(t, f.shardFiles(), 4, "a complete row names four shards that exist")
	}
	assert.NotEqual(t, "complete", state, "the delete touched the row: the finish does not complete it")
	// … and the re-upload is protected by the next run.
	f.run()
	state, _, _, _ = f.row("race.bin")
	assert.Equal(t, "complete", state)
	assert.Len(t, f.shardFiles(), 4)
}

// A webhook event whose targets could not be read is counted once as
// lookup_failed. Before: its failed-row job, dropped again by a full pool,
// was counted under overloaded too, and its rows carried the pool's reason.
func TestEventDelivery_ALookupFailedEventDroppedByAFullPoolIsCountedOnce(t *testing.T) {
	f := setupBatchFanoutFixture(t)
	hook := f.webhook("https://hooks.example.invalid/x", "*")
	eventID := f.event()
	_, gen, err := tenantWebhookEndpoints(context.Background(), f.db, zap.NewNop(), f.tenantID)
	require.NoError(t, err)
	rememberWebhookEndpoints(f.tenantID, gen, nil, errors.New("database: connection refused"))
	useDeliveryPool(t, newDeliveryPool(1, 0)) // every submit is dropped: the pool is full
	lookup := func() float64 {
		return testutil.ToFloat64(deliveriesDropped.WithLabelValues(deliveryKindWebhook, "lookup_failed"))
	}
	overload := func() float64 {
		return testutil.ToFloat64(deliveriesDropped.WithLabelValues(deliveryKindWebhook, "overloaded"))
	}
	l0, o0 := lookup(), overload()

	submitEventDelivery(context.Background(), f.db, zap.NewNop(), eventID, "object.deleted", f.tenantID, []byte(`{}`))

	assert.Equal(t, l0+1, lookup())
	assert.Equal(t, o0, overload(), "the same event is not counted twice")
	assert.Equal(t, 1, webhookRowsFor(t, f.db, hook, "failed", deliveryDroppedLookupFailed), "its row says why it was skipped")
}
