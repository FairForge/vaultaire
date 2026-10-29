package auth

// The described-request verifier (WP-EDGE-1 Part A, Stage 1a Prompt 44).
// ONE verifier, two entrances: ValidateRequest builds a RequestDescriptor
// from the live *http.Request and the edge admit builds one from what the
// Worker on s3.stored.cloud saw — method, host, raw path, raw query, every
// header the client sent — and both go through validateDescribed. The
// verifier picks the headers SignedHeaders names, so a Worker that forwards
// every client header (minus hop-by-hop) gives the same verdict the origin
// gives the same request: a tampered Host, a replayed signature, a revoked
// key, a skewed date all fail here as there. The key's IP allow-list
// (review R1-01) is applied to the client IP in both entrances — the
// origin's from internal/clientip, the Worker's from CF-Connecting-IP,
// trusted only because the admit itself is accepted only from a Cloudflare
// peer with the service token.
//
// A described request has no body: the payload hash the client declared in
// x-amz-content-sha256 is a CLAIM the signature covers (Step 0.5 in
// docs/reviews/WP-EDGE-1A.md); the live entrance makes the body live up to
// it with wrapPayloadVerification, the edge commit compares the claim with
// the SHA-256 the Worker computed as it streamed.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/clientip"
)

// ErrIPDenied: the key's IP allow-list does not admit the client's address.
// The S3 layer answers AccessDenied with its "restricted by IP address"
// hint and counts the failure as ip_denied.
var ErrIPDenied = errors.New("this key is restricted by IP address")

// RequestDescriptor is an S3 request as a verifier needs it: what the
// client signed over. Headers carry every header the client sent, by any
// case (the Worker lower-cases them); Host is the Host header as received —
// a descriptor whose Host differs from the signed one fails. Path is the
// raw path as received (percent-encoding kept, so an encoded slash in a key
// verifies as the client signed it), RawQuery the raw query string.
// ContentLength is the Content-Length header's value, -1 when none.
type RequestDescriptor struct {
	Method        string
	Host          string
	Path          string
	RawQuery      string
	Headers       http.Header
	ContentLength int64
}

// DescribeRequest is the live entrance's descriptor: the request as the
// origin received it.
func DescribeRequest(r *http.Request) RequestDescriptor {
	return RequestDescriptor{
		Method: r.Method, Host: r.Host, Path: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery,
		Headers: r.Header, ContentLength: r.ContentLength,
	}
}

// request builds the *http.Request the SigV4 verifier canonicalises from
// the descriptor — no body, the headers as described. The path is kept
// raw: EscapedPath returns it for the "as sent" canonical form and Path
// decodes it for the spec form, exactly as the live request yields both.
func (d RequestDescriptor) request() (*http.Request, error) {
	if d.Method == "" {
		return nil, fmt.Errorf("described request: method required")
	}
	if d.Path == "" || d.Path[0] != '/' {
		return nil, fmt.Errorf("described request: path must start with /")
	}
	u, err := url.ParseRequestURI(d.Path)
	if err != nil {
		return nil, fmt.Errorf("described request: path: %w", err)
	}
	u.RawQuery = d.RawQuery
	r := &http.Request{Method: d.Method, URL: u, Host: d.Host, Header: http.Header{}, ContentLength: d.ContentLength}
	for name, vals := range d.Headers {
		canon := http.CanonicalHeaderKey(name)
		if canon == "Host" {
			// The Host the client signed is the descriptor's own; a Host
			// header beside it would be a second opinion.
			continue
		}
		r.Header[canon] = append(r.Header[canon], vals...)
	}
	if r.ContentLength < 0 {
		if v := r.Header.Get("Content-Length"); v != "" {
			if n, perr := strconv.ParseInt(strings.TrimSpace(v), 10, 64); perr == nil {
				r.ContentLength = n
			}
		}
	}
	return r, nil
}

// ValidateDescribed is the edge entrance: the Worker's description of the
// client's request, verified exactly as the origin verifies the request
// itself (SigV4 header auth; a presigned query is refused here — the Worker
// passes presigned requests through to the origin). clientIP is the
// address the key's allow-list is checked against. Returns the key's
// tenant and scope.
func (a *Auth) ValidateDescribed(ctx context.Context, d RequestDescriptor, clientIP string) (tenantID string, scope *KeyScope, err error) {
	r, err := d.request()
	if err != nil {
		return "", nil, err
	}
	if q := r.URL.Query(); q.Has("X-Amz-Signature") || q.Has("X-Amz-Credential") || q.Has("X-Amz-Algorithm") {
		return "", nil, fmt.Errorf("%w: a presigned request is not verified by description", ErrSignatureMismatch)
	}
	return a.validateDescribed(r.WithContext(ctx), clientIP)
}

