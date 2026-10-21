package account

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-PAY-7: the tenants billing columns migration 100 drops have no reader
// or writer left — no SQL literal of the product (non-test Go under
// internal/ and cmd/) names one. Two-slot safety: the build that runs while
// the drop migrates must not touch them.
var droppedTenantsColumns = regexp.MustCompile(`\b(stripe_subscription_id|subscription_status|house_period|deletion_stripe_cancelled_at)\b`)

// droppedColumnsExempt are the files allowed to name them (none: the
// pre-097 backfill went before the drop, so this build runs on either schema).
var droppedColumnsExempt = map[string]bool{}

func TestNoSQLNamesTheDroppedTenantsColumns(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			if droppedColumnsExempt[filepath.ToSlash(rel)] {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				if s, err := strconv.Unquote(lit.Value); err == nil && droppedTenantsColumns.MatchString(s) {
					assert.Fail(t, "a dropped tenants column is still named", "%s: %q", rel, s)
				}
				return true
			})
			return nil
		})
		require.NoError(t, err)
	}
}
