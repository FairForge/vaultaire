// Package logstoretest gives a test its own transparency log (RP37-13).
//
// The log on the shared test database is ONE global log: every package's
// tests append to it, and a test that signed a checkpoint there could find
// the newest checkpoint signed by another test's key, or its own run
// "unchanged" because another process signed the same size first — a
// merge-queue ejection for nothing. A test that SIGNS opens its own log
// here: a throwaway database on the same server holding only migrations 103, 105 and 106 (the log, its cosignatures and anchors),
// dropped at the end. A test that only appends keeps the shared one and
// asserts only on its own entries. Imported by tests only (never linked
// into cmd/vaultaire).
package logstoretest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/lib/pq" // the postgres driver

	"github.com/FairForge/vaultaire/internal/testutil"
)

// PrivateLog opens a fresh database holding only the transparency log's
// tables (migrations 103, 105 and 106) and drops it when t ends. It skips the test when
// the test server is unreachable, like every DB-backed test.
func PrivateLog(t testing.TB) *sql.DB {
	t.Helper()
	ctx := context.Background()
	cfg := testutil.DBConfig()
	admin, err := sql.Open("postgres", testutil.DSNFor(cfg))
	if err != nil {
		t.Fatalf("translog test log: %v", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("test database unreachable (%v) — create it with `make test-db`", err)
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("translog test log: %v", err)
	}
	name := "translog_test_" + hex.EncodeToString(b)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		_ = admin.Close()
		t.Fatalf("translog test log: create %s: %v", name, err)
	}
	pcfg := cfg
	pcfg.Database = name
	db, err := sql.Open("postgres", testutil.DSNFor(pcfg))
	if err != nil {
		t.Fatalf("translog test log: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close()
	})
	_, self, _, _ := runtime.Caller(0)
	for _, m := range []string{"103_translog.sql", "105_translog_cosignatures.sql", "106_translog_anchors.sql"} {
		mig := filepath.Join(filepath.Dir(self), "..", "..", "..", "database", "migrations", m)
		sqlText, err := os.ReadFile(mig) // #nosec G304 -- the repository's own migration
		if err != nil {
			t.Fatalf("translog test log: %v", err)
		}
		if _, err := db.ExecContext(ctx, string(sqlText)); err != nil {
			t.Fatalf("translog test log: migration %s: %v", m, err)
		}
	}
	return db
}
