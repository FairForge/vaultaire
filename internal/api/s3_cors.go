package api

// CORS on the S3 API (docs/STATUS.md queue item 2, 2026-10-06).
//
// A browser never signs its preflight, and a web application cannot read a
// response that carries no Access-Control-* headers. Until this file
// `?cors` answered 501 and an OPTIONS to the S3 endpoint fell into the SigV4
// gate (403, no CORS headers), so no browser could use the S3 API directly —
// the only CORS was the /cdn path's `cors_origins`, which stays what it was.
//
// The shape is AWS's. PutBucketCors stores the rules on the bucket
// (`buckets.cors_rules`, migration 078; NULL = none, the default for every
// bucket), GetBucketCors returns them, DeleteBucketCors drops them.
//
// Bucket names are per tenant (the PK is tenant_id + name), so who may see
// which rule is decided in three places:
//
//   - The OPTIONS preflight (`handleS3Preflight`, registered for OPTIONS on
//     the S3 catch-all pattern) carries no credential, and a path-style URL
//     names no tenant: it is answered from the UNION of every same-named
//     bucket's rules (`corsRuleForRequest`). It only lets the browser SEND
//     the real, signed request, which is then authorised exactly as before.
//     What it reveals — "some bucket of that name allows this origin" — is
//     what AWS reveals too; "no bucket" and "no rule" are the one 403.
//   - A response written once the request is authenticated carries the
//     headers of the CALLER'S OWN bucket only (`applyS3CORSForTenant`, called
//     by `handleS3Request` after the key's gates and before the operation, so
//     a success and an operation error both carry them). A bucket without a
//     configuration gets no CORS header, whatever another tenant configured
//     under the same name. (Until 2026-10-06 the by-name lookup ran here too:
//     tenant A's rules — Allow-Credentials, Expose-Headers — were applied to
//     tenant B's signed responses.)
//   - A response written before a tenant is known (a bad signature, an
//     unknown, expired or IP-refused key) may carry Access-Control-Allow-Origin
//     alone when some same-named bucket's rule allows origin + method
//     (`applyS3CORSPreAuth`), so a page can read the error XML — never
//     credentials, exposed headers, methods or max-age.
//
// Every S3 response to a request with an Origin carries `Vary: Origin`,
// whether a rule matched or not (a shared cache must not serve one origin's
// answer to another). An OPTIONS under an application prefix (`/api`,
// `/auth`, `/dashboard`, `/admin`, `/webhook`) is not an S3 preflight: 404.
//
// Matching, like AWS: origins are compared case-sensitively (scheme, host,
// port) and may hold ONE `*`; methods are one of GET/PUT/HEAD/POST/DELETE;
// a preflight's Access-Control-Request-Headers must each match an
// AllowedHeader (case-insensitive, one `*` allowed); an actual request does
// not check headers. The matched rule's AllowedMethods, ExposeHeaders and
// MaxAgeSeconds are returned; a matched `*` origin is returned as `*` (no
// credentials), anything else echoes the request's Origin with
// Access-Control-Allow-Credentials: true.

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/FairForge/vaultaire/internal/tenant"
	"go.uber.org/zap"
)

const (
	maxCORSBodyBytes = 64 << 10
	maxCORSRules     = 100
)

var corsAllowedMethods = map[string]bool{"GET": true, "PUT": true, "HEAD": true, "POST": true, "DELETE": true}

// CORSConfiguration is the S3 XML document of PutBucketCors / GetBucketCors.
type CORSConfiguration struct {
	XMLName xml.Name   `xml:"CORSConfiguration"`
	Xmlns   string     `xml:"xmlns,attr,omitempty"`
	Rules   []CORSRule `xml:"CORSRule"`
}

