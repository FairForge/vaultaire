package drivers

// WP-R2-3 (R2-09, R7's open item): a driver's write path keeps the error it
// was handed in the chain (%w), so a request body the client did not finish
// — tagged engine.ErrCallerAborted where the API reads it — reaches the
// engine and the handler as the client's, never as the backend's. The
// engine adds its own source check for an SDK that drops the chain
// (sizeTrackingReader); this test keeps OUR code from doing it.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writePath names the functions the test reads: every method or helper of
// a driver's PUT (Put, PutObject, putPart, upload…, stripe…, write…).
func writePath(name string) bool {
	n := strings.ToLower(name)
	for _, w := range []string{"put", "upload", "stripe", "write"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// errorish is an argument that is an error value by its name.
func errorish(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && (id.Name == "err" || strings.HasSuffix(id.Name, "Err") || strings.HasSuffix(id.Name, "err"))
}

func TestDriverWritePaths_WrapTheirErrorsWithW(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !writePath(fn.Name.Name) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "fmt" || sel.Sel.Name != "Errorf" {
					return true
				}
				format, ok := call.Args[0].(*ast.BasicLit)
				if !ok {
					return true
				}
				for _, arg := range call.Args[1:] {
					if !errorish(arg) {
						continue
					}
					checked++
					if !strings.Contains(format.Value, "%w") {
						t.Errorf("%s: %s formats %s without %%w — the body's error (the client's) is lost to the engine",
							fset.Position(call.Pos()), fn.Name.Name, arg.(*ast.Ident).Name)
					}
				}
				return true
			})
		}
	}
	require.Greater(t, checked, 20, "the test reads the write paths it means to")
}
