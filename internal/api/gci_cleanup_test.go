package api

import (
	"database/sql"
	"testing"
)

// chunkPair identifies one global_content_index row.
type chunkPair struct{ scope, hash string }

// tenantChunkPairs lists the GCI rows a tenant's manifests reference right now.
func tenantChunkPairs(t *testing.T, db *sql.DB, tenantID string) []chunkPair {
	t.Helper()
	rows, err := db.Query(`SELECT DISTINCT dedup_scope, plaintext_hash FROM tenant_chunk_refs WHERE tenant_id::text = $1`, tenantID)
	if err != nil {
		t.Fatalf("list tenant chunk refs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []chunkPair
	for rows.Next() {
		var p chunkPair
		if err := rows.Scan(&p.scope, &p.hash); err != nil {
			t.Fatalf("scan chunk ref: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chunk refs: %v", err)
	}
	return out
}

// gciRowsFor counts the GCI rows among pairs that still exist.
func gciRowsFor(t *testing.T, db *sql.DB, pairs []chunkPair) int {
	t.Helper()
	n := 0
	for _, p := range pairs {
		var c int
		if err := db.QueryRow(`SELECT COUNT(*) FROM global_content_index WHERE dedup_scope = $1 AND plaintext_hash = $2`,
			p.scope, p.hash).Scan(&c); err != nil {
			t.Fatalf("count gci row: %v", err)
		}
		n += c
	}
	return n
}

// backdateGCIRows makes the given rows eligible for the GC sweep (grace expired)
// without touching any other test's rows.
func backdateGCIRows(t *testing.T, db *sql.DB, pairs []chunkPair) {
	t.Helper()
	for _, p := range pairs {
		if _, err := db.Exec(`UPDATE global_content_index
			SET marked_at = NOW() - INTERVAL '1 day', last_accessed_at = NOW() - INTERVAL '1 day'
			WHERE dedup_scope = $1 AND plaintext_hash = $2`, p.scope, p.hash); err != nil {
			t.Fatalf("backdate gci row: %v", err)
		}
	}
}

// cleanupTenantChunkRows removes a test tenant's chunk manifests and then the
// GCI rows those manifests referenced that no other tenant references.
//
// Review R15 (the last shared-DB flake): the fixtures used to run
// `DELETE FROM global_content_index WHERE NOT EXISTS (… tenant_chunk_refs …)`
// table-wide, which also deleted rows internal/crypto's GCI suite had just
// inserted with no manifest yet (TestGCI_RefCounting: `decrement_chunk_ref`
// returned NULL). Cleanup is now bounded to the hashes this tenant wrote.
// Unencrypted chunks live in the shared `_global` scope, so a row is removed
// only when no reference to it remains anywhere.
func cleanupTenantChunkRows(db *sql.DB, tenantID, scope string) {
	rows, err := db.Query(`SELECT DISTINCT dedup_scope, plaintext_hash FROM tenant_chunk_refs WHERE tenant_id::text = $1`, tenantID)
	var pairs []chunkPair
	if err == nil {
		for rows.Next() {
			var p chunkPair
			if rows.Scan(&p.scope, &p.hash) == nil {
				pairs = append(pairs, p)
			}
		}
		_ = rows.Err()
		_ = rows.Close()
	}
	_, _ = db.Exec(`DELETE FROM tenant_chunk_refs WHERE tenant_id::text = $1`, tenantID)
	_, _ = db.Exec(`DELETE FROM object_metadata WHERE tenant_id::text = $1`, tenantID)
	for _, p := range pairs {
		_, _ = db.Exec(`DELETE FROM global_content_index g
			WHERE g.dedup_scope = $1 AND g.plaintext_hash = $2
			  AND NOT EXISTS (SELECT 1 FROM tenant_chunk_refs r
				WHERE r.dedup_scope = g.dedup_scope AND r.plaintext_hash = g.plaintext_hash)`, p.scope, p.hash)
	}
	if scope != "" && scope != "_global" {
		_, _ = db.Exec(`DELETE FROM global_content_index WHERE dedup_scope = $1`, scope)
	}
}
