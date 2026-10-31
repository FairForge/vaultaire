// internal/usage/lock_order_test.go
//
// The ledger's lock order (ledger_tx.go): a tenant's tenant_floor_quotas
// rows are locked only by a transaction that already holds its
// tenant_quotas row. Every writer and deleter takes tenant_quotas, then the
// floors (standard, then vault), so a path that locks a floor first — or
// locks one without the total — can deadlock against any of them (Postgres
// then aborts one side: a write answers 500 after its backend write, a
// Stripe webhook is retried). Stage 1a Prompt 7, Part 0 (the WP-R10-8
// post-merge lock-order audit).
package usage

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writerHoldsTotal opens a transaction that takes the tenant's total row the
// way every commit does (ApplyLedgerTx's first statement) and returns it,
// with its backend's pid.
func writerHoldsTotal(t *testing.T, db *sql.DB, tenantID string) (*sql.Tx, int) {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec(`UPDATE tenant_quotas SET storage_used_bytes = storage_used_bytes + 1 WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)
	var pid int
	require.NoError(t, tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	return tx, pid
}

// blockedBy reports, within d, whether some backend waits on a lock held by
// pid (packages run in parallel on one database: only this test's writer
// counts).
func blockedBy(db *sql.DB, pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&n)
		if n > 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestLedger_LockOrder_ClearHouseTakesTheTotalBeforeTheFloors(t *testing.T) {
	// A write's commit holds the total and is about to update its floor; the
	// Stripe cancel webhook's ClearHouse runs meanwhile. Before: ClearHouse
	// deleted the floor rows first, then waited for the total — the write
	// then waited for the floor it deleted: deadlock, one side aborted.
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "lock-clear", "free", FreeTierLimits.StorageBytes)
	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: TB}))

	writer, pid := writerHoldsTotal(t, db, tenantID)
	clearErr := make(chan error, 1)
	go func() { clearErr <- m.ClearHouse(ctx, tenantID) }()
	require.True(t, blockedBy(db, pid, 5*time.Second), "ClearHouse waits for the writer")

	_, wErr := writer.Exec(`UPDATE tenant_floor_quotas SET storage_used_bytes = storage_used_bytes + 1 WHERE tenant_id = $1 AND floor = 'standard'`, tenantID)
	if wErr == nil {
		wErr = writer.Commit()
	}

	assert.NoError(t, wErr, "the writer's commit")
	select {
	case err := <-clearErr:
		assert.NoError(t, err, "ClearHouse")
	case <-time.After(10 * time.Second):
		t.Fatal("ClearHouse never finished")
	}
}

func TestLedger_LockOrder_AFloorOnlyChangeTakesTheTotalFirst(t *testing.T) {
	// A ledger change that moves floors and not the total (a write whose
	// reservation predates the floor row; a part settled on another floor)
	// locked the floor rows with no total. The total's lock is what orders
	// every floor writer (floor.go ReserveTx), so it waits for it now.
	db := setupTestDB(t)
	m := NewQuotaManager(db)
	ctx := context.Background()
	tenantID := newTestTenant(t, db, m, "lock-flooronly", "free", FreeTierLimits.StorageBytes)
	require.NoError(t, m.SetHouse(ctx, tenantID, House{StdBytes: TB, VaultBytes: TB}))

	holder, _ := writerHoldsTotal(t, db, tenantID)
	done := make(chan error, 1)
	go func() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx.Rollback() }()
		if err := ApplyLedgerTx(ctx, tx, tenantID, LedgerChange{Floors: map[string]int64{FloorStandard: 10, FloorVault: -10}}); err != nil {
			done <- err
			return
		}
		done <- tx.Commit()
	}()

	select {
	case err := <-done:
		t.Fatalf("the floor-only change committed while another transaction held the total (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, holder.Rollback())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the floor-only change never finished")
	}
}

// The structure half: every statement in the repository that writes or
// locks tenant_floor_quotas sits in a function (or a package-level rule
// list) where a statement that locks tenant_quotas comes first.

var (
	floorLockRE = regexp.MustCompile(`(?is)(UPDATE\s+tenant_floor_quotas|DELETE\s+FROM\s+tenant_floor_quotas|INSERT\s+INTO\s+tenant_floor_quotas|FROM\s+tenant_floor_quotas\b[^;]*FOR\s+(UPDATE|SHARE))`)
	totalLockRE = regexp.MustCompile(`(?is)(UPDATE\s+tenant_quotas\b|DELETE\s+FROM\s+tenant_quotas\b|INSERT\s+INTO\s+tenant_quotas\b|FROM\s+tenant_quotas\b[^;]*FOR\s+(UPDATE|SHARE))`)
)

// floorLockAllowed names the functions that lock a floor row with no total
// lock before it in their own body, and why that is safe.
var floorLockAllowed = map[string]string{
	// WP-PLAT-1: the total's lock is taken by lockPool (pool.go) — the
	// pool owner's row, then the tenant's own — before the floor statement.
	"usage.ReserveTxFor": "the tenant_quotas lock is lockPool's call, before the floor",
	// RP82-1: the pool's floor helpers are called only after lockPool /
	// lockPoolOwner (the owner's total, then the child's) — the order
	// TestLedger_LockOrder_PoolOwnerFirst_Structure pins in their callers.
	"usage.lockPoolFloor":       "called by ReserveTx / ApplyLedgerTx after lockPool / lockPoolOwner",
	"usage.ensureChildFloorRow": "called by ReserveTx after lockPool and lockPoolFloor",
}

// TestLedger_LockOrder_PoolOwnerFirst_Structure pins the pool's order
// (RP82-1) in the two functions that check a pooled floor: the owner's
// total (lockPool / lockPoolOwner, which then takes the child's), then the
// owner's floor (lockPoolFloor), then the tenant's own floor row (the
// UPDATE that charges it) — owner total → child total → owner floor →
// child floor.
func TestLedger_LockOrder_PoolOwnerFirst_Structure(t *testing.T) {
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, name := range []string{"floor.go", "ledger_tx.go"} {
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || (fd.Name.Name != "ReserveTxFor" && fd.Name.Name != "applyTenantLedgerTx") {
				continue
			}
			found[fd.Name.Name] = true
			var total, ownerFloor, ownFloor token.Pos
			ast.Inspect(fd, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok {
						switch id.Name {
						case "lockPool", "lockPoolOwner":
							if total == 0 {
								total = x.Pos()
							}
						case "lockPoolFloor":
							if ownerFloor == 0 {
								ownerFloor = x.Pos()
							}
						}
					}
				case *ast.BasicLit:
					if s, err := strconv.Unquote(x.Value); err == nil && ownFloor == 0 &&
						regexp.MustCompile(`(?is)UPDATE\s+tenant_floor_quotas`).MatchString(s) {
						ownFloor = x.Pos()
					}
				}
				return true
			})
			require.NotZero(t, total, "%s locks the pool owner", fd.Name.Name)
			require.NotZero(t, ownerFloor, "%s locks the owner's floor", fd.Name.Name)
			require.NotZero(t, ownFloor, "%s charges the tenant's own floor row after the owner's", fd.Name.Name)
			assert.Less(t, int(total), int(ownerFloor), "%s: the owner's total before the owner's floor", fd.Name.Name)
			assert.Less(t, int(ownerFloor), int(ownFloor), "%s: the owner's floor before the tenant's own", fd.Name.Name)
		}
	}
	// WP-KEY-1: the bodies are ReserveTxFor (ReserveTx charges no holder)
	// and ApplyLedgerTx's tenant half.
	assert.True(t, found["ReserveTxFor"] && found["applyTenantLedgerTx"], "both functions were found")
}

func TestLedger_LockOrder_TotalBeforeFloors_Structure(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var violations []string
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", ".git", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		// One scope per function, and one per package-level declaration (a
		// rule list such as account.Deleted runs its statements in order).
		check := func(name string, node ast.Node) {
			var sawTotal bool
			ast.Inspect(node, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				if totalLockRE.MatchString(s) {
					sawTotal = true
				}
				if floorLockRE.MatchString(s) {
					checked++
					if !sawTotal && floorLockAllowed[name] == "" {
						violations = append(violations, fset.Position(lit.Pos()).String()+" in "+name)
					}
				}
				return true
			})
		}
		for _, decl := range file.Decls {
			switch dd := decl.(type) {
			case *ast.FuncDecl:
				check(file.Name.Name+"."+dd.Name.Name, dd)
			case *ast.GenDecl:
				check(file.Name.Name+" package-level declaration", dd)
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Greater(t, checked, 5, "the walk found the floor statements")
	assert.Empty(t, violations, "a tenant_floor_quotas lock with no tenant_quotas lock before it (lock order: total, then floors)")
}
