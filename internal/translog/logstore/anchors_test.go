package logstore

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
)

// K4c-3a (migration 106): an anchor row is never rewritten — an OTS upgrade
// is a new row — and never deleted: the table is append-only like the log's
// others (UPDATE/DELETE/TRUNCATE raise).
func TestTranslogAnchors_AppendOnly(t *testing.T) {
	// Arrange: one signed checkpoint and one anchor of it.
	ctx := context.Background()
	db := logstoretest.PrivateLog(t)
	signer, _ := testSigner(t)
	s := New(db, signer)
	_, _, err := s.Append(ctx, ceremony(t))
	require.NoError(t, err)
	res, err := s.Checkpoint(ctx)
	require.NoError(t, err)
	require.True(t, res.Signed)
	d := sha256.Sum256([]byte("note"))
	a := Anchor{Size: res.Size, Kind: AnchorKindOTS, Service: "https://a.example", State: AnchorPending, Proof: []byte{1}, Digest: d[:]}
	require.NoError(t, s.AddAnchor(ctx, a))
	require.NoError(t, s.AddAnchor(ctx, a), "the same answer twice is kept once")

	// Act + Assert
	for _, q := range []string{
		`UPDATE translog_anchors SET state = 'complete'`,
		`UPDATE translog_anchors SET proof = '\x02'`,
		`DELETE FROM translog_anchors`,
		`TRUNCATE translog_anchors`,
	} {
		_, err := db.ExecContext(ctx, q)
		require.Error(t, err, q)
		assert.Contains(t, err.Error(), "append-only", q)
	}
	rows, err := s.AllAnchors(ctx, res.Size)
	require.NoError(t, err)
	assert.Equal(t, []Anchor{withTime(a, rows)}, rows)
	// The checks of migration 106.
	for name, bad := range map[string]Anchor{
		"kind":    {Size: res.Size, Kind: "tsa", Service: "https://a.example", State: AnchorPending, Proof: []byte{1}, Digest: d[:]},
		"state":   {Size: res.Size, Kind: AnchorKindOTS, Service: "https://a.example", State: "upgraded", Proof: []byte{1}, Digest: d[:]},
		"digest":  {Size: res.Size, Kind: AnchorKindOTS, Service: "https://a.example", State: AnchorComplete, Proof: []byte{1}, Digest: d[:31]},
		"proof":   {Size: res.Size, Kind: AnchorKindOTS, Service: "https://a.example", State: AnchorComplete, Proof: make([]byte, 65537), Digest: d[:]},
		"service": {Size: res.Size, Kind: AnchorKindOTS, Service: "a.example", State: AnchorComplete, Proof: []byte{1}, Digest: d[:]},
		"size":    {Size: res.Size + 1, Kind: AnchorKindOTS, Service: "https://a.example", State: AnchorComplete, Proof: []byte{1}, Digest: d[:]},
	} {
		assert.Error(t, s.AddAnchor(ctx, bad), name)
	}
}

// withTime is a with the received_at the table gave it.
func withTime(a Anchor, rows []Anchor) Anchor {
	if len(rows) > 0 {
		a.ReceivedAt = rows[0].ReceivedAt
	}
	return a
}
