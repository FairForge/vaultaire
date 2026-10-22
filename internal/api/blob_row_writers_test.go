package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RP33-1 b (Stage 1a Prompt 27): a commit of a head row or a version row
// with new bytes takes the bucket-mode lock SHARED and re-reads
// ssec_required — in commitBlobWrite. This test pins that every such commit
// IS commitBlobWrite, so a writer added later cannot skip the lock:
//
//   - an `INSERT INTO object_head_cache` is written only by a head-row
//     inserter or inside a blobWrite's `Upsert:` closure, and the inserters
//     are called only from such a closure (or from each other);
//   - a version row with bytes is written only by writeVersionRow, called
//     by the flip and by writeObjectVersion, whose one caller re-asserts the
//     version of a head row already committed (reassertVersionRow);
//   - every other `INSERT INTO object_versions` writes a row without new
//     bytes: a delete marker, or the null version of the current head row.
//
// Non-test Go files under internal/ and cmd/vaultaire are read.

var (
	insertHeadRow    = regexp.MustCompile(`(?i)INSERT\s+INTO\s+object_head_cache\b`)
	insertVersionRow = regexp.MustCompile(`(?i)INSERT\s+INTO\s+object_versions\b`)
)

// headInserters write a head row; each must run inside an Upsert closure.
var headInserters = map[string]bool{
	"upsertWholeObjectHeadRow":    true,
	"upsertWholeObjectHeadRowEnc": true,
	"upsertChunkedObjectHeadRow":  true,
}

// versionInserters may hold an `INSERT INTO object_versions`, and why.
var versionInserters = map[string]string{
	"writeVersionRow":      "the version row of new bytes — called by the flip (and writeObjectVersion)",
	"anchorNullVersionSQL": "the null version of the current head row: bytes already committed",
	"writeDeleteMarker":    "a delete marker: no bytes",
	"deleteSuspended":      "the null delete marker of a Suspended delete: no bytes",
}

// callersOnly: a function may be called only from these.
var callersOnly = map[string]map[string]bool{
	"writeVersionRow":    {"commitBlobWrite": true, "writeObjectVersion": true},
	"writeObjectVersion": {"reassertVersionRow": true},
}

func TestBlobRowWritersOnlyThroughTheFlip(t *testing.T) {
	root := filepath.Join("..", "..")
	seenVersionInserters := map[string]bool{}
	for _, dir := range []string{"internal", "cmd/vaultaire"} {
		require.NoError(t, filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			require.NoError(t, err)
			if !insertHeadRow.Match(b) && !insertVersionRow.Match(b) && !mentionsAny(b, headInserters) && !mentionsAny(b, callersOnly) {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, b, 0)
			require.NoError(t, err)
			checkRowWriters(t, fset, rel, f, seenVersionInserters)
			return nil
		}))
	}
	for name := range versionInserters {
		assert.True(t, seenVersionInserters[name], "%s no longer inserts a version row — remove it from versionInserters", name)
	}
}

func mentionsAny[V any](b []byte, names map[string]V) bool {
	for n := range names {
		if strings.Contains(string(b), n) {
			return true
		}
	}
	return false
}

// checkRowWriters walks one file with the stack of enclosing nodes.
func checkRowWriters(t *testing.T, fset *token.FileSet, rel string, f *ast.File, seenVersionInserters map[string]bool) {
	t.Helper()
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			at := fset.Position(x.Pos())
			if insertHeadRow.MatchString(x.Value) {
				if !inUpsertClosure(stack) && !headInserters[enclosingName(stack)] {
					assert.Failf(t, "a head row written outside the flip",
						"%s:%d inserts into object_head_cache outside a blobWrite Upsert closure — commit it through commitBlobWrite", rel, at.Line)
				}
			}
			if insertVersionRow.MatchString(x.Value) {
				name := enclosingName(stack)
				if _, ok := versionInserters[name]; !ok {
					assert.Failf(t, "a version row written outside the known writers",
						"%s:%d (%s) inserts into object_versions — a row with bytes goes through writeVersionRow inside the flip", rel, at.Line, name)
				}
				seenVersionInserters[name] = true
			}
		case *ast.CallExpr:
			id, ok := x.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			at := fset.Position(x.Pos())
			caller := enclosingName(stack)
			if headInserters[id.Name] && !inUpsertClosure(stack) && !headInserters[caller] {
				assert.Failf(t, "a head-row inserter called outside the flip",
					"%s:%d calls %s from %s, not from a blobWrite Upsert closure", rel, at.Line, id.Name, caller)
			}
			if allowed, ok := callersOnly[id.Name]; ok && !allowed[caller] {
				assert.Failf(t, "a version-row writer called from an unknown place",
					"%s:%d calls %s from %s", rel, at.Line, id.Name, caller)
			}
		}
		return true
	})
}

// enclosingName is the innermost named function or package-level value
// declaration around the node ("" when none).
func enclosingName(stack []ast.Node) string {
	for i := len(stack) - 1; i >= 0; i-- {
		switch x := stack[i].(type) {
		case *ast.FuncDecl:
			return x.Name.Name
		case *ast.ValueSpec:
			if len(x.Names) > 0 {
				return x.Names[0].Name
			}
		}
	}
	return ""
}

// inUpsertClosure: some enclosing function literal is the `Upsert:` value
// of a composite literal (a blobWrite handed to commitBlobWrite).
func inUpsertClosure(stack []ast.Node) bool {
	for i := len(stack) - 1; i > 0; i-- {
		lit, ok := stack[i].(*ast.FuncLit)
		if !ok {
			continue
		}
		if kv, ok := stack[i-1].(*ast.KeyValueExpr); ok && kv.Value == lit {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Upsert" {
				return true
			}
		}
	}
	return false
}
