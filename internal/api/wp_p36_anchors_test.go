package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/logstore/logstoretest"
	"github.com/FairForge/vaultaire/internal/translog/ots"
	"github.com/FairForge/vaultaire/internal/translog/ots/otstest"
)

// Stage 1a Prompt 36 Part 0 — the WP-P35 review's RP48 on the anchor job:
// RP48-1 the two-calendar floor held in configuration only (a pool's answer
// was accepted and never upgraded; the stale rule fired only when EVERY
// calendar stopped), RP48-2 sixteen lost commitments starved every newer
// proof for ever and nothing re-submitted or alerted, RP48-3 a bogus
// complete row replaced its own good pending path in the served file.

func newAnchorLabOne(t *testing.T) (*logstore.Store, *translogAnchorer, *otstest.Calendar) {
	t.Helper()
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	store := logstore.New(logstoretest.PrivateLog(t), signer)
	cal := otstest.New(t)
	anchors := []translogAnchor{{Kind: logstore.AnchorKindOTS, Service: cal.URL}}
	logstore.SetAnchors(anchorServices(anchors))
	job := newTranslogAnchorer(store, anchors, nil, zap.NewNop())
	job.JobName = "translog_anchor_p36_test"
	job.upgradeAfter = 0
	return store, job, cal
}

