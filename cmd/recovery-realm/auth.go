package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/recovery/realm"
	"github.com/golang-jwt/jwt/v5"
)

// Tenant tokens (docs/reviews/K5-PROBE.md § Authentication): HS256 under
// the tenant's key named by kid "<tenant>:<version>"; iss = that tenant;
// aud = this realm's id (32 lower-case hex, a single audience); sub = the
// opaque user id; nbf and exp both present, exp − nbf ≤ 86 400 s, 5 s of
// clock skew; scope "user". Anything else is 401 InvalidAuth.

const (
	maxTokenLifetime = 24 * time.Hour // MAX_LIFETIME_SECONDS
	clockSkew        = 5 * time.Second
	maxTokenLength   = 1000 // the SDK validator's max_token_length
	minTenantKeyLen  = 32
)

var tenantName = regexp.MustCompile(`^(test-)?[a-zA-Z0-9]+$`)

// errAuth is every refused token; the reason is for the test and the debug
// log, never the response.
var errAuth = errors.New("invalid authentication")

// keyring is the tenants' HS256 keys by name and version.
type keyring map[string]map[uint64][]byte

// parseKeyring reads {"<tenant>": {"<version>": "<hex key, ≥ 32 bytes>"}}.
// Errors never quote a key.
func parseKeyring(raw []byte) (keyring, error) {
	var doc map[string]map[string]string
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, errors.New("tenants: not a JSON object of {tenant: {version: hex key}}")
	}
	if len(doc) == 0 {
		return nil, errors.New("tenants: no tenant")
	}
	kr := keyring{}
	for tenant, versions := range doc {
		if !tenantName.MatchString(tenant) {
			return nil, fmt.Errorf("tenants: %q is not an alphanumeric tenant name", tenant)
		}
		if len(versions) == 0 {
			return nil, fmt.Errorf("tenants: %s has no key", tenant)
		}
		kr[tenant] = map[uint64][]byte{}
		for v, keyHex := range versions {
			ver, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("tenants: %s: version %q is not an integer", tenant, v)
			}
			key, err := hex.DecodeString(strings.TrimSpace(keyHex))
			if err != nil || len(key) < minTenantKeyLen {
				return nil, fmt.Errorf("tenants: %s:%d: the key is not hex of at least %d bytes", tenant, ver, minTenantKeyLen)
			}
			kr[tenant][ver] = key
		}
	}
	return kr, nil
}

type tokenClaims struct {
	jwt.RegisteredClaims
	Scope *string `json:"scope,omitempty"`
}

type authenticator struct {
	realmHex string
	keys     keyring
	now      func() time.Time
}

// validate returns who the token is for, or errAuth with the reason.
func (a *authenticator) validate(raw string) (realm.Auth, error) {
	if raw == "" {
		return realm.Auth{}, fmt.Errorf("%w: no token", errAuth)
	}
	if len(raw) > maxTokenLength {
		return realm.Auth{}, fmt.Errorf("%w: token too long", errAuth)
	}
	var tenant string
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithAudience(a.realmHex),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(clockSkew),
		jwt.WithTimeFunc(a.now),
	)
	var c tokenClaims
	_, err := parser.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		name, ver, ok := strings.Cut(kid, ":")
		v, perr := strconv.ParseUint(ver, 10, 64)
		if !ok || perr != nil {
			return nil, errors.New("kid is not tenant:version")
		}
		key, ok := a.keys[name][v]
		if !ok {
			return nil, errors.New("unknown kid")
		}
		tenant = name
		return key, nil
	})
	if err != nil {
		return realm.Auth{}, fmt.Errorf("%w: %w", errAuth, err)
	}
	switch {
	case c.Issuer != tenant:
		return realm.Auth{}, fmt.Errorf("%w: iss is not the kid's tenant", errAuth)
	case c.Subject == "":
		return realm.Auth{}, fmt.Errorf("%w: no sub", errAuth)
	case len(c.Audience) != 1:
		return realm.Auth{}, fmt.Errorf("%w: aud must name this realm alone", errAuth)
	case c.NotBefore == nil || c.ExpiresAt == nil:
		return realm.Auth{}, fmt.Errorf("%w: nbf and exp are required", errAuth)
	case c.ExpiresAt.Sub(c.NotBefore.Time) > maxTokenLifetime:
		return realm.Auth{}, fmt.Errorf("%w: exp - nbf exceeds 86400 s", errAuth)
	case c.Scope == nil || *c.Scope != "user":
		return realm.Auth{}, fmt.Errorf("%w: scope must be user", errAuth)
	}
	return realm.Auth{Tenant: tenant, Subject: c.Subject}, nil
}
