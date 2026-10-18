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
//
// Every mention of the name counts — a composite literal, new(…), a
// conversion, a package-level var — except the type's own declaration and
// its methods (Prompt 2b.5 F3.9: the pin matched composite literals only,
// and `x := new(exactSizeReader)` in a scratch file passed it).
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
		users = append(users, exactSizeReaderUsers(name, f)...)
	}
	assert.Equal(t, []string{"s3_copy.go:copyObject", "s3_upload_part_copy.go:uploadPartCopy"}, users)
}

// exactSizeReaderUsers names "file:func" (or "file:package-level") for every
// mention of exactSizeReader outside its own type declaration and methods.
func exactSizeReaderUsers(file string, f *ast.File) []string {
	const typ = "exactSizeReader"
	var users []string
	seen := map[string]bool{}
	add := func(where string) {
		if !seen[where] {
			seen[where] = true
			users = append(users, file+":"+where)
		}
	}
	mentions := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == typ {
				found = true
			}
			return !found
		})
		return found
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) == 1 && mentions(d.Recv.List[0].Type) {
				continue // a method of the type
			}
			if mentions(d) {
				add(d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == typ {
					continue
				}
				if mentions(spec) {
					add("package-level")
				}
			}
		}
	}
	return users
}

// The pin catches every shape of a new user.
func TestExactSizeReader_PinSeesEveryShape(t *testing.T) {
	for _, src := range []string{
		"package api\nfunc a() { _ = &exactSizeReader{} }",
		"package api\nfunc a() { x := new(exactSizeReader); _ = x }",
		"package api\nfunc a() { var x *exactSizeReader; _ = x }",
		"package api\nvar x = new(exactSizeReader)",
		"package api\nfunc a(r io.Reader) { _ = (*exactSizeReader)(nil) }",
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "scratch.go", src, 0)
		require.NoError(t, err, src)
		assert.NotEmpty(t, exactSizeReaderUsers("scratch.go", f), src)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "own.go",
		"package api\ntype exactSizeReader struct{}\nfunc (r *exactSizeReader) Read(p []byte) (int, error) { return 0, nil }", 0)
	require.NoError(t, err)
	assert.Empty(t, exactSizeReaderUsers("own.go", f), "the type's own declaration and methods are not users")
}
