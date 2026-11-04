// internal/auth/post_policy.go
//
// The S3 presigned POST policy (WP-POST-1, Stage 1a Prompt 57): a browser
// upload form carries a base64 JSON policy — an expiration and a list of
// conditions on the form's fields — and its SigV4 signature, made with the
// credential's signing key over the base64 policy itself. This is the ONE
// verifier's fourth entrance (beside ValidateRequest, ValidateDescribed and
// the presigned query): the credential is resolved by LookupCredential (the
// one lookup — revoked, expired, STS-bounded, agent-grant-bounded keys
// exactly as for a PUT), the signing key is deriveSigningKey (the
// verifier's own derivation, never a copy), the signature is compared in
// constant time, the key's IP allow-list is applied to the client address
// the caller vouches for, and every condition is held against the form.
// What a PUT checks after authentication — expiry, scope, permissions,
// Private, the bucket and the key — the S3 layer checks for the POST the
// same way, on the same code path.
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Post-policy errors. The S3 layer answers ErrPolicyMalformed with 400
// InvalidPolicyDocument, ErrPolicyExpired and ErrPolicyCondition with 403
// AccessDenied in AWS's wording ("Invalid according to Policy: Policy
// expired." / "… Policy Condition failed: [...]"), a missing
// or malformed signature field with 400 InvalidArgument, and the lookup's
// own errors (ErrAccessKeyRevoked, ErrSignatureMismatch, ErrIPDenied) as a
// PUT's.
var (
	ErrPolicyMalformed = errors.New("invalid policy document")
	ErrPolicyExpired   = errors.New("policy expired")
	ErrPolicyCondition = errors.New("policy condition failed")
	ErrPolicyFields    = errors.New("the POST form is missing a signature field")
)

// PolicyCondition is one condition of a policy: an exact match ("eq",
// from the {"field": "value"} or ["eq", "$field", "value"] form), a prefix
// ("starts-with"), or the file's size range ("content-length-range").
// Field is lower-cased and has no "$".
type PolicyCondition struct {
	Op       string
	Field    string
	Value    string
	Min, Max int64
}

// PostPolicy is a parsed policy document.
type PostPolicy struct {
	Expiration time.Time
	Conditions []PolicyCondition
}

// SizeRange is the policy's content-length-range (ok = it has one; with
// several, the narrowest — every one must hold).
func (p *PostPolicy) SizeRange() (min, max int64, ok bool) {
	for _, c := range p.Conditions {
		if c.Op != "content-length-range" {
			continue
		}
		if !ok || c.Min > min {
			min = c.Min
		}
		if !ok || c.Max < max {
			max = c.Max
		}
		ok = true
	}
	return min, max, ok
}

// maxPolicyBytes bounds a decoded policy (AWS: the form's fields are at
// most 20 KB; a policy is one of them).
const maxPolicyBytes = 20 << 10

