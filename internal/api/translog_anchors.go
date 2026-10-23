package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/translog/logstore"
	"github.com/FairForge/vaultaire/internal/translog/ots"
)

// The log anchors its checkpoints (K4c-3a, Stage 1a Prompt 35; plan 43.6 /
// F-16: "signed statements + OpenTimestamps + a Rekor mirror"; Trust's
// first ten, item 3; docs/TRANSPARENCY.md § Anchors). The translog_anchor
// job — its own job, never inside translog_checkpoint (2 min ceiling) —
// submits every signed checkpoint not yet anchored to each calendar of
// TRANSLOG_ANCHORS (SHA-256 of the note exactly as the log signed it), then
// asks each calendar to complete its pending proofs. A complete proof is a
// NEW row (migration 106 is append-only). A calendar that is down, refuses
// or answers garbage never fails the job: it is the run's result and note,
// vaultaire_translog_anchor_total and, past 2 h, TranslogAnchorStale.

// translogAnchor is one service: its kind and URL.
type translogAnchor struct {
	Kind    string
	Service string
}

// The bounds of the anchor job.
const (
	translogAnchorJobName = "translog_anchor"
	maxTranslogAnchors    = 8
	minOTSCalendars       = 2
	anchorCallTimeout     = 15 * time.Second
	// Per run and service: at most this many checkpoints submitted (newest
	// first) and pending proofs asked to complete (oldest first).
	anchorSubmitsPerRun  = 8
	anchorUpgradesPerRun = 16
	// A pending proof is first asked to complete this long after it was
	// received: a calendar puts its aggregate in a Bitcoin block every few
	// blocks, and every ask before that is a 404.
	anchorUpgradeAfter = 30 * time.Minute
)

// errRekorNotBuilt: a rekor line in TRANSLOG_ANCHORS.
var errRekorNotBuilt = errors.New("rekor anchors are not built (D-P35-2): Rekor v2 takes only hashedrekord entries signed with a prehash algorithm (Ed25519ph, ECDSA, RSA), and the log's key signs checkpoints only — a dedicated anchor key is an owner ceremony")

// parseTranslogAnchors reads TRANSLOG_ANCHORS: services separated by ";" or
// newlines, each "<kind> <url>" — "ots <calendar URL>", at least two
// calendars (a single calendar is a single point of trust). https only, no
// credentials, query or fragment; at most 8. Every entry must validate, or
// none is used.
func parseTranslogAnchors(v string) ([]translogAnchor, error) {
	var out []translogAnchor
	seen := map[string]bool{}
	for _, entry := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == '\n' }) {
		f := strings.Fields(entry)
		if len(f) == 0 {
			continue
		}
		if len(f) != 2 {
			return nil, fmt.Errorf("an entry is \"<kind> <url>\", got %d field(s)", len(f))
		}
		kind, raw := f[0], f[1]
		switch kind {
		case logstore.AnchorKindOTS:
		case logstore.AnchorKindRekor:
			return nil, errRekorNotBuilt
		default:
			return nil, fmt.Errorf("anchor kind %q: only ots", kind)
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%s %q: an https URL with no credentials, query or fragment", kind, raw)
		}
		service := strings.TrimSuffix(u.String(), "/")
		if seen[kind+" "+service] {
			return nil, fmt.Errorf("%s %s is listed twice", kind, service)
		}
		seen[kind+" "+service] = true
		out = append(out, translogAnchor{Kind: kind, Service: service})
	}
	if len(out) > maxTranslogAnchors {
		return nil, fmt.Errorf("%d anchoring services: at most %d", len(out), maxTranslogAnchors)
	}
	if n := len(out); n > 0 && n < minOTSCalendars {
		return nil, fmt.Errorf("%d OpenTimestamps calendar: at least %d (one calendar is one point of trust)", n, minOTSCalendars)
	}
	return out, nil
}

// translogAnchorsFromEnv is TRANSLOG_ANCHORS at boot: unset = none (one
// Info line, the job a no-op); a list that does not validate = one Error
// line and none; otherwise one Info line per service. The URLs are public:
// plain env, no credential.
func translogAnchorsFromEnv(logger *zap.Logger, getenv func(string) string) []translogAnchor {
	raw := strings.TrimSpace(getenv("TRANSLOG_ANCHORS"))
	if raw == "" {
		logstore.SetAnchors(nil)
		logger.Info("translog anchors: none — signed checkpoints are not anchored in OpenTimestamps")
		return nil
	}
	as, err := parseTranslogAnchors(raw)
	if err != nil {
		logstore.SetAnchors(nil)
		logger.Error("TRANSLOG_ANCHORS does not validate — nothing is anchored (docs/TRANSPARENCY.md § Anchors)", zap.Error(err))
		return nil
	}
	for _, a := range as {
		logger.Info("translog anchor", zap.String("kind", a.Kind), zap.String("service", a.Service))
	}
	logstore.SetAnchors(anchorServices(as))
	return as
}

func anchorServices(as []translogAnchor) []logstore.AnchorService {
	out := make([]logstore.AnchorService, 0, len(as))
	for _, a := range as {
		out = append(out, logstore.AnchorService{Kind: a.Kind, Service: a.Service})
	}
	return out
}

// translogAnchorer is the translog_anchor job.
type translogAnchorer struct {
	store   *logstore.Store
	anchors []translogAnchor
	client  *ots.Client
	logger  *zap.Logger
	now     func() time.Time
	// upgradeAfter: anchorUpgradeAfter (a test sets 0).
	upgradeAfter time.Duration
	JobName      string
}

