package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exactSizeReader probes its source for one byte past the size and blocks
// until EOF: safe on a framed backend body, never on a request body. Its
// only user is copyObject (the source of a CopyObject is engine.Get's
// reader). A new user must hold the same invariant — read the comment on
// the type, then add it here (2b.4 E2.5).
func TestExactSizeReader_OnlyUserIsTheCopyPath(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var users []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if cl, ok := n.(*ast.CompositeLit); ok {
					if id, ok := cl.Type.(*ast.Ident); ok && id.Name == "exactSizeReader" {
						users = append(users, name+":"+fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	assert.Equal(t, []string{"s3_copy.go:copyObject"}, users)
}
