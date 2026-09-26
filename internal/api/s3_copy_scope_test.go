package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/stretchr/testify/assert"
)

// R5-02: CopyObject is dispatched as PutObject; the scoped-key checks must
// also cover the SOURCE (GetObject permission + source bucket in scope).
func TestCopySourceScopeDenied(t *testing.T) {
	full := &auth.KeyScope{Permissions: []string{"*"}}
	putGet := &auth.KeyScope{Permissions: []string{"PutObject", "GetObject"}, BucketScope: []string{"dest"}}
	putOnly := &auth.KeyScope{Permissions: []string{"PutObject"}, BucketScope: []string{"dest", "src"}}
	unscopedBuckets := &auth.KeyScope{Permissions: []string{"PutObject", "GetObject"}}

	cases := []struct {
		name   string
		scope  *auth.KeyScope
		source string // "" = header absent
		denied bool
	}{
		{"no copy header", putOnly, "", false},
		{"nil scope", nil, "/src/k", false},
		{"full access", full, "/src/k", false},
		{"unscoped buckets, has GetObject", unscopedBuckets, "/src/k", false},
		{"source in scope", putGet, "/dest/other", false},
		{"source in scope, no leading slash", putGet, "dest/other", false},
		{"source in scope with versionId", putGet, "/dest/other?versionId=abc", false},
		{"source in scope, encoded key", putGet, "/dest/a%20b/c%2Fd", false},
		{"source bucket outside scope", putGet, "/src/k", true},
		{"source bucket outside scope, no slash", putGet, "src/k", true},
		{"source bucket outside scope, versionId", putGet, "/src/k?versionId=1", true},
		{"percent-encoded bucket never matches scope", putGet, "/%64est/k", true},
		{"uppercase bucket does not match scope", putGet, "/DEST/k", true},
		{"missing GetObject even with bucket in scope", putOnly, "/src/k", true},
		{"malformed source is left to the handler (400)", putGet, "/nobucketkey", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/dest/k", nil)
			if tc.source != "" {
				r.Header.Set("x-amz-copy-source", tc.source)
			}
			_, denied := copySourceScopeDenied(tc.scope, r)
			assert.Equal(t, tc.denied, denied)
		})
	}
}

func TestCopySourceScopeDenied_HeaderCaseAndFirstValue(t *testing.T) {
	scope := &auth.KeyScope{Permissions: []string{"PutObject", "GetObject"}, BucketScope: []string{"dest"}}
	r := httptest.NewRequest(http.MethodPut, "/dest/k", nil)
	r.Header["X-Amz-Copy-Source"] = []string{"/src/k", "/dest/k"} // first value is what handleCopyObject uses too
	_, denied := copySourceScopeDenied(scope, r)
	assert.True(t, denied)
}