func newTranslogAnchorer(store *logstore.Store, anchors []translogAnchor, logger *zap.Logger) *translogAnchorer {
	if store == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &translogAnchorer{store: store, anchors: anchors, client: ots.NewClient("vaultaire-translog (+https://stored.cloud)"),
		logger: logger, now: time.Now, upgradeAfter: anchorUpgradeAfter, JobName: translogAnchorJobName}
}

// anchorResult is the run's job_runs.result; Anchors is absent when no
// service is configured.
type anchorResult struct {
	Anchors map[string]*anchorOutcome `json:"anchors,omitempty"`
	Note    string                    `json:"note,omitempty"`
}

// anchorOutcome is one service's run.
type anchorOutcome struct {
	Kind      string `json:"kind"`
	Submitted int    `json:"submitted"`
	Upgraded  int    `json:"upgraded"`
	// Pending: proofs asked to complete that the calendar has not put in a
	// Bitcoin block yet.
	Pending int `json:"pending"`
	// Outcome is the run's worst: ok, or the first failure.
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

func (a *translogAnchorer) spec() jobSpec {
	return jobSpec{Name: a.JobName, Every: 10 * time.Minute, BootDelay: 3 * time.Minute, MaxRunTime: 5 * time.Minute,
		Run: func(ctx context.Context) (jobReport, error) {
			if len(a.anchors) == 0 {
				return jobReport{Result: anchorResult{Note: "no anchors configured (TRANSLOG_ANCHORS unset)"},
					Note: "no anchors configured (TRANSLOG_ANCHORS unset)"}, nil
			}
			res := anchorResult{Anchors: map[string]*anchorOutcome{}}
			var mu sync.Mutex
			var wg sync.WaitGroup
			var dbErr error
			for _, an := range a.anchors {
				wg.Add(1)
				go func(an translogAnchor) {
					defer wg.Done()
					o, err := a.service(ctx, an)
					mu.Lock()
					defer mu.Unlock()
					res.Anchors[an.Service] = o
					if err != nil && dbErr == nil {
						dbErr = err
					}
				}(an)
			}
			wg.Wait()
			rep := jobReport{Result: res, Note: anchorNote(res.Anchors)}
			for _, o := range res.Anchors {
				rep.Rows += int64(o.Submitted + o.Upgraded)
			}
			return rep, dbErr
		}}
}

// service runs one service: submit, then upgrade. A failed call is the
// outcome (and stops that service's calls this run); only a database error
// is returned.
func (a *translogAnchorer) service(ctx context.Context, an translogAnchor) (*anchorOutcome, error) {
	o := &anchorOutcome{Kind: an.Kind, Outcome: logstore.AnchorOK}
	fail := func(err error) {
		o.Outcome, o.Detail = anchorOutcomeOf(err), err.Error()
		logstore.CountAnchor(an.Kind, an.Service, o.Outcome)
		a.logger.Warn("translog anchor: a call failed — the job goes on, it is asked again next run", zap.String("kind", an.Kind),
			zap.String("service", an.Service), zap.String("outcome", o.Outcome), zap.Error(err))
	}
	todo, err := a.store.Unanchored(ctx, an.Kind, an.Service, anchorSubmitsPerRun)
	if err != nil {
		o.Outcome, o.Detail = "error", err.Error()
		return o, err
	}
	for _, cp := range todo {
		digest := logstore.NoteDigest(cp.Note)
		cctx, cancel := context.WithTimeout(ctx, anchorCallTimeout)
		proof, err := a.client.Submit(cctx, an.Service, digest)
		cancel()
		if err != nil {
			fail(err)
			return o, nil
		}
		if err := a.store.AddAnchor(ctx, logstore.Anchor{Size: cp.Size, Kind: an.Kind, Service: an.Service,
			State: logstore.AnchorPending, Proof: proof, Digest: digest[:]}); err != nil {
			o.Outcome, o.Detail = "error", err.Error()
			return o, err
		}
		o.Submitted++
		logstore.CountAnchor(an.Kind, an.Service, logstore.AnchorOK)
	}
	pending, err := a.store.PendingAnchors(ctx, an.Kind, an.Service, a.now().Add(-a.upgradeAfter), anchorUpgradesPerRun)
	if err != nil {
		o.Outcome, o.Detail = "error", err.Error()
		return o, err
	}
	for _, p := range pending {
		cctx, cancel := context.WithTimeout(ctx, anchorCallTimeout)
		proof, complete, err := a.client.Upgrade(cctx, an.Service, p.Proof)
		cancel()
		switch {
		case errors.Is(err, ots.ErrPending), err == nil && !complete:
			o.Pending++
			continue
		case err != nil:
			fail(err)
			return o, nil
		}
		if err := a.store.AddAnchor(ctx, logstore.Anchor{Size: p.Size, Kind: an.Kind, Service: an.Service,
			State: logstore.AnchorComplete, Proof: proof, Digest: p.Digest}); err != nil {
			o.Outcome, o.Detail = "error", err.Error()
			return o, err
		}
		o.Upgraded++
		logstore.CountAnchor(an.Kind, an.Service, logstore.AnchorUpgraded)
	}
	return o, nil
}

func anchorOutcomeOf(err error) string {
	switch {
	case errors.Is(err, ots.ErrUnreachable), errors.Is(err, context.DeadlineExceeded):
		return logstore.AnchorUnreachable
	case errors.Is(err, ots.ErrRefused):
		return logstore.AnchorRefused
	default:
		return logstore.AnchorBadAnswer
	}
}

// anchorNote is the run's note: every service whose run failed.
func anchorNote(m map[string]*anchorOutcome) string {
	var parts []string
	for s, o := range m {
		if o.Outcome != logstore.AnchorOK {
			parts = append(parts, fmt.Sprintf("anchor %s %s: %s (%s)", o.Kind, s, o.Outcome, o.Detail))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