// validateDescribed is the verifier both entrances share: the header
// checks, the credential lookup, the SigV4 verification and the key's IP
// allow-list, over r (the live request, or the one a descriptor builds;
// its context is the lookup's).
func (a *Auth) validateDescribed(r *http.Request, clientIP string) (string, *KeyScope, error) {
	fullAccess := &KeyScope{Permissions: []string{"*"}}

	authHeader := r.Header.Get("Authorization")

	if authHeader == "" {
		if r.URL.Query().Get("AWSAccessKeyId") != "" {
			return "", nil, fmt.Errorf("%w: unsigned AWSAccessKeyId query authentication is not supported", ErrSignatureMismatch)
		}
		if a.db == nil {
			return "test-tenant", fullAccess, nil
		}
		return "", nil, fmt.Errorf("missing authorization")
	}

	// SigV2-era clients: key-existence only, refused.
	if strings.HasPrefix(authHeader, "AWS ") {
		return "", nil, fmt.Errorf("%w: AWS signature version 2 is not supported", ErrSignatureMismatch)
	}

	// The scheme must be followed by a space (RFC 9110 § 11.4: scheme SP
	// params — every SDK sends it). It was a bare HasPrefix, which also
	// attempted "AWS4-HMAC-SHA256,Credential=…" and a tab after the scheme,
	// shapes the S3 credential gate's space-Cut did not see (RP58-8 c): the
	// two predicates must agree, so the verifier narrows to what
	// IsAWSAuthorization names. SigV4A (AWS4-ECDSA-P256-SHA256) is such a
	// credential too and is refused here as unsupported (RP58-1).
	if !strings.HasPrefix(authHeader, algorithm+" ") {
		return "", nil, fmt.Errorf("invalid authorization format")
	}

	params, err := parseSigV4AuthHeader(authHeader)
	if err != nil {
		// The error names parts of the header; the header is the credential
		// (CodeQL go/clear-text-logging) — the caller gets it, the log does not.
		a.logger.Debug("failed to parse SigV4 authorization header")
		return "", nil, err
	}
	if a.db == nil {
		a.logger.Warn("no database connection, using test-tenant")
		return "test-tenant", fullAccess, nil
	}
	cred, err := a.LookupCredential(r.Context(), params.AccessKey)
	if err != nil {
		return "", nil, err
	}
	// A key whose plaintext secret was never stored (legacy rows with only
	// a bcrypt secret_hash) can never verify: fail closed, but leave an
	// actionable trail — the key must be regenerated.
	if cred.SecretKey == "" {
		// The DB-derived tenant, never the request-derived key id (CodeQL
		// go/clear-text-logging on the Authorization header); the metric
		// carries the per-key signal.
		a.logger.Warn("access key has no stored secret — cannot verify SigV4 signature; regenerate this API key",
			zap.String("tenant_id", cred.TenantID))
		return "", nil, fmt.Errorf("%w: key has no stored secret for signature verification; regenerate this API key", ErrSignatureMismatch)
	}
	if err := a.verifySigV4(r, params, cred.SecretKey, cred.TenantID); err != nil {
		a.logger.Debug("signature verification failed",
			zap.String("tenant_id", cred.TenantID),
			zap.Bool("time_skewed", errors.Is(err, ErrRequestTimeSkewed)))
		return "", nil, err
	}
	// A signed streaming body is checked chunk by chunk as the handler reads
	// it (WP-R15-1): the verifier rides on the body. A described request has
	// none (the edge never takes a signed stream — edgeStreamingOf).
	if v := a.streamChunkVerifier(r, params, cred.SecretKey); v != nil && r.Body != nil {
		r.Body = &signedStreamBody{ReadCloser: r.Body, v: v}
	}
	// The key's IP allow-list, against the address the entrance vouches for
	// (R1-01): the origin's trusted peer, or the Worker's CF-Connecting-IP.
	if cred.Scope != nil && !CheckIPAllowlist(cred.Scope.IPAllowlist, clientIP) {
		return "", nil, ErrIPDenied
	}
	return cred.TenantID, cred.Scope, nil
}

// clientIPOf is the live entrance's client address (internal/clientip is
// the one reader of the proxy headers).
func clientIPOf(r *http.Request) string {
	return clientip.FromRequest(r)
}
