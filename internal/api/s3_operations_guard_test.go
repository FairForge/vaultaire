package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R5-12 (R5-21): the S3 parser and the permission grants list are ONE
// list. S3Parser.determineOperation may assign S3Request.Operation only an
// auth.Op* constant, the "Unknown" sentinel or opUnsupportedSubresource —
// never a string literal of its own. The grants list used to lag the parser
// by 18 operations because they were two lists.
func TestS3ParserEmitsOnlyKnownOperations(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "s3.go", nil, 0)
	require.NoError(t, err)

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "determineOperation" {
			fn = fd
		}
	}
	require.NotNil(t, fn, "S3Parser.determineOperation is the parser")

	var assigned int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		sel, ok := as.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Operation" {
			return true
		}
		assigned++
		switch rhs := as.Rhs[0].(type) {
		case *ast.SelectorExpr:
			pkg, _ := rhs.X.(*ast.Ident)
			require.NotNil(t, pkg)
			assert.Equal(t, "auth", pkg.Name, "line %d: operation from another package", fset.Position(as.Pos()).Line)
			assert.True(t, auth.ValidPermissions[constValue(t, rhs.Sel.Name)],
				"line %d: auth.%s is not in ValidPermissions", fset.Position(as.Pos()).Line, rhs.Sel.Name)
		case *ast.Ident:
			assert.Equal(t, "opUnsupportedSubresource", rhs.Name, "line %d", fset.Position(as.Pos()).Line)
		case *ast.BasicLit:
			assert.Equal(t, `"Unknown"`, rhs.Value,
				"line %d: a literal operation name — add it to auth.S3Operations and use the constant", fset.Position(as.Pos()).Line)
		default:
			t.Errorf("line %d: unexpected operation expression %T", fset.Position(as.Pos()).Line, rhs)
		}
		return true
	})
	assert.Greater(t, assigned, 40, "the parser assigns every operation here")
}

// constValue maps an auth.Op* identifier to its value: the constants are
// `Op` + the operation name.
func constValue(t *testing.T, ident string) string {
	t.Helper()
	require.True(t, len(ident) > 2 && ident[:2] == "Op", "%s is not an Op constant", ident)
	return ident[2:]
}

// Every operation the parser can emit is grantable, and the grants list
// holds nothing but those operations, `*` and the one privilege.
func TestValidPermissionsIsTheParserList(t *testing.T) {
	want := map[string]bool{"*": true, auth.PermBypassGovernanceRetention: true}
	for _, op := range auth.S3Operations {
		want[op] = true
	}
	assert.Equal(t, want, auth.ValidPermissions)
	// The 18 R5-21 named, by sample.
	for _, op := range []string{"GetBucketAcl", "PutObjectAcl", "GetBucketLocation", "ListObjectVersions",
		"GetBucketLogging", "DeleteBucketInventory", "GetObjectTagging", "DeleteObjectTagging"} {
		assert.True(t, auth.ValidPermissions[op], "%s must be grantable", op)
	}
	assert.False(t, auth.ValidPermissions["Unknown"])
	assert.False(t, auth.ValidPermissions[opUnsupportedSubresource])
}