func signOne(t *testing.T, store *logstore.Store, participants int) logstore.Stored {
	t.Helper()
	ctx := context.Background()
	_, _, err := store.Append(ctx, translog.KeyCeremony{Action: translog.ActionKeyInstalled, KeyRole: translog.KeyTranslogSigningKey,
		KeyFingerprint: "sha256:" + strings.Repeat("4", 64), Participants: participants})
	require.NoError(t, err)
	res, err := store.Checkpoint(ctx)
	require.NoError(t, err)
	require.True(t, res.Signed, "%+v", res)
	cp, ok, err := store.LatestCheckpoint(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	return cp
}

func runAnchor(t *testing.T, job *translogAnchorer) anchorResult {
	t.Helper()
	rep, err := job.spec().Run(context.Background())
	require.NoError(t, err, "a calendar never fails the job")
	res, ok := rep.Result.(anchorResult)
	require.True(t, ok)
	return res
}

// RP48-1 (a): an answer whose pending attestation names another URL — every
// aggregator pool does this — is refused: ErrBadAnswer naming both URLs,
// nothing stored, bad_answer counted. Before, it was accepted and then never
// upgraded (Upgrade asks only the configured calendar), an anchor that could
// never complete.
func TestAnchorJob_PoolAnswer_RefusedNothingStored(t *testing.T) {
	// Arrange: the calendar answers like a pool — its pending attestations
	// name an aggregated calendar, not itself.
	store, job, cal := newAnchorLabOne(t)
	cal.PendingURL = "https://alice.btc.calendar.opentimestamps.org"
	cp := signOne(t, store, 1)
	before := promtest.ToFloat64(logstore.AnchorCounter(logstore.AnchorKindOTS, cal.URL, logstore.AnchorBadAnswer))

	// Act
	res := runAnchor(t, job)

	// Assert: refused, named, nothing stored, counted.
	o := res.Anchors[cal.URL]
	require.NotNil(t, o)
	assert.Equal(t, logstore.AnchorBadAnswer, o.Outcome)
	assert.Contains(t, o.Detail, "alice.btc.calendar.opentimestamps.org", "the URL the attestation names")
	assert.Contains(t, o.Detail, cal.URL, "the calendar asked")
	assert.Zero(t, o.Submitted)
	rows, err := store.AllAnchors(context.Background(), cp.Size)
	require.NoError(t, err)
	assert.Empty(t, rows, "nothing stored")
	assert.Equal(t, before+1, promtest.ToFloat64(logstore.AnchorCounter(logstore.AnchorKindOTS, cal.URL, logstore.AnchorBadAnswer)))
}

// RP48-2 (starvation): sixteen pending proofs whose commitments the calendar
// has lost (404 for ever) must not keep a newer proof out of the upgrade
// pass for ever. Before, PendingAnchors was ORDER BY received_at LIMIT 16:
// the 16 oldest rows were asked every run and the 17th never.
func TestAnchorJob_LostCommitmentsNeverStarveNewerProofs(t *testing.T) {
	// Arrange: 16 anchored checkpoints whose commitments the calendar loses.
	store, job, cal := newAnchorLabOne(t)
	for i := 0; i < 16; i++ {
		signOne(t, store, i+1)
	}
	runAnchor(t, job) // submits 8 (newest first)
	runAnchor(t, job) // submits the other 8
	cal.Lose()

	// A 17th checkpoint, submitted, and the calendar confirms.
	cp17 := signOne(t, store, 17)
	res := runAnchor(t, job)
	require.Equal(t, 1, res.Anchors[cal.URL].Submitted, "the 17th is submitted")
	cal.Confirm()

	// Act: within three runs the 17th must have been asked and completed,
	// whatever the 16 lost rows do.
	completed := false
	for i := 0; i < 3 && !completed; i++ {
		runAnchor(t, job)
		rows, err := store.AllAnchors(context.Background(), cp17.Size)
		require.NoError(t, err)
		for _, r := range rows {
			if r.State == logstore.AnchorComplete {
				completed = true
			}
		}
	}

	// Assert
	assert.True(t, completed, "the 17th proof is upgraded although 16 older rows 404 for ever")
}

// RP48-3: the served .ots is every row of the note's digest combined —
// pending and complete — so a calendar that answers a wrong complete proof
// cannot replace its own good pending path (ots verify judges each path).
func TestAnchorOTS_ServesPendingBesideComplete(t *testing.T) {
	// Arrange: a pending row from the calendar, then a bogus complete row of
	// the same digest and service (a wrong block height).
	ctx := context.Background()
	store, job, cal := newAnchorLabOne(t)
	cp := signOne(t, store, 1)
	runAnchor(t, job)
	bogus := otstest.New(t)
	bogus.ConfirmAt(123)
	digest := sha256.Sum256([]byte(cp.Note))
	p, err := ots.NewClient("test").Submit(ctx, bogus.URL, digest)
	require.NoError(t, err)
	complete, done, err := ots.NewClient("test").Upgrade(ctx, bogus.URL, p)
	require.NoError(t, err)
	require.True(t, done)
	require.NoError(t, store.AddAnchor(ctx, logstore.Anchor{Size: cp.Size, Kind: logstore.AnchorKindOTS, Service: cal.URL,
		State: logstore.AnchorComplete, Proof: complete, Digest: digest[:]}))

	// Act
	served, err := store.AnchorOTS(ctx, cp.Size)
	require.NoError(t, err)

	// Assert: both paths are in the served file.
	_, atts, err := ots.Describe(served)
	require.NoError(t, err)
	assert.Contains(t, atts, ots.Attestation{BitcoinHeight: 123}, "the complete path")
	assert.Contains(t, atts, ots.Attestation{Calendar: cal.URL}, "the pending path stays beside it")
}

// RP48-1 (b): a per-service view on scrape, bounded by the configured
// services — the largest checkpoint each service has a row for, when its
// newest anchor arrived, and its oldest pending proof's age — so one dead
// calendar of two is visible (TranslogAnchorServiceStale) instead of hiding
// behind the other's max.
func TestAnchorStateCollector_PerServiceView(t *testing.T) {
	// Arrange: two configured calendars, rows for one only (b down).
	skey, _, err := translog.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := translog.NewSigner(skey)
	require.NoError(t, err)
	db := logstoretest.PrivateLog(t)
	store := logstore.New(db, signer)
	a, b := otstest.New(t), otstest.New(t)
	anchors := []translogAnchor{{Kind: logstore.AnchorKindOTS, Service: a.URL}, {Kind: logstore.AnchorKindOTS, Service: b.URL}}
	logstore.SetAnchors(anchorServices(anchors))
	t.Cleanup(func() { logstore.SetAnchors(nil) })
	job := newTranslogAnchorer(store, anchors, nil, zap.NewNop())
	job.JobName = "translog_anchor_p36_svc_test"
	job.upgradeAfter = 0
	b.Down.Store(true)
	cp := signOne(t, store, 1)
	runAnchor(t, job)

	// Act: gather the collector.
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(logstore.NewAnchorStateCollector(db, nil))
	bySvc := gatherBy(t, reg, "vaultaire_translog_anchored_size_by_service")
	at := gatherBy(t, reg, "vaultaire_translog_anchor_timestamp_by_service_seconds")
	oldest := gatherBy(t, reg, "vaultaire_translog_anchor_oldest_pending_seconds")

	// Assert: both configured services are exported; a has the checkpoint
	// and a pending age, b has zeros (never answered); nothing else is.
	require.Len(t, bySvc, 2, "one series per configured service")
	assert.Equal(t, float64(cp.Size), bySvc["ots|"+a.URL])
	assert.Zero(t, bySvc["ots|"+b.URL])
	assert.Positive(t, at["ots|"+a.URL])
	assert.Zero(t, at["ots|"+b.URL])
	assert.GreaterOrEqual(t, oldest["ots|"+a.URL], float64(0))
	assert.Positive(t, oldest["ots|"+a.URL]+1, "a pending row: an age, possibly 0 seconds old")
	assert.Zero(t, oldest["ots|"+b.URL], "no pending row = 0, so the rule stays silent")
}

// gatherBy is a family's samples keyed "kind|service".
func gatherBy(t *testing.T, g prometheus.Gatherer, family string) map[string]float64 {
	t.Helper()
	mfs, err := g.Gather()
	require.NoError(t, err)
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.GetMetric() {
			var kind, service string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "kind":
					kind = l.GetValue()
				case "service":
					service = l.GetValue()
				}
			}
			out[kind+"|"+service] = m.GetGauge().GetValue()
		}
	}
	return out
}

