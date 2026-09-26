// sql_schema_audit_test.go — Review R9 (R9-01/02/03/09): every static SQL
// literal in the linked packages is PREPAREd against the migrated test
// database, so a query that names a column or table the migrations never
// created fails here instead of in production. sqlmock-based tests cannot
// catch this class of bug — they assert whatever column names the author
// believed in (R9-02 shipped with a green sqlmock test for the wrong column).
//
// What counts as a SQL literal: a string constant (or a concatenation of
// string constants) that starts with SELECT/INSERT/UPDATE/DELETE/WITH and
// mentions FROM/INTO/SET/VALUES/JOIN. Literals that are the format argument of
// a Sprintf/Errorf call, or that are concatenated with non-constant parts, are
// dynamic and skipped. A literal containing `%d`/`%s` that is NOT a format
// argument is prepared as-is — that is exactly the GetUsageHistory bug (R9-03).
package database_test

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/FairForge/vaultaire/internal/testutil"
	"github.com/stretchr/testify/require"

	_ "github.com/lib/pq"
)

var (
	sqlStart = regexp.MustCompile(`(?is)^\s*(SELECT|INSERT|UPDATE|DELETE|WITH)\b`)
	sqlBody  = regexp.MustCompile(`(?i)\b(FROM|INTO|SET|VALUES|JOIN)\b`)
)

// auditRoots are the source trees whose SQL must match the schema. Only
// packages linked into cmd/vaultaire matter, but walking all of internal/ is
// cheaper than maintaining the list; unlinked packages that talk to tables no
// migration creates are excluded below until R11 decides their fate.
var auditRoots = []string{"../../internal", "../../cmd/vaultaire"}

// auditExcludedFiles hold SQL against tables that no migration has ever
// created. They are unreachable today (R9-15): the compliance services are
// wired with a nil DB (server.go), the anomaly reporter has no caller and the
// access-pattern handlers have no route. Remove an entry when its table gains
// a migration.
var auditExcludedFiles = map[string]string{
	"internal/compliance/gdpr.go":      "subject_access_requests / processing_activities / data_inventory never migrated",
	"internal/intelligence/anomaly.go": "access_anomalies never migrated",
	"internal/api/patterns.go":         "access_anomalies never migrated",
}

type sqlSite struct {
	file string
	line int
	sql  string
}

func TestSQLLiteralsMatchSchema(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}

	sites := collectSQLSites(t)
	require.Greater(t, len(sites), 300, "the walker found suspiciously few SQL sites — did the roots move?")

	var failures []string
	for _, s := range sites {
		stmt, err := db.Prepare(s.sql)
		if err == nil {
			_ = stmt.Close()
			continue
		}
		msg := err.Error()
		// Parameter typing is resolved after every table/column reference has
		// been validated, so these mean "schema is fine, types are ambiguous".
		if strings.Contains(msg, "could not determine data type of parameter") ||
			strings.Contains(msg, "inconsistent types deduced for parameter") {
			continue
		}
		first := strings.SplitN(strings.TrimSpace(s.sql), "\n", 2)[0]
		if len(first) > 100 {
			first = first[:100]
		}
		failures = append(failures, s.file+":"+strconv.Itoa(s.line)+"  "+msg+"\n      "+first)
	}
	sort.Strings(failures)
	require.Empty(t, failures, "%d SQL literal(s) do not match the migrated schema:\n%s",
		len(failures), strings.Join(failures, "\n"))
}

func collectSQLSites(t *testing.T) []sqlSite {
	t.Helper()
	var sites []sqlSite
	fset := token.NewFileSet()
	for _, root := range auditRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel := filepath.ToSlash(strings.TrimPrefix(path, "../../"))
			if _, excluded := auditExcludedFiles[rel]; excluded {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return nil // not our concern here; go build covers it
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					// fmt.Sprintf("...", ...) / fmt.Errorf(...): the literal is a format,
					// not a query — skip it but keep walking the other arguments.
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Sprintf" || sel.Sel.Name == "Errorf") && len(x.Args) > 0 {
						for _, a := range x.Args[1:] {
							ast.Inspect(a, func(m ast.Node) bool { return visit(m, rel, fset, &sites) })
						}
						return false
					}
				case *ast.BinaryExpr:
					if x.Op == token.ADD {
						if s, ok := constString(x); ok && isSQL(s) {
							sites = append(sites, sqlSite{rel, fset.Position(x.Pos()).Line, s})
						}
						return false // do not double-count the pieces
					}
				case *ast.BasicLit:
					return visit(x, rel, fset, &sites)
				}
				return true
			})
			return nil
		})
		require.NoError(t, err)
	}
	return sites
}

func visit(n ast.Node, rel string, fset *token.FileSet, sites *[]sqlSite) bool {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return true
	}
	if s, err := strconv.Unquote(lit.Value); err == nil && isSQL(s) {
		*sites = append(*sites, sqlSite{rel, fset.Position(lit.Pos()).Line, s})
	}
	return true
}

func isSQL(s string) bool {
	return sqlStart.MatchString(s) && sqlBody.MatchString(s)
}

// constString folds a tree of string-constant concatenations; any
// non-constant operand makes the whole expression dynamic (ok = false).
func constString(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return constString(x.X)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, okl := constString(x.X)
		r, okr := constString(x.Y)
		return l + r, okl && okr
	default:
		return "", false
	}
}