// CORSRule is one rule, in the XML shape on the wire and the JSON shape in
// `buckets.cors_rules`. MaxAgeSeconds is a pointer because AWS distinguishes
// "not set" (no Access-Control-Max-Age) from 0.
type CORSRule struct {
	ID             string   `xml:"ID,omitempty" json:"id,omitempty"`
	AllowedOrigins []string `xml:"AllowedOrigin" json:"allowed_origins"`
	AllowedMethods []string `xml:"AllowedMethod" json:"allowed_methods"`
	AllowedHeaders []string `xml:"AllowedHeader,omitempty" json:"allowed_headers,omitempty"`
	ExposeHeaders  []string `xml:"ExposeHeader,omitempty" json:"expose_headers,omitempty"`
	MaxAgeSeconds  *int     `xml:"MaxAgeSeconds,omitempty" json:"max_age_seconds,omitempty"`
}

// corsValidationError is a refusal of a PutBucketCors body: the S3 error
// code and the message AWS would give.
type corsValidationError struct {
	code, message string
}

func (e *corsValidationError) Error() string { return e.code + ": " + e.message }

// validateCORSRules checks the rules the way AWS does and normalises the
// methods to upper case. It never touches the stored configuration.
func validateCORSRules(rules []CORSRule) error {
	if len(rules) == 0 {
		return &corsValidationError{ErrMalformedXML, "A CORSConfiguration must have at least one CORSRule."}
	}
	if len(rules) > maxCORSRules {
		return &corsValidationError{ErrInvalidRequest, fmt.Sprintf("A CORS configuration can have at most %d rules.", maxCORSRules)}
	}
	for i := range rules {
		r := &rules[i]
		if len(r.AllowedOrigins) == 0 || len(r.AllowedMethods) == 0 {
			return &corsValidationError{ErrMalformedXML, "Each CORSRule must have at least one AllowedOrigin and one AllowedMethod."}
		}
		for _, o := range r.AllowedOrigins {
			if o == "" {
				return &corsValidationError{ErrMalformedXML, "AllowedOrigin must not be empty."}
			}
			if strings.Count(o, "*") > 1 {
				return &corsValidationError{ErrInvalidRequest, fmt.Sprintf("AllowedOrigin %q can not have more than one wildcard.", o)}
			}
		}
		for j, m := range r.AllowedMethods {
			up := strings.ToUpper(strings.TrimSpace(m))
			if !corsAllowedMethods[up] {
				return &corsValidationError{ErrInvalidRequest, fmt.Sprintf("Found unsupported HTTP method in CORS config. Unsupported method is %s", m)}
			}
			r.AllowedMethods[j] = up
		}
		for _, h := range r.AllowedHeaders {
			if h == "" {
				return &corsValidationError{ErrMalformedXML, "AllowedHeader must not be empty."}
			}
			if strings.Count(h, "*") > 1 {
				return &corsValidationError{ErrInvalidRequest, fmt.Sprintf("AllowedHeader %q can not have more than one wildcard.", h)}
			}
		}
		for _, h := range r.ExposeHeaders {
			if h == "" || strings.Contains(h, "*") {
				return &corsValidationError{ErrInvalidRequest, fmt.Sprintf("ExposeHeader %q contains wildcard. We currently do not support wildcard for ExposeHeader.", h)}
			}
		}
		if r.MaxAgeSeconds != nil && *r.MaxAgeSeconds < 0 {
			return &corsValidationError{ErrInvalidRequest, "MaxAgeSeconds must be non-negative."}
		}
	}
	return nil
}

// corsPatternMatch: `*` matches anything; a pattern with one `*` matches a
// value that starts with what is before it and ends with what is after it;
// anything else is equality. fold = case-insensitive (headers; origins are
// case-sensitive).
func corsPatternMatch(pattern, value string, fold bool) bool {
	if pattern == "*" {
		return true
	}
	if fold {
		pattern, value = strings.ToLower(pattern), strings.ToLower(value)
	}
	i := strings.Index(pattern, "*")
	if i < 0 {
		return pattern == value
	}
	prefix, suffix := pattern[:i], pattern[i+1:]
	return len(value) >= len(prefix)+len(suffix) &&
		strings.HasPrefix(value, prefix) && strings.HasSuffix(value, suffix)
}

