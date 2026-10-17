package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R6-2: CoreEngine.drivers and CoreEngine.primary are read and written
// inside e.mu sections only. These are those sections, by file:function;
// every other call works from the view it took (e.view()). A new read of
// either field elsewhere fails here — the admin dashboard swaps the primary
// on a serving engine, and a map read beside AddDriver's write was a runtime
// fault ("concurrent map read and map write"), not a race report.
var lockedRegistrySites = map[string]bool{
	"engine.go:AddDriver":            true,
	"engine.go:SetPrimary":           true,
	"engine.go:SwapPrimary":          true,
	"engine.go:view":                 true,
	"engine.go:GetDriverNames":       true,
	"engine.go:GetPrimary":           true,
	"engine.go:GetDriver":            true,
	"store_identity.go:SharedStores": true,
}

// registrySite is one read or write of a registry field of a CoreEngine.
type registrySite struct {
	site   string // file:function
	locked bool   // the function takes e.mu (Lock or RLock)
}

func TestEngineRegistry_ReadOnlyUnderTheLock(t *testing.T) {
	// Arrange
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var sites []registrySite
	var mapWrites []string

	// Act
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f) // #nosec G304 — the package's own sources
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, f, src, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			engines := coreEngineIdents(fn)
			site := f + ":" + fn.Name.Name
			reads, locked := false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					id, ok := x.X.(*ast.Ident)
					if !ok || !engines[id.Name] {
						return true
					}
					if x.Sel.Name == "drivers" || x.Sel.Name == "primary" {
						reads = true
					}
				case *ast.CallExpr:
					// e.mu.Lock() / e.mu.RLock()
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Lock" || sel.Sel.Name == "RLock") {
						if mu, ok := sel.X.(*ast.SelectorExpr); ok && mu.Sel.Name == "mu" {
							if id, ok := mu.X.(*ast.Ident); ok && engines[id.Name] {
								locked = true
							}
						}
					}
					// delete(<x>.drivers, …)
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "delete" && len(x.Args) > 0 && isDriversField(x.Args[0]) {
						mapWrites = append(mapWrites, site)
					}
				case *ast.AssignStmt:
					// <x>.drivers[k] = … writes a map a view may hold.
					for _, lhs := range x.Lhs {
						if ix, ok := lhs.(*ast.IndexExpr); ok && isDriversField(ix.X) {
							mapWrites = append(mapWrites, site)
						}
					}
				}
				return true
			})
			if reads {
				sites = append(sites, registrySite{site: site, locked: locked})
			}
		}
	}

	// Assert
	var unlisted, unlocked []string
	seen := map[string]bool{}
	for _, s := range sites {
		seen[s.site] = true
		if !lockedRegistrySites[s.site] {
			unlisted = append(unlisted, s.site)
		} else if !s.locked {
			unlocked = append(unlocked, s.site)
		}
	}
	var stale []string
	for s := range lockedRegistrySites {
		if !seen[s] {
			stale = append(stale, s)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	assert.Empty(t, unlisted, "reads e.drivers / e.primary outside the locked sites: take the call's view (e.view()) and thread it through")
	assert.Empty(t, unlocked, "listed as a locked site but takes no e.mu lock")
	assert.Empty(t, stale, "listed but no longer reads the registry: drop it from lockedRegistrySites")
	assert.Empty(t, mapWrites, "writes into a driver map in place: AddDriver publishes a copy (a view's map is read without the lock)")
}

// coreEngineIdents are the names bound to a CoreEngine in fn: its receiver
// and its parameters of type CoreEngine / *CoreEngine.
func coreEngineIdents(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	add := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			typ := f.Type
			if st, ok := typ.(*ast.StarExpr); ok {
				typ = st.X
			}
			if id, ok := typ.(*ast.Ident); ok && id.Name == "CoreEngine" {
				for _, n := range f.Names {
					out[n.Name] = true
				}
			}
		}
	}
	add(fn.Recv)
	add(fn.Type.Params)
	return out
}

func isDriversField(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "drivers"
}
