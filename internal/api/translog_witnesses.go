package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/mod/sumdb/note"

	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// The log collects cosignatures (K4c-2, Stage 1a Prompt 34b; plan 43.6: "at
// least one external witness co-signing every checkpoint";
// docs/TRANSPARENCY.md § The witness). After the translog_checkpoint job
// signs, it submits the newest checkpoint to every witness of
// TRANSLOG_WITNESSES by c2sp.org/tlog-witness add-checkpoint, verifies each
// answer against that witness's pinned cosignature/v1 key, and keeps the
// lines (translog_cosignatures, migration 105); the read side serves them
// on the checkpoint. A witness that is down, refuses or answers garbage
// never fails the signing: it is the run's note, a count and, past 2 h
// while the log grows, TranslogWitnessStale.

// translogWitness is one witness: the operator's name for it (the metrics
// label), its submission prefix and its pinned cosignature key.
type translogWitness struct {
	Name string
	URL  string
	V    note.Verifier
}

// The bounds of the collect step.
const (
	maxTranslogWitnesses = 8                // a checkpoint carries at most 100 signature lines
	witnessSubmitTimeout = 15 * time.Second // one add-checkpoint call
	// witnessRefresh: a witness whose newest cosignature of the newest
	// checkpoint is older than this is asked again (old size = the size),
	// so an idle log's cosignatures stay fresh for a reader's max age.
	witnessRefresh = 6 * time.Hour
	// witnessFutureSkew: a cosignature dated further ahead is not kept.
	witnessFutureSkew = 5 * time.Minute
)

var witnessNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// parseTranslogWitnesses reads TRANSLOG_WITNESSES: witnesses separated by
// ";" or newlines, each "<name> <url> <vkey>" separated by spaces. The name
// is ours (lower case, digits, ".", "_", "-"), the URL the witness's
// submission prefix (https; http only to a loopback host — a lab), the vkey
// its cosignature/v1 verifier key (0x04 Ed25519 or 0x06 ML-DSA-44). Every
// entry must validate, or none is used.
func parseTranslogWitnesses(v string) ([]translogWitness, error) {
	var out []translogWitness
	names, keys := map[string]bool{}, map[string]bool{}
	for _, entry := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == '\n' }) {
		f := strings.Fields(entry)
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("an entry is \"<name> <url> <vkey>\", got %d field(s)", len(f))
		}
		name, raw, vkey := f[0], f[1], f[2]
		if !witnessNameRE.MatchString(name) {
			return nil, fmt.Errorf("witness name %q: lower case letters, digits, '.', '_', '-', at most 63", name)
		}
		if names[name] {
			return nil, fmt.Errorf("witness %q is listed twice", name)
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("witness %s: %q is not a submission prefix URL", name, raw)
		}
		switch u.Scheme {
		case "https":
		case "http":
			if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" && h != "::1" {
				return nil, fmt.Errorf("witness %s: http only to a loopback host (a lab); use https", name)
			}
		default:
			return nil, fmt.Errorf("witness %s: %q is not http(s)", name, raw)
		}
		wv, err := translog.NewCosignatureVerifier(vkey)
		if err != nil {
			return nil, fmt.Errorf("witness %s: %w", name, err)
		}
		id := translog.KeyID(wv.Name(), wv.KeyHash())
		if keys[id] {
			return nil, fmt.Errorf("witness %s: key %s is listed twice", name, id)
		}
		names[name], keys[id] = true, true
		out = append(out, translogWitness{Name: name, URL: strings.TrimSuffix(u.String(), "/"), V: wv})
	}
	if len(out) > maxTranslogWitnesses {
		return nil, fmt.Errorf("%d witnesses: at most %d", len(out), maxTranslogWitnesses)
	}
	return out, nil
}

// translogWitnessesFromEnv is TRANSLOG_WITNESSES at boot: unset = none
// (one Info line, the collect step off); a list that does not validate =
// one Error line and none (like SYNC_WEBDAV_URLS); otherwise one Info line
// naming each witness and its key ID. The keys are public, the URLs are
// not secrets: plain env, no credential (the protocol has no
// authentication beyond the checkpoint's signature).
func translogWitnessesFromEnv(logger *zap.Logger, getenv func(string) string) []translogWitness {
	raw := strings.TrimSpace(getenv("TRANSLOG_WITNESSES"))
	if raw == "" {
		logstore.SetWitnesses(nil)
		logger.Info("translog witnesses: none — checkpoints are signed and not submitted to any witness")
		return nil
	}
	ws, err := parseTranslogWitnesses(raw)
	if err != nil {
		logstore.SetWitnesses(nil)
		logger.Error("TRANSLOG_WITNESSES does not validate — no witness is used (docs/TRANSPARENCY.md § The witness)", zap.Error(err))
		return nil
	}
	names := make([]string, 0, len(ws))
	for _, w := range ws {
		names = append(names, w.Name)
		logger.Info("translog witness", zap.String("witness", w.Name), zap.String("url", w.URL),
			zap.String("key", translog.KeyID(w.V.Name(), w.V.KeyHash())))
	}
	logstore.SetWitnesses(names)
	return ws
}

// cosignCollector submits checkpoints to the witnesses and keeps what they sign.
type cosignCollector struct {
	store     *logstore.Store
	witnesses []translogWitness
	hc        *http.Client
	now       func() time.Time
	logger    *zap.Logger
}

func newCosignCollector(store *logstore.Store, ws []translogWitness, logger *zap.Logger) *cosignCollector {
	if store == nil || len(ws) == 0 {
		return nil
	}
	return &cosignCollector{store: store, witnesses: ws, now: time.Now, logger: logger,
		hc: &http.Client{Timeout: witnessSubmitTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // a witness answers, it does not send the log elsewhere
		}}}
}