// corsMatchRule returns the first rule that allows origin + method (and,
// when checkHeaders, every requested header), or nil.
func corsMatchRule(rules []CORSRule, origin, method string, requestedHeaders []string, checkHeaders bool) *CORSRule {
	method = strings.ToUpper(method)
	for i := range rules {
		r := &rules[i]
		if corsAllowedOrigin(r, origin) == "" {
			continue
		}
		methodOK := false
		for _, m := range r.AllowedMethods {
			if strings.EqualFold(m, method) {
				methodOK = true
				break
			}
		}
		if !methodOK {
			continue
		}
		if checkHeaders && !corsHeadersAllowed(r, requestedHeaders) {
			continue
		}
		return r
	}
	return nil
}

// corsAllowedOrigin returns what Access-Control-Allow-Origin should carry
// for this rule and origin: `*` when the rule's matching pattern is `*`,
// the origin itself when a specific pattern matched, "" when none did.
func corsAllowedOrigin(r *CORSRule, origin string) string {
	for _, p := range r.AllowedOrigins {
		if corsPatternMatch(p, origin, false) {
			if p == "*" {
				return "*"
			}
			return origin
		}
	}
	return ""
}

func corsHeadersAllowed(r *CORSRule, requested []string) bool {
	for _, h := range requested {
		ok := false
		for _, p := range r.AllowedHeaders {
			if corsPatternMatch(p, h, true) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// corsRequestedHeaders parses Access-Control-Request-Headers.
func corsRequestedHeaders(r *http.Request) []string {
	raw := r.Header.Get("Access-Control-Request-Headers")
	if raw == "" {
		return nil
	}
	var out []string
	for _, h := range strings.Split(raw, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// setS3CORSHeaders writes the matched rule's headers. preflight adds the
// echoed request headers and Max-Age; an actual response carries only what
// the browser needs to read it.
func setS3CORSHeaders(h http.Header, rule *CORSRule, origin string, requested []string, preflight bool) {
	allow := corsAllowedOrigin(rule, origin)
	if allow == "" {
		return
	}
	h.Set("Access-Control-Allow-Origin", allow)
	if allow != "*" {
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	h.Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))
	if len(rule.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
	addVary(h, "Origin")
	if preflight {
		if len(requested) > 0 {
			h.Set("Access-Control-Allow-Headers", strings.Join(requested, ", "))
		}
		if rule.MaxAgeSeconds != nil {
			h.Set("Access-Control-Max-Age", fmt.Sprint(*rule.MaxAgeSeconds))
		}
		addVary(h, "Access-Control-Request-Method")
		addVary(h, "Access-Control-Request-Headers")
	}
}

// addVary adds a Vary token once.
func addVary(h http.Header, token string) {
	for _, v := range h.Values("Vary") {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return
			}
		}
	}
	h.Add("Vary", token)
}

// corsAppPrefixes are the first path segments of the application's own
// routes: an OPTIONS under one of them is never an S3 preflight.
var corsAppPrefixes = map[string]bool{"api": true, "auth": true, "dashboard": true, "admin": true, "webhook": true}

// corsBucketFromPath: the S3 API is path-style here (the parser's
// parsePath); a preflight for `/` names no bucket.
func corsBucketFromPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return p
}

// corsRuleForRequest finds the first rule of ANY tenant's bucket of that
// name that allows the request — the union a preflight is answered from, and
// what a pre-authentication error may reveal Allow-Origin for. Never use it
// for a response the caller's own bucket decides (corsRuleForTenant). Nil
// without a database, without a bucket name, and on any error (logged): CORS
// is answered best-effort and never fails a request.
func (s *Server) corsRuleForRequest(ctx context.Context, bucket, origin, method string, requested []string, preflight bool) *CORSRule {
	if s.db == nil || bucket == "" || strings.HasPrefix(bucket, "_") {
		return nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT cors_rules FROM buckets WHERE name = $1 AND cors_rules IS NOT NULL ORDER BY tenant_id`, bucket)
	if err != nil {
		s.logger.Warn("cors: bucket lookup failed", zap.String("bucket", bucket), zap.Error(err))
		return nil
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			s.logger.Warn("cors: scan failed", zap.String("bucket", bucket), zap.Error(err))
			return nil
		}
		var rules []CORSRule
		if err := json.Unmarshal(raw, &rules); err != nil {
			s.logger.Warn("cors: stored rules do not parse", zap.String("bucket", bucket), zap.Error(err))
			continue
		}
		if r := corsMatchRule(rules, origin, method, requested, preflight); r != nil {
			return r
		}
	}
	if err := rows.Err(); err != nil {
		s.logger.Warn("cors: bucket lookup failed", zap.String("bucket", bucket), zap.Error(err))
	}
	return nil
}

// corsRuleForTenant finds the first rule of the tenant's OWN bucket that
// allows origin + method (headers are not checked on an actual request —
// AWS). Nil as corsRuleForRequest.
func (s *Server) corsRuleForTenant(ctx context.Context, tenantID, bucket, origin, method string) *CORSRule {
	if s.db == nil || tenantID == "" || bucket == "" || strings.HasPrefix(bucket, "_") {
		return nil
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT cors_rules FROM buckets WHERE tenant_id = $1 AND name = $2 AND cors_rules IS NOT NULL`,
		tenantID, bucket).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		s.logger.Warn("cors: bucket lookup failed", zap.String("tenant_id", tenantID),
			zap.String("bucket", bucket), zap.Error(err))
		return nil
	}
	var rules []CORSRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		s.logger.Warn("cors: stored rules do not parse", zap.String("tenant_id", tenantID),
			zap.String("bucket", bucket), zap.Error(err))
		return nil
	}
	return corsMatchRule(rules, origin, method, nil, false)
}

