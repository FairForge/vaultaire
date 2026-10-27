package api

// WP-R10-8 Part A: every release in the transaction that makes it true
// (R10-17, R10-43). The displaced bytes of an overwrite, the bytes of a
// delete and the settle of a reservation are written to tenant_quotas and
// tenant_floor_quotas before the head/version transaction commits — never
// after it, on a detached 5 s context that a crash or a lock wait could
// lose. "Before" = these tests on origin/main (49eed72's parent chain).

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FairForge/vaultaire/internal/crypto"
	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// holdQuotaRow locks the tenant's tenant_quotas row from another
// transaction for d (a long lock queue: a migration's ALTER, a reconcile).
func holdQuotaRow(t *testing.T, db *sql.DB, tenantID string, d time.Duration) (released <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	tx, err := db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT 1 FROM tenant_quotas WHERE tenant_id = $1 FOR UPDATE`, tenantID)
	require.NoError(t, err)
	go func() {
		time.Sleep(d)
		_ = tx.Rollback()
		close(done)
	}()
	return done
}

// ledgerAgrees asserts the incremental ledgers equal what the reconcile
// would write — read, never run.
func (f *quotaAccountingFixture) ledgerAgrees(t *testing.T, step string) {
	t.Helper()
	total, floors, err := f.qm.BilledBytes(context.Background(), f.tenantID)
	require.NoError(t, err)
	require.Equal(t, total, f.used(t), "%s: tenant_quotas", step)
	rows, err := f.qm.GetFloors(context.Background(), f.tenantID)
	require.NoError(t, err)
	for _, r := range rows {
		require.Equal(t, floors[r.Floor], r.UsedBytes, "%s: tenant_floor_quotas[%s]", step, r.Floor)
	}
}

func TestLedger_DeleteBehindALockedQuotaRow_ReleasesWithTheRow(t *testing.T) {
	// R10-43 on a plain DELETE. Before: passes — the release ran after the
	// row's commit on postCommit's bound, which outlasts this lock; the
	// window there was a crash between the two transactions (the structure
	// test is its guard). After: one transaction.
	f := setupQuotaAccountingFixture(t, 1<<30)
	require.Equal(t, http.StatusOK, f.put(t, "k", testBytes(3000)))
	var released <-chan struct{}
	deleteBeforeRowHook = func(_, _, _ string) {
		deleteBeforeRowHook = func(_, _, _ string) {}
		released = holdQuotaRow(t, f.db, f.tenantID, 5500*time.Millisecond)
	}
	t.Cleanup(func() { deleteBeforeRowHook = func(_, _, _ string) {} })

	code := f.del(t, "k")

	<-released
	assert.Equal(t, http.StatusNoContent, code)
	f.ledgerAgrees(t, "after the delete")
	assert.Zero(t, f.used(t))
}

func TestLedger_VersionDeleteBehindALockedQuotaRow_ReleasesWithTheRow(t *testing.T) {
	// R10-43 on DELETE ?versionId (releaseDetached: a 5 s detached context
	// after the version transaction committed). Before: the version row and
	// its blob went, the release timed out behind the lock — the deleted
	// version's 3000 bytes billed until a reconcile.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.versioning(t, "Enabled")
	code, v1 := f.putVersion(t, "k", 3000)
	require.Equal(t, http.StatusOK, code)
	code, _ = f.putVersion(t, "k", 5000)
	require.Equal(t, http.StatusOK, code)
	released := holdQuotaRow(t, f.db, f.tenantID, 5500*time.Millisecond)

	f.delVersion(t, "k", v1)

	<-released
	f.ledgerAgrees(t, "after the version delete")
	assert.Equal(t, int64(5000), f.used(t))
}

func TestLedger_OverwriteBehindALockedQuotaRow_SettlesWithTheRow(t *testing.T) {
	// R10-17. Before: the flip committed, the release of the displaced
	// 5000 bytes timed out behind the lock: 8000 billed for a 3000-byte
	// object.
	f := setupQuotaAccountingFixture(t, 1<<30)
	require.Equal(t, http.StatusOK, f.put(t, "k", testBytes(5000)))
	var released <-chan struct{}
	headFlipHook = func(_, _, _ string) error {
		headFlipHook = func(_, _, _ string) error { return nil }
		released = holdQuotaRow(t, f.db, f.tenantID, 5500*time.Millisecond)
		return nil
	}
	t.Cleanup(func() { headFlipHook = func(_, _, _ string) error { return nil } })

	code := f.put(t, "k", testBytes(3000))

	<-released
	assert.Equal(t, http.StatusOK, code)
	f.ledgerAgrees(t, "after the overwrite")
	assert.Equal(t, int64(3000), f.used(t))
}

func TestLedger_ConcurrentOverwriteDeleteReconcile_TwoKeys_NoDeadlock(t *testing.T) {
	// The lock order (blob_flip.go, usage/ledger_tx.go): key → head row →
	// versions → upload → tenant_quotas → floors; the reconcile and the
	// reservation take the quota rows alone. Overwrites and deletes of two
	// keys of one tenant, under versioning, with reconciles alongside: no
	// deadlock (Postgres would abort one with 40P01 → a 5xx) and no hang.
	// Then the same storm without reconciles — a reconcile during writes
	// erases in-flight reservations by design (handleQuotaReconcile: run it
	// quiesced) — and the ledger equals the reconcile's sums.
	f := setupQuotaAccountingFixture(t, 1<<30)
	f.versioning(t, "Enabled")
	f.house(t, 1<<30, 1<<30)
	storm := func(withReconcile bool) {
		var wg sync.WaitGroup
		errs := make(chan string, 1000)
		deadline := time.Now().Add(3 * time.Second)
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(int64(g)))
				for time.Now().Before(deadline) {
					key := []string{"a", "b"}[rng.Intn(2)]
					var code int
					switch op := rng.Intn(4); {
					case op <= 1:
						code = f.put(t, key, testBytes(1000+rng.Intn(4000)))
					case op == 2:
						code = f.del(t, key)
					case withReconcile:
						if err := f.qm.ReconcileTenantStorageUsage(context.Background(), f.tenantID); err != nil {
							errs <- err.Error()
						}
						continue
					default:
						code = f.put(t, key, testBytes(500))
					}
					if code >= 500 {
						errs <- fmt.Sprintf("op answered %d", code)
					}
				}
			}(g)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Fatal("the storm hung: a lock cycle")
		}
		close(errs)
		for e := range errs {
			t.Error(e)
		}
	}
	storm(true)
	require.NoError(t, f.qm.ReconcileTenantStorageUsage(context.Background(), f.tenantID))
	storm(false)
	f.ledgerAgrees(t, "after the storm")
}

// randomOps drives one tenant through a random sequence of every writer and
// deleter and asserts, after each step, that tenant_quotas and every
// tenant_floor_quotas row equal what ReconcileTenantStorageUsage would
// write — without running it.
func TestLedger_RandomSequence_EqualsTheReconcileAtEveryStep(t *testing.T) {
	// WP-VM-1: every run also writes chunked objects (anything over 3000
	// bytes), runs the collector (retired manifests and blobs released),
	// and "→Enabled→Suspended" changes the versioning state mid-run, so
	// objects chunked before versioning become null versions (PM-3).
	// Prompt 8 Part 0: every run also has a Suspended bucket ("susp-bucket")
	// it writes chunked into and deletes from (the R61-5 Suspended delete of
	// a chunked null version), and the manifest copy — a chunked source
	// copied where a PUT would chunk — as an op of its own.
	for _, status := range []string{"", "Enabled", "Suspended", "→Enabled→Suspended"} {
		t.Run("versioning="+status, func(t *testing.T) {
			seed := time.Now().UnixNano()
			t.Logf("seed %d", seed)
			rng := rand.New(rand.NewSource(seed))
			f := setupQuotaAccountingFixture(t, 1<<30)
			f.server.gci = crypto.NewGlobalContentIndex(f.db)
			f.server.chunkingThreshold = 3000
			// Chunked writes in all three versioning states (WP-VM-1 Part D):
			// the tenant's versioned_chunking row.
			fl := flags.New(f.db, zap.NewNop())
			registerFlags(fl)
			require.NoError(t, fl.Set(context.Background(), flagVersionedChunking, f.tenantID, true, "test"))
			t.Cleanup(func() {
				_, _ = f.db.Exec(`DELETE FROM feature_flags WHERE flag_key = $1 AND tenant_id = $2`, flagVersionedChunking, f.tenantID)
			})
			f.server.flags = fl
			f.house(t, 1<<30, 1<<30)
			if status == "Enabled" || status == "Suspended" {
				f.versioning(t, status)
			} else {
				f.unversionedBucket(t)
			}
			_, err := f.db.Exec(`INSERT INTO buckets (tenant_id, name, tier_preference) VALUES ($1, 'attic-bucket', 'archive')
				ON CONFLICT (tenant_id, name) DO UPDATE SET tier_preference = 'archive'`, f.tenantID)
			require.NoError(t, err)
			_, err = f.db.Exec(`INSERT INTO buckets (tenant_id, name, visibility, versioning_status) VALUES ($1, 'susp-bucket', 'private', 'Suspended')
				ON CONFLICT (tenant_id, name) DO UPDATE SET versioning_status = 'Suspended'`, f.tenantID)
			require.NoError(t, err)
			// The body's first bytes are the tenant's: the plaintext content
			// index is shared by every test on the database.
			body := func(n int) []byte {
				b := testBytes(n)
				copy(b, f.tenantID)
				return b
			}
			keys := []string{"k0", "k1", "k2"}
			for step := 0; step < 60; step++ {
				if status == "→Enabled→Suspended" && (step == 20 || step == 40) {
					next := map[int]string{20: "Enabled", 40: "Suspended"}[step]
					f.versioning(t, next)
				}
				key := keys[rng.Intn(len(keys))]
				var op string
				switch rng.Intn(13) {
				case 0, 1:
					op = "PUT " + key
					require.Equal(t, http.StatusOK, f.put(t, key, body(rng.Intn(6000))), op)
				case 8:
					op = "PUT chunked " + key
					require.Equal(t, http.StatusOK, f.put(t, key, body(3001+rng.Intn(6000))), op)
				case 9:
					op = "collector"
					releaseRetiredManifests(t, f.db, f.server.engine, f.server.gci, f.tenantID)
				case 2:
					op = "PUT GLACIER " + key
					require.Equal(t, http.StatusOK, f.putClass(t, "test-bucket", key, "GLACIER", body(1+rng.Intn(6000))), op)
				case 3:
					op = "DELETE " + key
					require.Equal(t, http.StatusNoContent, f.del(t, key), op)
				case 4:
					op = "DELETE ?versionId " + key
					var vid string
					err := f.db.QueryRow(`SELECT version_id FROM object_versions WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2
						ORDER BY random() LIMIT 1`, f.tenantID, key).Scan(&vid)
					if err == sql.ErrNoRows {
						continue
					}
					require.NoError(t, err)
					w := f.getV(t, "DELETE", key, vid)
					require.Equal(t, http.StatusNoContent, w.Code, op+": "+w.Body.String())
				case 5:
					op = "Complete " + key
					f.complete(t, key)
				case 6:
					src := keys[rng.Intn(len(keys))]
					if src == key { // a self-copy of a chunked object without REPLACE is InvalidRequest (as AWS)
						src = keys[(rng.Intn(len(keys)-1)+1+indexOf(keys, key))%len(keys)]
					}
					op = "COPY " + src + " → " + key
					if f.headRows(t, src) == 0 {
						continue
					}
					w := f.copyObject(t, src, key)
					require.Equal(t, http.StatusOK, w.Code, op+": "+w.Body.String())
				case 7:
					op = "DeleteObjects " + key
					f.batchDelete(t, batchEntry{key: key})
				case 10:
					op = "PUT chunked susp-bucket/" + key
					require.Equal(t, http.StatusOK, f.putClass(t, "susp-bucket", key, "", body(3001+rng.Intn(6000))), op)
				case 11:
					op = "Suspended DELETE susp-bucket/" + key
					w := doS3Request(f.server, f.tenant, "DELETE", "/susp-bucket/"+key, nil)
					require.Equal(t, http.StatusNoContent, w.Code, op+": "+w.Body.String())
				case 12:
					dst := keys[(indexOf(keys, key)+1+rng.Intn(len(keys)-1))%len(keys)]
					op = "COPY manifest " + key + " → " + dst
					require.Equal(t, http.StatusOK, f.put(t, key, body(3001+rng.Intn(6000))), op+": the chunked source")
					w := f.copyObject(t, key, dst)
					require.Equal(t, http.StatusOK, w.Code, op+": "+w.Body.String())
					var chunked bool
					require.NoError(t, f.db.QueryRow(`SELECT is_chunked FROM object_head_cache
						WHERE tenant_id = $1 AND bucket = 'test-bucket' AND object_key = $2`, f.tenantID, dst).Scan(&chunked))
					require.True(t, chunked, op+": a manifest copy")
				}
				f.ledgerAgrees(t, fmt.Sprintf("step %d (%s)", step, op))
			}
		})
	}
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

func (f *quotaAccountingFixture) copyObject(t *testing.T, src, dst string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("PUT", "/test-bucket/"+dst, nil)
	r.Header.Set("x-amz-copy-source", "/test-bucket/"+src)
	r = r.WithContext(s3Ctx(r.Context(), f.tenant))
	w := httptest.NewRecorder()
	f.server.handleS3Request(w, r)
	return w
}

// The structure test: no quota statement runs after a commit. Every
// release of displaced or deleted bytes goes through displacedRow.releases
// inside the transaction (writeBill.settleTx, releasesTx via
// deleteHeadRowTx / deleteVersion, and the dry-run prediction that reads the
// same rule); the helpers that released after COMMIT are gone and must not
// come back.
func TestLedger_NoQuotaStatementAfterACommit(t *testing.T) {
	forbidden := map[string]bool{"settlePutQuota": true, "releaseDisplacedOn": true, "releaseDetached": true}
	allowedReleasesCallers := map[string]bool{"settleTx": true, "deleteHeadRowTx": true, "predictRelease": true, "deleteSuspended": true}
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if forbidden[fn.Name.Name] {
				t.Errorf("%s: %s is back — a release after the commit (WP-R10-8)", fset.Position(fn.Pos()), fn.Name.Name)
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "releases" && len(call.Args) == 0 && !allowedReleasesCallers[fn.Name.Name] {
					t.Errorf("%s: %s reads displacedRow.releases outside the commit's settle — release in the transaction (writeBill / releasesTx)",
						fset.Position(call.Pos()), fn.Name.Name)
				}
				if id, ok := call.Fun.(*ast.Ident); ok && forbidden[id.Name] {
					t.Errorf("%s: %s calls %s", fset.Position(call.Pos()), fn.Name.Name, id.Name)
				}
				return true
			})
		}
	}
}

// ledgerCalls change tenant_quotas / tenant_floor_quotas inside the caller's
// transaction; statements are the database calls a transaction makes.
var (
	ledgerCalls = map[string]bool{"ApplyLedgerTx": true, "ReserveTx": true, "releasesTx": true, "settleTx": true, "giveBackPart": true,
		"deleteHeadRowTx": true} // releases inside, last
	statementCalls = map[string]bool{"ExecContext": true, "QueryContext": true, "QueryRowContext": true, "Exec": true, "Query": true, "QueryRow": true,
		"retireBlob": true, "install": true}
)

// quotaLastAllowed names the functions where a statement follows a ledger
// call, and why that is safe.
var quotaLastAllowed = map[string]string{
	// A part that measured more than it reserved is checked before its
	// install (the rename replaces the part number's previous file): the
	// quota row is held through the rename and the part row's insert. Every
	// other locker of the upload row takes it before the quota row.
	"settleUploadPart":     "a part measured past its reservation (R108-8) is refused before the install",
	"settleUploadPartWith": "the same install, for a staged and an edge part (WP-EDGE-1)",
	"settleEdgeCodecPart":  "the same install, for a codec part (WP-EDGE-2): the quota row is held through the part row's insert, as settleUploadPartWith holds it",
}

// TestLedger_QuotaStatementIsLast (Stage 1a Prompt 7, Part 0): the ledger is
// the last thing a transaction changes before COMMIT, so the tenant's quota
// row is held from its UPDATE to the COMMIT only (ledger_tx.go). A database
// statement after a ledger call in the same function holds the tenant's one
// hot row through it — and one that can wait on another lock is a deadlock
// waiting for its second party.
func TestLedger_QuotaStatementIsLast(t *testing.T) {
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
			if !ok || fn.Body == nil {
				continue
			}
			var firstLedger, ledgerEnd token.Pos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := ""
				switch f := call.Fun.(type) {
				case *ast.Ident:
					callee = f.Name
				case *ast.SelectorExpr:
					callee = f.Sel.Name
				}
				switch {
				case ledgerCalls[callee]:
					checked++
					if firstLedger == token.NoPos {
						firstLedger, ledgerEnd = call.Pos(), call.End() // its own arguments run before it
					}
				case statementCalls[callee] && firstLedger != token.NoPos && call.Pos() > ledgerEnd && quotaLastAllowed[fn.Name.Name] == "":
					t.Errorf("%s: %s runs %s after its ledger call at %s — the ledger must be the transaction's last statement",
						fset.Position(call.Pos()), fn.Name.Name, callee, fset.Position(firstLedger))
				}
				return true
			})
		}
	}
	assert.Greater(t, checked, 5, "the walk found the ledger calls")
}
