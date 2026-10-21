package payments

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The generic claim (docs/PAYMENTS.md § 2.4, WP-PAY-4 PR 2): committed =
// duplicate; rolled back = the redelivery takes it; a concurrent delivery
// waits on the uncommitted row; the tenant is written on commit.

func (f *fixture) claimID(suffix string) string { return "claim-" + f.tenantID + "-" + suffix }

func (f *fixture) cleanupClaims(t *testing.T) {
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM payment_events WHERE provider = 'btcpay' AND event_id LIKE $1`, "claim-"+f.tenantID+"-%")
	})
}

func TestClaimEvent_CommittedIsDuplicate_TenantWritten(t *testing.T) {
	f := newFixture(t)
	f.cleanupClaims(t)
	ctx := context.Background()

	c, dup, err := ClaimEvent(ctx, f.db, ProviderBTCPay, f.claimID("a"), "InvoiceSettled", []byte(`{"x":1}`))
	require.NoError(t, err)
	require.False(t, dup)
	require.NoError(t, c.Commit(ctx, f.tenantID))

	c2, dup, err := ClaimEvent(ctx, f.db, ProviderBTCPay, f.claimID("a"), "InvoiceSettled", []byte(`{"x":1}`))
	require.NoError(t, err)
	assert.True(t, dup)
	assert.Nil(t, c2)
	var tenant, typ string
	require.NoError(t, f.db.QueryRow(`SELECT tenant_id, event_type FROM payment_events WHERE provider = 'btcpay' AND event_id = $1`,
		f.claimID("a")).Scan(&tenant, &typ))
	assert.Equal(t, f.tenantID, tenant)
	assert.Equal(t, "InvoiceSettled", typ)
}

func TestClaimEvent_RolledBackIsTakenByTheRedelivery(t *testing.T) {
	f := newFixture(t)
	f.cleanupClaims(t)
	ctx := context.Background()

	c, dup, err := ClaimEvent(ctx, f.db, ProviderBTCPay, f.claimID("b"), "InvoiceSettled", []byte(`{}`))
	require.NoError(t, err)
	require.False(t, dup)
	c.Rollback()

	c2, dup, err := ClaimEvent(ctx, f.db, ProviderBTCPay, f.claimID("b"), "InvoiceSettled", []byte(`{}`))
	require.NoError(t, err)
	assert.False(t, dup, "a failed handler leaves the event to the redelivery")
	require.NoError(t, c2.Commit(ctx, ""))
}

func TestClaimEvent_ConcurrentDeliveryWaitsThenDuplicate(t *testing.T) {
	f := newFixture(t)
	f.cleanupClaims(t)
	ctx := context.Background()
	c, _, err := ClaimEvent(ctx, f.db, ProviderBTCPay, f.claimID("c"), "InvoiceSettled", []byte(`{}`))
	require.NoError(t, err)

	type result struct {
		dup bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		c2, dup, err := ClaimEvent(ctx, f.db, ProviderBTCPay, f.claimID("c"), "InvoiceSettled", []byte(`{}`))
		if c2 != nil {
			c2.Rollback()
		}
		done <- result{dup, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the second delivery did not wait on the uncommitted claim: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, c.Commit(ctx, f.tenantID))
	r := <-done
	require.NoError(t, r.err)
	assert.True(t, r.dup)
}