// varyOrigin is the first thing handleS3Request does: every response to a
// request that carries an Origin varies on it, matched or not.
func varyOrigin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		addVary(w.Header(), "Origin")
	}
}

// applyS3CORSPreAuth is called by handleS3Request just before it writes a
// response for a request whose tenant is not known (an auth failure): ONLY
// Access-Control-Allow-Origin, when some same-named bucket's rule allows the
// origin and method, so a page can read the error.
func (s *Server) applyS3CORSPreAuth(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	rule := s.corsRuleForRequest(r.Context(), corsBucketFromPath(r.URL.Path), origin, r.Method, nil, false)
	if rule == nil {
		return
	}
	if allow := corsAllowedOrigin(rule, origin); allow != "" {
		w.Header().Set("Access-Control-Allow-Origin", allow)
		addVary(w.Header(), "Origin")
	}
}

// applyS3CORSForTenant is called by handleS3Request once the caller is
// authenticated, before the operation runs: the headers of the caller's own
// bucket's matching rule, on whatever response follows. Nothing for a plain
// S3 client (no Origin), a bucket without a configuration or an origin no
// rule of it allows.
func (s *Server) applyS3CORSForTenant(w http.ResponseWriter, r *http.Request, tenantID string) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	rule := s.corsRuleForTenant(r.Context(), tenantID, corsBucketFromPath(r.URL.Path), origin, r.Method)
	if rule == nil {
		return
	}
	setS3CORSHeaders(w.Header(), rule, origin, nil, false)
}

// handleS3Preflight answers a browser's OPTIONS before any authentication,
// from the union of every same-named bucket's rules (no tenant is knowable).
// 200 + the rule's headers on a match; 403 AccessForbidden with AWS's
// CORSResponse message otherwise (no bucket and no rule alike); 400 when the
// request is not a preflight; 404 under an application prefix.
func (s *Server) handleS3Preflight(w http.ResponseWriter, r *http.Request) {
	if corsAppPrefixes[corsBucketFromPath(r.URL.Path)] {
		http.NotFound(w, r)
		return
	}
	varyOrigin(w, r)
	origin := r.Header.Get("Origin")
	method := r.Header.Get("Access-Control-Request-Method")
	if origin == "" || method == "" {
		WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(),
			WithMessage("Insufficient information. Origin request header needed."))
		return
	}
	requested := corsRequestedHeaders(r)
	rule := s.corsRuleForRequest(r.Context(), corsBucketFromPath(r.URL.Path), origin, method, requested, true)
	if rule == nil {
		WriteS3Error(w, ErrCORSForbidden, r.URL.Path, generateRequestID())
		return
	}
	setS3CORSHeaders(w.Header(), rule, origin, requested, true)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// --- the sub-resource ----------------------------------------------------------