// RP48-2 (the horizon): a pending proof past the horizon is re-submitted to
// the same calendar ONCE — the fresh proof is a 'resubmitted' row beside the
// original (one per calendar and checkpoint, by migration 107's unique
// index), and either row's completion writes the one complete row.
func TestAnchorJob_PendingPastHorizon_ResubmittedOnce(t *testing.T) {
	// Arrange: one pending proof, the horizon at 0.
	ctx := context.Background()
	store, job, cal := newAnchorLabOne(t)
	job.resubmitAfter = 0
	cp := signOne(t, store, 1)
	first := runAnchor(t, job)
	require.Equal(t, 1, first.Anchors[cal.URL].Submitted)

	// Act: two more runs — the first re-submits, the second must not again.
	second := runAnchor(t, job)
	third := runAnchor(t, job)

	// Assert: exactly one resubmitted row, whole-run idempotent.
	assert.Equal(t, 1, second.Anchors[cal.URL].Resubmitted+first.Anchors[cal.URL].Resubmitted)
	assert.Zero(t, third.Anchors[cal.URL].Resubmitted, "re-submitted once, never again")
	rows, err := store.AllAnchors(ctx, cp.Size)
	require.NoError(t, err)
	states := map[string]int{}
	for _, r := range rows {
		states[r.State]++
	}
	assert.Equal(t, map[string]int{logstore.AnchorPending: 1, logstore.AnchorResubmitted: 1}, states)

	// The calendar confirms: the next run completes (the resubmitted proof's
	// commitment is live), and nothing re-submits a completed checkpoint.
	cal.Confirm()
	fourth := runAnchor(t, job)
	assert.Equal(t, 1, fourth.Anchors[cal.URL].Upgraded)
	rows, err = store.AllAnchors(ctx, cp.Size)
	require.NoError(t, err)
	complete := 0
	for _, r := range rows {
		if r.State == logstore.AnchorComplete {
			complete++
		}
	}
	assert.Equal(t, 1, complete)
	fifth := runAnchor(t, job)
	assert.Zero(t, fifth.Anchors[cal.URL].Resubmitted, "a completed checkpoint is never re-submitted")
}