// collect submits the newest checkpoint to every witness that is due — it
// has not cosigned it, or its newest cosignature of it is older than
// witnessRefresh — in parallel. It never fails: each witness's outcome is
// in the map (nil = none was due, or nothing is signed).
func (c *cosignCollector) collect(ctx context.Context) map[string]logstore.WitnessOutcome {
	if c == nil {
		return nil
	}
	latest, ok, err := c.store.LatestCheckpoint(ctx)
	if err != nil || !ok {
		if err != nil {
			c.logger.Warn("translog witnesses: the newest checkpoint cannot be read — nothing submitted", zap.Error(err))
		}
		return nil
	}
	var mu sync.Mutex
	out := map[string]logstore.WitnessOutcome{}
	var wg sync.WaitGroup
	for _, w := range c.witnesses {
		wg.Add(1)
		go func(w translogWitness) {
			defer wg.Done()
			o, due := c.submit(ctx, w, latest)
			if !due {
				return
			}
			logstore.CountCosign(w.Name, o.Outcome)
			if o.Outcome != logstore.CosignOK {
				c.logger.Warn("translog witness: no cosignature this run — the checkpoint stays signed", zap.String("witness", w.Name),
					zap.String("outcome", o.Outcome), zap.Int64("size", o.Size), zap.Int64("old", o.Old), zap.String("detail", o.Detail))
			}
			mu.Lock()
			out[w.Name] = o
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	if len(out) == 0 {
		return nil
	}
	return out
}

// submit asks one witness to cosign cp; due = false when it is not due.
func (c *cosignCollector) submit(ctx context.Context, w translogWitness, cp logstore.Stored) (logstore.WitnessOutcome, bool) {
	o := logstore.WitnessOutcome{Size: cp.Size}
	keyID := translog.KeyID(w.V.Name(), w.V.KeyHash())
	held, newest, err := c.store.WitnessState(ctx, keyID)
	if err != nil {
		o.Outcome, o.Detail = logstore.CosignUnreachable, "the log's record of this witness cannot be read: "+err.Error()
		return o, true
	}
	if held >= cp.Size && c.now().Sub(newest) < witnessRefresh {
		return o, false
	}
	o.Old = held
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := c.store.ConsistencyProof(ctx, o.Old, cp.Size)
		if err != nil {
			o.Outcome, o.Detail = logstore.CosignUnreachable, "the consistency proof cannot be built: "+err.Error()
			return o, true
		}
		sctx, cancel := context.WithTimeout(ctx, witnessSubmitTimeout)
		ans, err := translog.SubmitCheckpoint(sctx, c.hc, w.URL, translog.AddCheckpointBody(o.Old, proof, []byte(cp.Note)))
		cancel()
		switch {
		case err != nil:
			o.Outcome, o.Detail = logstore.CosignUnreachable, err.Error()
			return o, true
		case ans.Status == http.StatusOK:
			return c.keep(ctx, w, cp, ans.Lines, o), true
		case ans.Status == http.StatusConflict:
			// The witness holds another size (its own pull pass moved it, or
			// an answer was lost): the proof from the size it names, once.
			if attempt == 0 && ans.HeldSize >= 0 && ans.HeldSize <= cp.Size && ans.HeldSize != o.Old {
				o.Old = ans.HeldSize
				continue
			}
			o.Outcome = logstore.CosignConflict
			o.Detail = fmt.Sprintf("409: the witness holds size %d", ans.HeldSize)
			if ans.HeldSize > cp.Size {
				o.Detail += fmt.Sprintf(" — a larger tree than the log's newest checkpoint (%d): a restored database, or a fork (docs/TRANSPARENCY.md § After a database restore)", cp.Size)
			}
			return o, true
		case ans.Status >= 500:
			o.Outcome, o.Detail = logstore.CosignUnreachable, fmt.Sprintf("HTTP %d", ans.Status)
			return o, true
		default:
			o.Outcome, o.Detail = logstore.CosignRefused, fmt.Sprintf("HTTP %d", ans.Status)
			return o, true
		}
	}
	return o, true // unreachable: the loop returns
}

// keep verifies a 200's lines against the witness's pinned key and stores them.
func (c *cosignCollector) keep(ctx context.Context, w translogWitness, cp logstore.Stored, lines []byte, o logstore.WitnessOutcome) logstore.WitnessOutcome {
	cos, err := translog.VerifyAnswer([]byte(cp.Note), lines, w.V)
	if err != nil {
		o.Outcome, o.Detail = logstore.CosignBadAnswer, err.Error()
		return o
	}
	sort.Slice(cos, func(i, j int) bool { return cos[i].Line < cos[j].Line })
	for _, s := range cos {
		if s.Time.After(c.now().Add(witnessFutureSkew)) {
			o.Outcome, o.Detail = logstore.CosignBadAnswer, "a cosignature dated "+s.Time.UTC().Format(time.RFC3339)+", in the future"
			return o
		}
	}
	for _, s := range cos {
		if err := c.store.AddCosignature(ctx, cp.Size, w.Name, s); err != nil {
			o.Outcome, o.Detail = logstore.CosignUnreachable, err.Error()
			return o
		}
	}
	o.Outcome = logstore.CosignOK
	return o
}

// witnessNote is the run's note for the witnesses that did not cosign.
func witnessNote(ws map[string]logstore.WitnessOutcome) string {
	var bad []string
	for name, o := range ws {
		if o.Outcome != logstore.CosignOK {
			bad = append(bad, fmt.Sprintf("witness %s: %s (%s)", name, o.Outcome, o.Detail))
		}
	}
	sort.Strings(bad)
	return strings.Join(bad, "; ")
}