func (s *Server) handleGetBucketCors(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	var raw []byte
	err = s.db.QueryRowContext(r.Context(),
		`SELECT cors_rules FROM buckets WHERE tenant_id = $1 AND name = $2`, t.ID, req.Bucket).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		s.writeNoSuchBucket(w, r, t.ID, req.Bucket)
		return
	}
	if err != nil {
		s.logger.Error("query cors rules", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if len(raw) == 0 {
		WriteS3Error(w, ErrNoSuchCORSConfiguration, r.URL.Path, generateRequestID())
		return
	}
	var rules []CORSRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		s.logger.Error("stored cors rules do not parse", zap.String("tenant_id", t.ID), zap.String("bucket", req.Bucket), zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(CORSConfiguration{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Rules: rules})
}

func (s *Server) handlePutBucketCors(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCORSBodyBytes+1))
	if err != nil {
		WriteS3Error(w, bodyReadErrorCode(err), r.URL.Path, generateRequestID())
		return
	}
	if len(body) > maxCORSBodyBytes {
		WriteS3ErrorWithContext(w, ErrInvalidRequest, r.URL.Path, generateRequestID(),
			WithMessage("The CORS configuration is too large."))
		return
	}
	var cfg CORSConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil {
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}
	if err := validateCORSRules(cfg.Rules); err != nil {
		var ve *corsValidationError
		if errors.As(err, &ve) {
			WriteS3ErrorWithContext(w, ve.code, r.URL.Path, generateRequestID(), WithMessage(ve.message))
			return
		}
		WriteS3Error(w, ErrMalformedXML, r.URL.Path, generateRequestID())
		return
	}
	stored, err := json.Marshal(cfg.Rules)
	if err != nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE buckets SET cors_rules = $1::jsonb, updated_at = NOW() WHERE tenant_id = $2 AND name = $3`,
		stored, t.ID, req.Bucket)
	if err != nil {
		s.logger.Error("store cors rules", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		s.writeNoSuchBucket(w, r, t.ID, req.Bucket)
		return
	}
	s.logger.Info("bucket cors configured", zap.String("tenant_id", t.ID),
		zap.String("bucket", req.Bucket), zap.Int("rules", len(cfg.Rules)))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDeleteBucketCors(w http.ResponseWriter, r *http.Request, req *S3Request) {
	t, err := tenant.FromContext(r.Context())
	if err != nil || t == nil {
		WriteS3Error(w, ErrAccessDenied, r.URL.Path, generateRequestID())
		return
	}
	if s.db == nil {
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE buckets SET cors_rules = NULL, updated_at = NOW() WHERE tenant_id = $1 AND name = $2`,
		t.ID, req.Bucket)
	if err != nil {
		s.logger.Error("delete cors rules", zap.Error(err))
		WriteS3Error(w, ErrInternalError, r.URL.Path, generateRequestID())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		s.writeNoSuchBucket(w, r, t.ID, req.Bucket)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeNoSuchBucket: the 404 with the usual "did you mean" suggestion.
func (s *Server) writeNoSuchBucket(w http.ResponseWriter, r *http.Request, tenantID, bucket string) {
	reqID := generateRequestID()
	if suggestion := bucketSuggestion(r.Context(), s.db, tenantID, bucket); suggestion != "" {
		WriteS3ErrorWithContext(w, ErrNoSuchBucket, r.URL.Path, reqID, WithSuggestion(suggestion))
		return
	}
	WriteS3Error(w, ErrNoSuchBucket, r.URL.Path, reqID)
}
