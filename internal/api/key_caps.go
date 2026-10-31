package api

// Per-key and per-bucket budgets (WP-KEY-1 PR B, Stage 1a Prompt 54; F-39,
// decided by the owner 2026-10-31; D-RP88-1/2; migration 121).
//
// Storage: a row names the key that wrote it (written_by_key — the
// request's KeyScope.KeyID, an STS token's parent's; the primary pair and a
// background write: none). The key's and the bucket's counters move with
// the tenant's in the one ledger (internal/usage/holders.go); the
// reservation checks min(key, bucket, tenant, pool) and a refusal is 403
// QuotaExceeded with x-stored-cap naming the first cap that failed.
//
// Egress: a key's or a bucket's egress cap per UTC month REFUSES a response
// that does not fit what is left of it (F-39 (a); RP88-6, D-STRIPE-63 as
// restated): the response's size is known before its first body byte (its
// Content-Length at WriteHeader, the admitted span at the edge), so one
// whose size is more than the cap's remaining bytes is 403 QuotaExceeded,
// x-stored-cap, and Retry-After = the next UTC month's start — and one that
// fits is pre-charged in ONE conditional UPDATE at that moment (concurrent
// responses can never together pass the cap), then credited back by what it
// did not send (a client abort). The month is a durable counter
// on the row (egress_used_bytes / egress_month), moved by each response of
// a capped key or bucket, so both slots and a restart see one number.
// key.cap_warning events fire at 80 and 95 %, once each a month
// (egress_warned_pct, claimed by an UPDATE … RETURNING). Pacing stays the
// tenant allowance's alone (D-25): nothing here slows a response.
//
// The primary pair is never capped (F-39 (c)): its scope has a KeyID, its
// caps are NULL and a cap on it is refused (409 primary_never_capped). A
// bucket's caps apply to every key, the primary's included.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/usage"
)

// capWarningSteps are the egress warning steps, in percent of a cap.
var capWarningSteps = []int{80, 95}

type writerKeyCtxKey struct{}

// withWriterKey names the key a write is counted to when it is not the
// request's own credential: an edge commit (the admit's key), a
// CompleteMultipartUpload (the upload's key). "" = none.
func withWriterKey(ctx context.Context, keyID string) context.Context {
	return context.WithValue(ctx, writerKeyCtxKey{}, keyID)
}

// writerKeyOf is the key a write on ctx is counted to: withWriterKey's,
// else the request's credential (KeyScope.KeyID) unless it is the primary
// pair (F-39 (c)), else none.
func writerKeyOf(ctx context.Context) string {
	if v, ok := ctx.Value(writerKeyCtxKey{}).(string); ok {
		return v
	}
	if ks := auth.KeyScopeFromContext(ctx); ks != nil && !ks.Primary {
		return ks.KeyID
	}
	return ""
}

// holderOf is a write's holders on ctx in bucket.
func holderOf(ctx context.Context, bucket string) usage.Holder {
	return usage.Holder{KeyID: writerKeyOf(ctx), Bucket: bucket}
}

// nullUUID is a key id as a nullable uuid argument.
func nullUUID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// setStoredCap names a refusal's cap ("" = the tenant's): x-stored-cap.
func setStoredCap(w http.ResponseWriter, capName string) string {
	if capName == "" {
		capName = usage.CapTenant
	}
	w.Header().Set("x-stored-cap", capName)
	return capName
}

// writeQuotaRefusal answers a reservation's refusal: 403 QuotaExceeded,
// x-stored-cap naming the cap ("" = the tenant's).
func writeQuotaRefusal(w http.ResponseWriter, path, capName string) {
	capName = setStoredCap(w, capName)
	msg := ""
	switch capName {
	case usage.CapKey:
		msg = "This key's storage cap is reached."
	case usage.CapBucket:
		msg = "This bucket's storage cap is reached."
	}
	opts := []ErrorOption{WithSuggestion("Upgrade at " + billingURL())}
	if msg != "" {
		opts = append(opts, WithMessage(msg))
	}
	WriteS3ErrorWithContext(w, ErrQuotaExceeded, path, generateRequestID(), opts...)
}

// --- Egress caps ---

// egressMonth is the UTC month a response counts in, as egress_month holds
// it (its first day).
func egressMonth(now time.Time) string {
	return usage.EgressMonthStart(now).UTC().Format("2006-01-02")
}