// ParsePostPolicy decodes a base64 policy: its expiration (RFC 3339, as
// every SDK writes it) and its conditions. An unknown operator, a condition
// of another shape, a missing or unreadable expiration is ErrPolicyMalformed.
func ParsePostPolicy(b64 string) (*PostPolicy, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) == 0 || len(raw) > maxPolicyBytes {
		return nil, fmt.Errorf("%w: the policy is not base64 of at most %d bytes", ErrPolicyMalformed, maxPolicyBytes)
	}
	var doc struct {
		Expiration string            `json:"expiration"`
		Conditions []json.RawMessage `json:"conditions"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPolicyMalformed, err)
	}
	exp, err := time.Parse(time.RFC3339Nano, doc.Expiration)
	if err != nil {
		return nil, fmt.Errorf("%w: expiration %q is not an ISO 8601 UTC time", ErrPolicyMalformed, doc.Expiration)
	}
	p := &PostPolicy{Expiration: exp}
	for i, rc := range doc.Conditions {
		c, err := parseCondition(rc)
		if err != nil {
			return nil, fmt.Errorf("%w: condition %d: %w", ErrPolicyMalformed, i, err)
		}
		p.Conditions = append(p.Conditions, c...)
	}
	return p, nil
}

func parseCondition(rc json.RawMessage) ([]PolicyCondition, error) {
	trimmed := strings.TrimSpace(string(rc))
	if strings.HasPrefix(trimmed, "{") {
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil || len(m) == 0 {
			return nil, errors.New("an exact-match condition is one {\"field\": \"value\"}")
		}
		var out []PolicyCondition
		for k, v := range m {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("the value of %q is not a string", k)
			}
			out = append(out, PolicyCondition{Op: "eq", Field: strings.ToLower(strings.TrimPrefix(k, "$")), Value: s})
		}
		return out, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(rc, &arr); err != nil || len(arr) != 3 {
		return nil, errors.New("a condition is an object or a three-element array")
	}
	var op string
	if err := json.Unmarshal(arr[0], &op); err != nil {
		return nil, errors.New("the operator is not a string")
	}
	switch strings.ToLower(op) {
	case "eq", "starts-with":
		var field, value string
		if json.Unmarshal(arr[1], &field) != nil || json.Unmarshal(arr[2], &value) != nil || !strings.HasPrefix(field, "$") {
			return nil, fmt.Errorf("%s takes \"$field\" and a string", op)
		}
		return []PolicyCondition{{Op: strings.ToLower(op), Field: strings.ToLower(field[1:]), Value: value}}, nil
	case "content-length-range":
		lo, err1 := policyInt(arr[1])
		hi, err2 := policyInt(arr[2])
		if err1 != nil || err2 != nil || lo < 0 || hi < lo {
			return nil, errors.New("content-length-range takes two non-negative integers, min ≤ max")
		}
		return []PolicyCondition{{Op: "content-length-range", Min: lo, Max: hi}}, nil
	}
	return nil, fmt.Errorf("unknown operator %q", op)
}

func policyInt(raw json.RawMessage) (int64, error) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		// Some clients write the bounds as strings.
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
		n = json.Number(s)
	}
	return strconv.ParseInt(n.String(), 10, 64)
}

// unsignedPolicyFields are the form fields a policy need not name (AWS:
// the signature, the policy, the file, and any x-ignore-* field).
func unsignedPolicyField(name string) bool {
	return name == "x-amz-signature" || name == "policy" || name == "file" || strings.HasPrefix(name, "x-ignore-")
}

// ValidatePostPolicy verifies a POST form's policy. fields are the form's
// fields with lower-cased names (the caller adds "bucket" from the URL;
// "key" is the field as sent, ${filename} unexpanded — what the policy
// names); clientIP is the address the key's allow-list is held to. It
// returns the key's tenant and scope and the parsed policy (its size range
// is the caller's to hold the file to).
func (a *Auth) ValidatePostPolicy(ctx context.Context, fields map[string]string, clientIP string) (string, *KeyScope, *PostPolicy, error) {
	if fields["x-amz-algorithm"] == "" && fields["awsaccesskeyid"] != "" {
		// Refused before any lookup: a malformed form, never a "known key"
		// (RP96-B-2 — ErrSignatureMismatch classified the id as known and
		// hashed an attacker-chosen id into the by-key series, R11-28).
		return "", nil, nil, fmt.Errorf("%w: a SigV2 POST policy (AWSAccessKeyId) is not supported", ErrPolicyFields)
	}
	for _, f := range []string{"x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-signature", "policy"} {
		if fields[f] == "" {
			return "", nil, nil, fmt.Errorf("%w: %s", ErrPolicyFields, f)
		}
	}
	if fields["x-amz-algorithm"] != algorithm {
		return "", nil, nil, fmt.Errorf("%w: x-amz-algorithm must be %s", ErrPolicyFields, algorithm)
	}
	cred := strings.Split(fields["x-amz-credential"], "/")
	if len(cred) != 5 || cred[0] == "" || cred[3] != "s3" || cred[4] != aws4Request {
		return "", nil, nil, fmt.Errorf("%w: x-amz-credential is <key>/<date>/<region>/s3/aws4_request", ErrPolicyFields)
	}
	accessKey, date, region := cred[0], cred[1], cred[2]
	if _, err := time.Parse(timeFormat, fields["x-amz-date"]); err != nil || !strings.HasPrefix(fields["x-amz-date"], date) {
		return "", nil, nil, fmt.Errorf("%w: x-amz-date must be the credential's day, YYYYMMDDTHHMMSSZ", ErrPolicyFields)
	}
	policy, err := ParsePostPolicy(fields["policy"])
	if err != nil {
		return "", nil, nil, err
	}
	if !verifierNow().UTC().Before(policy.Expiration) {
		return "", nil, nil, ErrPolicyExpired
	}
	if a.db == nil {
		return "test-tenant", &KeyScope{Permissions: []string{"*"}}, policy, nil
	}
	c, err := a.LookupCredential(ctx, accessKey)
	if err != nil {
		return "", nil, nil, err
	}
	if c.SecretKey == "" {
		a.logger.Warn("access key has no stored secret — cannot verify a POST policy; regenerate this API key",
			zap.String("tenant_id", c.TenantID))
		return "", nil, nil, fmt.Errorf("%w: key has no stored secret for signature verification", ErrSignatureMismatch)
	}
	key := a.deriveSigningKey(c.SecretKey, date, region, "s3")
	want := hex.EncodeToString(hmacSHA256(key, []byte(fields["policy"])))
	if subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(fields["x-amz-signature"]))) != 1 {
		return "", nil, nil, ErrSignatureMismatch
	}
	if c.Scope != nil && !CheckIPAllowlist(c.Scope.IPAllowlist, clientIP) {
		return "", nil, nil, ErrIPDenied
	}
	if err := policy.check(fields); err != nil {
		return "", nil, nil, err
	}
	return c.TenantID, c.Scope, policy, nil
}

// check holds every condition against the form, and every form field to a
// condition naming it.
func (p *PostPolicy) check(fields map[string]string) error {
	named := map[string]bool{}
	for _, c := range p.Conditions {
		if c.Op == "content-length-range" {
			continue
		}
		named[c.Field] = true
		got := fields[c.Field]
		switch c.Op {
		case "eq":
			if got != c.Value {
				return fmt.Errorf("%w: [\"eq\", \"$%s\", %q]", ErrPolicyCondition, c.Field, c.Value)
			}
		case "starts-with":
			// Content-Type may name several comma-separated types; each
			// must start with the prefix (AWS's rule).
			parts := []string{got}
			if c.Field == "content-type" {
				parts = strings.Split(got, ",")
			}
			for _, v := range parts {
				if !strings.HasPrefix(strings.TrimSpace(v), c.Value) {
					return fmt.Errorf("%w: [\"starts-with\", \"$%s\", %q]", ErrPolicyCondition, c.Field, c.Value)
				}
			}
		}
	}
	for name := range fields {
		if name == "bucket" || unsignedPolicyField(name) || named[name] {
			continue
		}
		return fmt.Errorf("%w: extra input fields: %s", ErrPolicyCondition, name)
	}
	if !named["bucket"] {
		return fmt.Errorf("%w: the policy names no bucket", ErrPolicyCondition)
	}
	return nil
}
