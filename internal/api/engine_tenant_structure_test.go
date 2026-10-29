package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/FairForge/vaultaire/internal/engine"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R6-1 A5 (R6-22): no engine call runs without a tenant. The engine
// refuses one (engine.ErrNoTenant, vaultaire_engine_no_tenant_refusals_total);
// this proves, call site by call site, that none is made: every storage call
// on the engine in this package is listed with where its context's tenant
// comes from. A new call site fails the test until it is added here with its
// reason — read the context it passes before you add it.
var engineCallTenants = map[string]string{
	"account_export.go:deleteExportObject": "tctx = common.WithTenantID(ctx, tenantID)",
	"cdn.go:handleCDNRequest":              "ctx = common.WithTenantID(ctx, tenantID) (the slug's tenant)",
	"chunk_move.go:moveOne":                "the one address: engine.ChunkContext",
	"chunk_move.go:removeLegacy":           "engine.LegacyChunkContext(ctx, tenantID)",
	"chunk_move.go:orphan":                 "engine.LegacyChunkContext(ctx, tenantID)",
	"chunk_store.go:put":                   "engine.ChunkContext",
	"chunk_store.go:getAt":                 "engine.ChunkContext / LegacyChunkContext (its two callers, get and getLegacy)",
	"chunk_store.go:delete":                "engine.ChunkContext",
	"chunk_store.go:existsAt":              "its callers: exists (engine.ChunkContext), legacyCopyExists and the chunk move (engine.LegacyChunkContext)",
	"deletion_runner.go:deleteOnBackend":   "tctx = common.WithTenantID(ctx, d.TenantID) (eraseObjects → deleteObject)",
	"object_write_shared.go:placeOn":       "its callers: the PUT, Complete and CopyObject request contexts (handleS3Request sets common.TenantIDKey) and the generated writer's tctx",
	"s3_batch.go:batchDeleteKey":           "the request context (handleS3Request)",
	"s3_copy_source.go:openCopySource":     "its callers' request contexts: copyObject and uploadPartCopy (handleS3Request)",
	"s3_engine_adapter.go:HandleDelete":    "the request context (handleS3Request)",
	"s3_engine_adapter.go:HandleGet":       "the request context (handleS3Request)",
	"sse_stream.go:openSealedGet":          "HandleGet's request context",
	"vault_parity.go:Open":                 "common.WithTenantID(ctx, tenantID) on the data ranges",
	"vault_parity.go:protect":              "tctx = common.WithTenantID(ctx, c.tenantID)",
	"vault_tape_audit.go:audit":            "tctx = common.WithTenantID(ctx, p.Tenant) (WP-VAULT-2 A4)",
}

// engineStorageCalls finds the storage calls on an engine in this package's
// product files: X.M(...) with M a storage method and X an engine-valued
// expression by its spelling (a.engine, s.eng, ce, sb, eng …).
func engineStorageCalls(t *testing.T) map[string][]string {
	t.Helper()
	methods := map[string]bool{"Get": true, "GetRange": true, "Put": true, "Delete": true,
		"GetOn": true, "PutOn": true, "DeleteOn": true, "ExistsOn": true}
	receivers := map[string]bool{"engine": true, "eng": true, "ce": true, "sb": true}
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	found := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		require.NoError(t, err)
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !methods[sel.Sel.Name] {
					return true
				}
				var recv string
				switch x := sel.X.(type) {
				case *ast.Ident:
					recv = x.Name
				case *ast.SelectorExpr:
					recv = x.Sel.Name
				}
				if !receivers[recv] {
					return true
				}
				site := name + ":" + fd.Name.Name
				found[site] = append(found[site], fmt.Sprintf("%s.%s (line %d)", recv, sel.Sel.Name, fset.Position(call.Pos()).Line))
				return true
			})
		}
	}
	return found
}

func TestEngineCalls_CarryTheTenant(t *testing.T) {
	found := engineStorageCalls(t)
	var unlisted, stale []string
	for site, calls := range found {
		if _, ok := engineCallTenants[site]; !ok {
			unlisted = append(unlisted, site+": "+strings.Join(calls, ", "))
		}
	}
	for site := range engineCallTenants {
		if _, ok := found[site]; !ok {
			stale = append(stale, site)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	assert.Empty(t, unlisted, "an engine storage call not in engineCallTenants: say where its context's tenant comes from")
	assert.Empty(t, stale, "engineCallTenants names a call site that no longer exists")
}

// noTenantRefusals is the sum of vaultaire_engine_no_tenant_refusals_total.
func noTenantRefusals() float64 {
	var n float64
	for _, op := range []string{"put", "get", "get_range", "delete", "exists"} {
		n += promtest.ToFloat64(engine.NoTenantRefusalsCounter().WithLabelValues(op))
	}
	return n
}

// expectedNoTenantRefusals: the refusals tests made on purpose.
var expectedNoTenantRefusals atomic.Int64

// expectNoTenantRefusals runs fn, a deliberate call with no tenant, and
// declares the refusals it made (TestMain counts every other one as a bug).
func expectNoTenantRefusals(t *testing.T, fn func()) {
	t.Helper()
	before := noTenantRefusals()
	fn()
	made := noTenantRefusals() - before
	require.Positive(t, made, "the call was meant to be refused for having no tenant")
	expectedNoTenantRefusals.Add(int64(made))
}

// TestMain fails the package when any of its tests made an engine call
// with no tenant other than the ones declared (A5: the counter must stay 0
// through the suite, as on prod).
func TestMain(m *testing.M) {
	// The package's multipart fixtures upload 64 KiB parts; S3's 5 MiB
	// minimum (multipartMinPartBytes) is held by TestMultipartConformance,
	// which restores it.
	multipartMinPartBytes = 1
	code := m.Run()
	if refused := noTenantRefusals() - float64(expectedNoTenantRefusals.Load()); code == 0 && refused != 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %v engine call(s) made with no tenant in the context during the api tests (vaultaire_engine_no_tenant_refusals_total)\n", refused)
		code = 1
	}
	os.Exit(code)
}