// secondsToNextMonth is Retry-After for a spent monthly cap.
func secondsToNextMonth(now time.Time) int {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	return int(math.Ceil(next.Sub(now).Seconds()))
}

// egressCaps is a request's capped holders, read before the body, with what
// is left of each one's month.
type egressCaps struct {
	keyID           string // the capped key ("" = no key cap)
	keyRemaining    int64
	bucket          string
	bucketLimit     int64 // > 0: the bucket is capped
	bucketRemaining int64
}

// capped: some holder of the response has an egress cap.
func (c egressCaps) capped() bool { return c.keyID != "" || c.bucketLimit > 0 }

// remaining is the fewest bytes left on the response's capped holders and
// the cap that leaves them (ok false = no cap).
func (c egressCaps) remaining() (n int64, capName string, ok bool) {
	if c.keyID != "" {
		n, capName, ok = c.keyRemaining, usage.CapKey, true
	}
	if c.bucketLimit > 0 && (!ok || c.bucketRemaining < n) {
		n, capName, ok = c.bucketRemaining, usage.CapBucket, true
	}
	return n, capName, ok
}

// bucketEgressCap reads tenantID's bucket's egress cap and this month's
// bytes (0, 0 = none).
func (s *Server) bucketEgressCap(ctx context.Context, tenantID, bucket string) (limit, used int64, err error) {
	if s.db == nil || bucket == "" {
		return 0, 0, nil
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(egress_limit_bytes, 0),
		       CASE WHEN egress_month = $3::date THEN egress_used_bytes ELSE 0 END
		  FROM buckets WHERE tenant_id = $1 AND name = $2`, tenantID, bucket, egressMonth(time.Now())).Scan(&limit, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	return limit, used, err
}

// egressCapsOf decides a download of tenantID's bucket on ctx: the caps to
// count it against, or the spent cap that refuses it ("key" | "bucket").
// A failed read refuses nothing (the tenant's allowance still applies) and
// is logged.
func (s *Server) egressCapsOf(ctx context.Context, tenantID, bucket string) (egressCaps, string) {
	c := egressCaps{bucket: bucket}
	if ks := auth.KeyScopeFromContext(ctx); ks != nil && !ks.Primary && ks.KeyID != "" && ks.EgressCap > 0 {
		if ks.EgressUsed >= ks.EgressCap {
			return c, usage.CapKey
		}
		c.keyID, c.keyRemaining = ks.KeyID, ks.EgressCap-ks.EgressUsed
	}
	limit, used, err := s.bucketEgressCap(ctx, tenantID, bucket)
	if err != nil {
		s.logger.Warn("egress cap: read the bucket's cap failed — not enforced for this response",
			zap.String("tenant_id", tenantID), zap.Error(err))
		return c, ""
	}
	if limit > 0 {
		if used >= limit {
			return c, usage.CapBucket
		}
		c.bucketLimit, c.bucketRemaining = limit, limit-used
	}
	return c, ""
}

// writeEgressCapRefusal answers a spent egress cap: 403 QuotaExceeded,
// x-stored-cap, Retry-After at the next UTC month.
func writeEgressCapRefusal(w http.ResponseWriter, r *http.Request, capName string) {
	markClientRefusal(r.Context())
	egressCapRefusals.WithLabelValues(capName).Inc()
	w.Header().Set("x-stored-cap", capName)
	w.Header().Set("Retry-After", strconv.Itoa(secondsToNextMonth(time.Now())))
	WriteS3ErrorWithContext(w, ErrQuotaExceeded, r.URL.Path, generateRequestID(),
		WithMessage("This "+capName+"'s monthly download cap is spent."),
		WithSuggestion("It resets at the start of the next UTC month, or raise the cap in the dashboard."))
}

// countEgress adds n bytes served to the capped holders' months and fires
// the warnings they cross. Detached: the response is over.
func (s *Server) countEgress(ctx context.Context, tenantID string, c egressCaps, n int64) {
	if s.db == nil || n <= 0 || (c.keyID == "" && c.bucketLimit <= 0) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	month := egressMonth(time.Now())
	if c.keyID != "" {
		s.countEgressOn(ctx, tenantID, usage.CapKey, c.keyID, "", month, n)
	}
	if c.bucketLimit > 0 {
		s.countEgressOn(ctx, tenantID, usage.CapBucket, "", c.bucket, month, n)
	}
}

// The egress counter's statements per holder: the month's bytes (a count
// after the response), the conditional pre-charge (RP88-6: the planned
// bytes, only while they fit), the credit of what was planned and not sent,
// and the once-only claim of a warning step, which answers the step it
// displaced (RP88-7) so two responses never both emit the 80 %.
const (
	keyEgressCountSQL = `
		UPDATE api_keys SET
		       egress_used_bytes = CASE WHEN egress_month = $3::date THEN egress_used_bytes + $4 ELSE $4 END,
		       egress_warned_pct = CASE WHEN egress_month = $3::date THEN egress_warned_pct ELSE 0 END,
		       egress_month = $3::date
		 WHERE id = $1::uuid AND tenant_id = $2
		RETURNING egress_used_bytes, COALESCE(egress_limit_bytes, 0), egress_warned_pct`
	bucketEgressCountSQL = `
		UPDATE buckets SET
		       egress_used_bytes = CASE WHEN egress_month = $3::date THEN egress_used_bytes + $4 ELSE $4 END,
		       egress_warned_pct = CASE WHEN egress_month = $3::date THEN egress_warned_pct ELSE 0 END,
		       egress_month = $3::date
		 WHERE name = $1 AND tenant_id = $2
		RETURNING egress_used_bytes, COALESCE(egress_limit_bytes, 0), egress_warned_pct`
	keyEgressChargeSQL = `
		UPDATE api_keys SET
		       egress_used_bytes = CASE WHEN egress_month = $3::date THEN egress_used_bytes + $4 ELSE $4 END,
		       egress_warned_pct = CASE WHEN egress_month = $3::date THEN egress_warned_pct ELSE 0 END,
		       egress_month = $3::date
		 WHERE id = $1::uuid AND tenant_id = $2
		   AND (egress_limit_bytes IS NULL
		        OR (CASE WHEN egress_month = $3::date THEN egress_used_bytes ELSE 0 END) + $4 <= egress_limit_bytes)
		RETURNING egress_used_bytes, COALESCE(egress_limit_bytes, 0), egress_warned_pct`
	bucketEgressChargeSQL = `
		UPDATE buckets SET
		       egress_used_bytes = CASE WHEN egress_month = $3::date THEN egress_used_bytes + $4 ELSE $4 END,
		       egress_warned_pct = CASE WHEN egress_month = $3::date THEN egress_warned_pct ELSE 0 END,
		       egress_month = $3::date
		 WHERE name = $1 AND tenant_id = $2
		   AND (egress_limit_bytes IS NULL
		        OR (CASE WHEN egress_month = $3::date THEN egress_used_bytes ELSE 0 END) + $4 <= egress_limit_bytes)
		RETURNING egress_used_bytes, COALESCE(egress_limit_bytes, 0), egress_warned_pct`
	keyEgressCreditSQL = `UPDATE api_keys SET egress_used_bytes = GREATEST(0, egress_used_bytes - $4)
		 WHERE id = $1::uuid AND tenant_id = $2 AND egress_month = $3::date`
	bucketEgressCreditSQL = `UPDATE buckets SET egress_used_bytes = GREATEST(0, egress_used_bytes - $4)
		 WHERE name = $1 AND tenant_id = $2 AND egress_month = $3::date`
	keyEgressWarnSQL = `
		UPDATE api_keys a SET egress_warned_pct = $4
		  FROM (SELECT id, egress_warned_pct AS prev FROM api_keys
		         WHERE id = $1::uuid AND tenant_id = $2 AND egress_month = $3::date FOR UPDATE) o
		 WHERE a.id = o.id AND o.prev < $4
		RETURNING o.prev`
	bucketEgressWarnSQL = `
		UPDATE buckets b SET egress_warned_pct = $4
		  FROM (SELECT tenant_id, name, egress_warned_pct AS prev FROM buckets
		         WHERE name = $1 AND tenant_id = $2 AND egress_month = $3::date FOR UPDATE) o
		 WHERE b.tenant_id = o.tenant_id AND b.name = o.name AND o.prev < $4
		RETURNING o.prev`
)

// egressHolder is one capped holder's statements and id.
func egressHolder(kind string, c egressCaps) (count, charge, credit, warn, id string) {
	if kind == usage.CapBucket {
		return bucketEgressCountSQL, bucketEgressChargeSQL, bucketEgressCreditSQL, bucketEgressWarnSQL, c.bucket
	}
	return keyEgressCountSQL, keyEgressChargeSQL, keyEgressCreditSQL, keyEgressWarnSQL, c.keyID
}

// kinds is the response's capped holders, key first (the refusal's order).
func (c egressCaps) kinds() []string {
	var out []string
	if c.keyID != "" {
		out = append(out, usage.CapKey)
	}
	if c.bucketLimit > 0 {
		out = append(out, usage.CapBucket)
	}
	return out
}

func (s *Server) countEgressOn(ctx context.Context, tenantID, kind, keyID, bucket, month string, n int64) {
	c := egressCaps{keyID: keyID, bucket: bucket}
	count, _, _, _, id := egressHolder(kind, c)
	var used, limit int64
	var warned int
	if err := s.db.QueryRowContext(ctx, count, id, tenantID, month, n).Scan(&used, &limit, &warned); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logger.Warn("egress cap: count the month failed", zap.String("tenant_id", tenantID), zap.String("cap", kind), zap.Error(err))
		}
		return
	}
	s.warnEgress(ctx, tenantID, kind, c, month, used, limit)
}

// chargeEgress pre-charges n planned bytes to every capped holder of c in
// ONE transaction, each by a conditional UPDATE (RP88-6): all fit or none
// is charged. It answers the cap that refused ("" = charged), and charged
// false with no refusal when the ledger could not be read (logged: the
// response is then counted after it ends, as before).
func (s *Server) chargeEgress(ctx context.Context, tenantID string, c egressCaps, n int64) (refused string, charged bool) {
	if s.db == nil || !c.capped() {
		return "", false
	}
	month := egressMonth(time.Now())
	type hit struct {
		kind        string
		used, limit int64
	}
	var hits []hit
	err := func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		for _, kind := range c.kinds() {
			_, charge, _, _, id := egressHolder(kind, c)
			var h hit
			var warned int
			h.kind = kind
			err := tx.QueryRowContext(ctx, charge, id, tenantID, month, n).Scan(&h.used, &h.limit, &warned)
			if errors.Is(err, sql.ErrNoRows) {
				refused = kind
				return nil // rolled back: nothing charged anywhere
			}
			if err != nil {
				return err
			}
			hits = append(hits, h)
		}
		return tx.Commit()
	}()
	if err != nil {
		s.logger.Warn("egress cap: the pre-charge failed — the response is counted after it ends",
			zap.String("tenant_id", tenantID), zap.Error(err))
		return "", false
	}
	if refused != "" {
		return refused, false
	}
	for _, h := range hits {
		s.warnEgress(ctx, tenantID, h.kind, c, month, h.used, h.limit)
	}
	return "", true
}

// creditEgress gives back n pre-charged bytes a response did not send.
// Detached: the response is over.
func (s *Server) creditEgress(ctx context.Context, tenantID string, c egressCaps, n int64) {
	if s.db == nil || n <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	month := egressMonth(time.Now())
	for _, kind := range c.kinds() {
		_, _, credit, _, id := egressHolder(kind, c)
		if _, err := s.db.ExecContext(ctx, credit, id, tenantID, month, n); err != nil {
			s.logger.Warn("egress cap: the credit of unsent bytes failed", zap.String("tenant_id", tenantID), zap.String("cap", kind), zap.Error(err))
		}
	}
}

// warnEgress fires the warning steps a holder's month crossed, each once a
// month: the claim answers the step it displaced, and only the steps above
// it are emitted (RP88-7).
func (s *Server) warnEgress(ctx context.Context, tenantID, kind string, c egressCaps, month string, used, limit int64) {
	if limit <= 0 {
		return
	}
	step := 0
	for _, pct := range capWarningSteps {
		if egressCrossed(used, limit, pct) {
			step = pct
		}
	}
	if step == 0 {
		return
	}
	_, _, _, warn, id := egressHolder(kind, c)
	var prev int
	if err := s.db.QueryRowContext(ctx, warn, id, tenantID, month, step).Scan(&prev); err != nil {
		return // already claimed at this step or above (no row), or the claim failed
	}
	for _, pct := range capWarningSteps {
		if pct <= prev || pct > step {
			continue
		}
		data := map[string]any{"cap": kind, "threshold_pct": pct, "used_bytes": used, "limit_bytes": limit,
			"month": month, "resets_at": usage.EgressMonthStart(time.Now()).AddDate(0, 1, 0).UTC().Format(time.RFC3339)}
		if kind == usage.CapKey {
			data["key_id"] = c.keyID
		} else {
			data["bucket"] = c.bucket
		}
		emitEvent(ctx, s.db, s.logger, "key.cap_warning", tenantID, data)
		egressCapWarnings.WithLabelValues(kind, strconv.Itoa(pct)).Inc()
	}
}

// egressCapWriter refuses, at the response's WriteHeader, a 200/206 whose
// Content-Length is more than its capped holders have left this month, and
// pre-charges one that fits (RP88-6). It sits OUTSIDE the handler (as the
// counting writer does), so no path inside the handler sends around it. A
// refusal clears the response's header map, answers 403 QuotaExceeded +
// x-stored-cap + Retry-After, and drops the body; the status the outer
// counting writer keeps is then the 403, so nothing is counted.
type egressCapWriter struct {
	http.ResponseWriter
	s        *Server
	r        *http.Request
	tenantID string
	caps     egressCaps
	// refuse writes the surface's own refusal (S3 XML, /cdn JSON).
	refuse func(w http.ResponseWriter, capName string)

	decided, refused, charged bool
	status                    int
	planned, sent             int64
}

func (w *egressCapWriter) WriteHeader(code int) {
	if w.decided {
		if !w.refused {
			w.ResponseWriter.WriteHeader(code)
		}
		return
	}
	w.decided, w.status = true, code
	if code == http.StatusOK || code == http.StatusPartialContent {
		if n, err := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64); err == nil && n >= 0 {
			capName := ""
			if left, name, ok := w.caps.remaining(); ok && n > left {
				capName = name
			} else {
				capName, w.charged = w.s.chargeEgress(w.r.Context(), w.tenantID, w.caps, n)
			}
			if capName != "" {
				w.refused = true
				h := w.Header()
				for k := range h {
					delete(h, k)
				}
				w.refuse(w.ResponseWriter, capName)
				return
			}
			w.planned = n
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *egressCapWriter) Write(p []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.refused {
		return len(p), nil // the refused response's body is dropped
	}
	n, err := w.ResponseWriter.Write(p)
	w.sent += int64(n)
	return n, err
}

// Flush passes through (a streaming handler keeps flushing).
func (w *egressCapWriter) Flush() {
	if w.refused {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.decided {
			w.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

// Unwrap lets http.ResponseController and noteS3Error reach the writer.
func (w *egressCapWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish settles the response once the handler has returned: a pre-charged
// response gives back what it planned and did not send (a client abort; it
// never sends more than its Content-Length); one that went out with no
// Content-Length is counted now (the count before RP88-6).
func (w *egressCapWriter) finish() {
	switch {
	case w.refused:
	case w.charged:
		w.s.creditEgress(w.r.Context(), w.tenantID, w.caps, w.planned-w.sent)
	case w.status == 0 || w.status == http.StatusOK || w.status == http.StatusPartialContent:
		w.s.countEgress(w.r.Context(), w.tenantID, w.caps, w.sent)
	}
}

// newEgressCapWriter wraps w for a download of the capped holders c; nil
// when nothing is capped (no wrapper, no cost).
func (s *Server) newEgressCapWriter(w http.ResponseWriter, r *http.Request, tenantID string, c egressCaps,
	refuse func(http.ResponseWriter, string)) *egressCapWriter {
	if !c.capped() {
		return nil
	}
	return &egressCapWriter{ResponseWriter: w, s: s, r: r, tenantID: tenantID, caps: c, refuse: refuse}
}

var (
	egressCapRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_egress_cap_refusals_total",
		Help: "Downloads refused because a key's or a bucket's own monthly egress cap was spent (WP-KEY-1, F-39 (a)), by cap.",
	}, []string{"cap"})
	egressCapWarnings = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_egress_cap_warnings_total",
		Help: "key.cap_warning events fired (80 and 95 % of a key's or a bucket's monthly egress cap), by cap and step.",
	}, []string{"cap", "step"})
)

func init() {
	for _, c := range []string{usage.CapKey, usage.CapBucket} {
		egressCapRefusals.WithLabelValues(c)
		for _, pct := range capWarningSteps {
			egressCapWarnings.WithLabelValues(c, strconv.Itoa(pct))
		}
	}
}

// --- The cap routes: PUT /api/v1/manage/keys/{id}/caps and
// /api/v1/manage/buckets/{name}/caps ---

// capsRequest is a cap set: both members always (null or absent = none).
type capsRequest struct {
	StorageLimit *int64 `json:"storage_limit_bytes"`
	EgressLimit  *int64 `json:"egress_limit_bytes"`
}

func (s *Server) readCaps(w http.ResponseWriter, r *http.Request) (usage.Caps, bool) {
	var req capsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", "request body must be valid JSON", "")
		return usage.Caps{}, false
	}
	// A management key narrows, never widens (RP88-8): a person may do
	// anything.
	return usage.Caps{StorageLimit: req.StorageLimit, EgressLimit: req.EgressLimit,
		NarrowOnly: managementKeyOf(r.Context()) != nil}, true
}

// tenantEgressAllowance is the tenant's monthly allowance (0 = unknown),
// for the "above the tenant's" note.
func (s *Server) tenantEgressAllowance(ctx context.Context, tenantID string) int64 {
	qm, ok := s.quotaManager.(*usage.QuotaManager)
	if !ok || qm == nil {
		return 0
	}
	a, err := qm.EgressAllowance(ctx, tenantID)
	if err != nil {
		return 0
	}
	return a.Bytes
}

func capsJSON(object, id string, c usage.Caps, res usage.CapsResult) map[string]any {
	return map[string]any{"object": object, "id": id, "storage_limit_bytes": c.StorageLimit, "egress_limit_bytes": c.EgressLimit,
		"storage_used_bytes": res.StorageUsed, "notes": res.Notes}
}

func (s *Server) writeCapsError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, usage.ErrNegativeCap):
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_cap", err.Error(), "storage_limit_bytes")
	case errors.Is(err, usage.ErrCapWidening):
		writeManagementError(w, ErrTypeConflict, "cap_widening_needs_person",
			"a management key may set a cap where none is or lower one; raising or clearing a cap needs a person", "storage_limit_bytes")
	case errors.Is(err, usage.ErrPrimaryNeverCapped):
		writeManagementError(w, ErrTypeConflict, "primary_never_capped", "the primary key pair is never capped; cap a scoped key instead", "id")
	case errors.Is(err, usage.ErrNoSuchHolder):
		writeManagementError(w, ErrTypeNotFound, what+"_not_found", "no live "+what+" of this account has that name", "id")
	default:
		s.logger.Error("set caps", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to set the caps", "")
	}
}

// handleMgmtSetKeyCaps is PUT /api/v1/manage/keys/{id}/caps.
func (s *Server) handleMgmtSetKeyCaps(w http.ResponseWriter, r *http.Request) {
	tenantID := activeTenant(r)
	qm, ok := s.quotaManager.(*usage.QuotaManager)
	if tenantID == "" || !ok || qm == nil {
		writeManagementError(w, ErrTypeAPI, "no_database", "caps need the quota ledger", "")
		return
	}
	c, ok := s.readCaps(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	res, err := qm.SetKeyCaps(r.Context(), tenantID, id, c, s.tenantEgressAllowance(r.Context(), tenantID))
	if err != nil {
		s.writeCapsError(w, err, "key")
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, Action: "key.caps_set", Resource: "key:" + id,
		Metadata: map[string]any{"storage_limit_bytes": c.StorageLimit, "egress_limit_bytes": c.EgressLimit, "notes": res.Notes}})
	out := capsJSON("key_caps", id, c, res)
	out["request_id"] = getRequestID(w)
	writeJSON(w, http.StatusOK, out)
}

// handleMgmtSetBucketCaps is PUT /api/v1/manage/buckets/{name}/caps.
func (s *Server) handleMgmtSetBucketCaps(w http.ResponseWriter, r *http.Request) {
	tenantID := activeTenant(r)
	name := chi.URLParam(r, "name")
	if mgmtSystemBucketNotFound(w, name) {
		return
	}
	qm, ok := s.quotaManager.(*usage.QuotaManager)
	if tenantID == "" || !ok || qm == nil {
		writeManagementError(w, ErrTypeAPI, "no_database", "caps need the quota ledger", "")
		return
	}
	c, ok := s.readCaps(w, r)
	if !ok {
		return
	}
	res, err := qm.SetBucketCaps(r.Context(), tenantID, name, c, s.tenantEgressAllowance(r.Context(), tenantID))
	if err != nil {
		s.writeCapsError(w, err, "bucket")
		return
	}
	audit.Record(r.Context(), s.db, audit.Entry{TenantID: tenantID, Action: "bucket.caps_set", Resource: "bucket:" + name,
		Metadata: map[string]any{"storage_limit_bytes": c.StorageLimit, "egress_limit_bytes": c.EgressLimit, "notes": res.Notes}})
	out := capsJSON("bucket_caps", name, c, res)
	out["request_id"] = getRequestID(w)
	writeJSON(w, http.StatusOK, out)
}
