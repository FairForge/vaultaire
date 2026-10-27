package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/crypto"
)

// Three tenants (top directories) whose files overlap in known amounts: X
// and Y are independent 8 MiB golden bodies (no chunk in common), and a file
// equal to another chunks identically. So, in bytes:
//
//	a/x1 = X, a/x2 = X     → tenant a holds X once      (8 MiB unique)
//	b/x  = X, b/y  = Y     → tenant b holds X and Y     (16 MiB unique)
//	c/y  = Y               → tenant c holds Y           (8 MiB unique)
//
// total 40 MiB, per-tenant unique 32 MiB, global unique 16 MiB, and
// cross_tenant_only_fraction = (32 − 16) / 40 = 0.4.
func TestMeasure_PerTenantAndCrossTenant(t *testing.T) {
	const mib = 1 << 20
	x := crypto.GoldenBody(11, 8*mib)
	y := crypto.GoldenBody(12, 8*mib)
	dir := t.TempDir()
	for path, body := range map[string][]byte{"a/x1": x, "a/sub/x2": x, "b/x": x, "b/y": y, "c/y": y} {
		p := filepath.Join(dir, filepath.FromSlash(path))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, body, 0o600))
	}

	rep, err := measureDir(dir)
	require.NoError(t, err)
	assert.Equal(t, 3, rep.Tenants)
	assert.Equal(t, 5, rep.Files)
	assert.Equal(t, int64(40*mib), rep.TotalBytes)
	assert.Equal(t, int64(32*mib), rep.PerTenantUniqueBytes)
	assert.Equal(t, int64(16*mib), rep.GlobalUniqueBytes)
	assert.InDelta(t, 0.4, rep.CrossTenantOnlyFraction, 1e-9)
	assert.Equal(t, int64(8*mib), rep.ByTenant["a"].UniqueBytes)
	assert.Equal(t, int64(16*mib), rep.ByTenant["b"].UniqueBytes)
	assert.Equal(t, int64(8*mib), rep.ByTenant["c"].UniqueBytes)

	var out bytes.Buffer
	rep.writeTable(&out)
	assert.Contains(t, out.String(), "cross_tenant_only_fraction")
	assert.True(t, strings.Contains(out.String(), "0.4000"), out.String())
}

// A file at the top of the tree belongs to no tenant: refused, never
// guessed.
func TestMeasure_TopLevelFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loose"), []byte("x"), 0o600))
	_, err := measureDir(dir)
	assert.Error(t, err)
}
