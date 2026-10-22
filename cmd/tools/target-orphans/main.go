// cmd/tools/target-orphans — the READ-ONLY census of what the target-only
// backends (`sync`, `blomp`) hold that no row names.
//
// Nothing finds a whole-object blob left on a target backend with no row
// naming it: `retired_blobs` collects only what a write queued, the erasure
// sweep walks a tenant only when the account is erased, `chunk_orphans`
// walks chunk blobs on the primary. On 2026-10-22 one such blob sat on Sync
// (a bench race key, 250 KB) and was found by hand. This walks every tenant
// on the backend through the driver's WalkTenant (on Sync the union of
// every bridge), sorts each file by what it is and asks the database who
// names it:
//
//	object   <tenant>_<bucket>/<key or address>  the blobNamedBy rule (a
//	         head row, a version, an open demotion, a Sia promotion, an
//	         active upload) or a pending retired_blobs row
//	parity   <tenant>__parity/<digest>/<etag>/p<j> a vault_parity row with
//	         that shard_prefix and the backend in its legs
//	pack     t-_global/_packs/<aa>/<sha256>.pack   a packs row
//	stripe   <container>%p/…  and  segment  _segments/…  — the stripe_gc /
//	         swift_segment_gc reapers' own; counted, never judged here
//
// Tenants walked: every tenants row, `_global`, `_probe`, -tenants, and on a
// WebDAV backend every `t-*` folder any bridge lists at the root (a deleted
// tenant's leftovers). Blomp's root is not listed: a deleted tenant there
// must be named with -tenants.
//
// It writes nothing, anywhere (the session is read only; no Remove is
// called). Each file that is not named is one JSON line on stdout; the
// summary by backend, kind and verdict goes to stderr. Verdicts: named,
// upload_owned, retired_pending, orphan, orphan_tenant_gone, listed_missing,
// reaper_owned, canary, unclassified. A listing lags writes AND deletes
// (Blomp up to hours, a Sync bridge up to 5 min): every orphan is confirmed
// by the driver's Exists (listed but absent = listed_missing), and a file
// written a moment ago can still be judged before its row commits — read a
// finding twice before acting on it.
//
// First run, 2026-10-22: Sync empty below its root; Blomp 2 orphans of 64 MiB
// — bench PUTs Blomp answered 503 (the client got 503, no row) yet stored —
// and 1 listed_missing. A backend that stores a body and answers an error
// leaks the blob: nothing queues an address whose Put failed.
//
// Usage (on the box, with the service's env):
//
//	set -a; . /opt/vaultaire/configs/.env; set +a
//	./target-orphans -backends sync,blomp > orphans.jsonl
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/database"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

type kind string

const (
	kindObject  kind = "object"
	kindParity  kind = "parity"
	kindPack    kind = "pack"
	kindStripe  kind = "stripe"
	kindSegment kind = "segment"
	kindCanary  kind = "canary"
	kindOther   kind = "other"
)

// The names the server writes (internal/api: parityBucket, newBlobAddress;
// internal/packstore: DefaultContainer; the probe canary's reserved tenant).
const (
	parityBucket    = "_parity"
	packTenant      = "_global"
	packContainer   = "_packs"
	probeTenant     = "_probe"
	segmentsName    = "_segments"
	stripeDirSuffix = "%p"
	blobIDLen       = 26
	blobIDAlphabet  = "0123456789abcdefghjkmnpqrstvwxyz"
)

// item is one walked file and what it is.
type item struct {
	Tenant      string `json:"tenant"`
	Container   string `json:"container"`
	Artifact    string `json:"artifact"`
	Kind        kind   `json:"kind"`
	Bucket      string `json:"bucket,omitempty"`
	Key         string `json:"key,omitempty"`
	ShardPrefix string `json:"shard_prefix,omitempty"`
}

// classify sorts one walked file of tenantID by its container and name.
func classify(tenantID string, o engine.TenantObject) item {
	it := item{Tenant: tenantID, Container: o.Container, Artifact: o.Artifact, Kind: kindOther}
	switch {
	case tenantID == probeTenant:
		it.Kind = kindCanary
	case tenantID == packTenant && o.Container == packContainer:
		it.Kind, it.Key = kindPack, o.Artifact
	case o.Container == segmentsName:
		it.Kind = kindSegment
	case strings.HasSuffix(o.Container, stripeDirSuffix):
		it.Kind = kindStripe
	case o.Container == tenantID+"_"+parityBucket:
		parts := strings.Split(o.Artifact, "/")
		if len(parts) == 3 && strings.HasPrefix(parts[2], "p") {
			if _, err := strconv.Atoi(parts[2][1:]); err == nil {
				it.Kind, it.ShardPrefix = kindParity, parts[0]+"/"+parts[1]
			}
		}
	default:
		bucket, ok := strings.CutPrefix(o.Container, tenantID+"_")
		if ok && bucket != "" {
			it.Kind, it.Bucket, it.Key = kindObject, bucket, keyOf(o.Artifact)
		}
	}
	return it
}

// keyOf is the object key a blob's name belongs to: the name minus a
// newBlobAddress suffix (`@` + 26 characters of its alphabet), else the name
// itself (a blob written at its key before 087).
func keyOf(artifact string) string {
	i := strings.LastIndexByte(artifact, '@')
	if i < 0 || len(artifact)-i-1 != blobIDLen {
		return artifact
	}
	for _, c := range artifact[i+1:] {
		if !strings.ContainsRune(blobIDAlphabet, c) {
			return artifact
		}
	}
	return artifact[:i]
}

var hrefRE = regexp.MustCompile(`<(?:[A-Za-z]+:)?href>([^<]*)</`)

// tenantFolders are the tenant ids of the `t-<id>` folders directly under
// root in a Depth 1 PROPFIND answer.
func tenantFolders(body, root string) []string {
	prefix := "/" + strings.Trim(root, "/") + "/"
	if prefix == "//" {
		prefix = "/"
	}
	var out []string
	for _, m := range hrefRE.FindAllStringSubmatch(body, -1) {
		p := m[1]
		if u, err := url.Parse(p); err == nil && u.Path != "" {
			p = u.Path
		} else if s, err := url.PathUnescape(p); err == nil {
			p = s
		}
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		rest = strings.TrimSuffix(rest, "/")
		if id, ok := strings.CutPrefix(rest, "t-"); ok && id != "" && !strings.Contains(id, "/") {
			out = append(out, id)
		}
	}
	return out
}

// webdavTenants lists the root of every bridge and returns the union of the
// tenant folders (a bridge that cannot list is an error: a tenant only it
// shows would be missed).
func webdavTenants(ctx context.Context, cfg drivers.WebDAVConfig) ([]string, error) {
	bridges := cfg.Bridges
	if len(bridges) == 0 {
		bridges = []drivers.WebDAVBridge{{URL: cfg.URL, Password: cfg.Password}}
	}
	user := cfg.User
	if user == "" {
		user = "sync"
	}
	seen := map[string]struct{}{}
	for i, b := range bridges {
		u := strings.TrimRight(b.URL, "/") + "/" + strings.Trim(cfg.Root, "/") + "/"
		req, err := http.NewRequestWithContext(ctx, "PROPFIND", u, nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(user, b.Password)
		req.Header.Set("Depth", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("bridge %d: list root: %w", i, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("bridge %d: list root: %w", i, err)
		}
		if resp.StatusCode != http.StatusMultiStatus {
			return nil, fmt.Errorf("bridge %d: list root: status %d", i, resp.StatusCode)
		}
		for _, id := range tenantFolders(string(body), cfg.Root) {
			seen[id] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out, nil
}

// verdict asks the database who names it on backend.
func verdict(ctx context.Context, db *sql.DB, backend string, it item) (string, error) {
	var named, upload bool
	switch it.Kind {
	case kindCanary:
		return "canary", nil
	case kindStripe, kindSegment:
		return "reaper_owned", nil
	case kindOther:
		return "unclassified", nil
	case kindPack:
		err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM packs WHERE backend = $1 AND name = $2)`,
			backend, it.Key).Scan(&named)
		if err != nil {
			return "", fmt.Errorf("pack %s: %w", it.Key, err)
		}
	case kindParity:
		err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM vault_parity
			WHERE tenant_id = $1 AND shard_prefix = $2 AND $3 = ANY(legs))`,
			it.Tenant, it.ShardPrefix, backend).Scan(&named)
		if err != nil {
			return "", fmt.Errorf("parity %s: %w", it.ShardPrefix, err)
		}
	case kindObject:
		// internal/api/blob_address.go blobNamedBy, verbatim but for the
		// chunked sentinel: keep the two in step.
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM object_head_cache
			                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
			                  AND COALESCE(storage_key, object_key) = $4 AND NOT is_chunked)
			    OR EXISTS (SELECT 1 FROM object_versions
			                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
			                  AND storage_key = $4 AND NOT is_delete_marker
			                  AND COALESCE(backend_name, '') <> 'chunked')
			    OR ($4 = $3 AND EXISTS (SELECT 1 FROM object_versions
			                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
			                  AND is_latest AND is_delete_marker))
			    OR EXISTS (SELECT 1 FROM smart_demotions
			                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
			                  AND COALESCE(storage_key, object_key) = $4 AND hot_outcome IN ('', 'delete_failed'))
			    OR EXISTS (SELECT 1 FROM sia_promotions
			                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3 AND promoted_key = $4),
			       EXISTS (SELECT 1 FROM multipart_uploads
			                WHERE tenant_id = $1 AND bucket = $2 AND object_key = $3
			                  AND storage_key = $4 AND status = 'active')`,
			it.Tenant, it.Bucket, it.Key, it.Artifact).Scan(&named, &upload)
		if err != nil {
			return "", fmt.Errorf("object %s/%s: %w", it.Container, it.Artifact, err)
		}
		if !named && !upload {
			var retired bool
			if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM retired_blobs
				WHERE backend_id = $1 AND container = $2 AND storage_key = $3)`,
				backend, it.Container, it.Artifact).Scan(&retired); err != nil {
				return "", fmt.Errorf("retired %s/%s: %w", it.Container, it.Artifact, err)
			}
			if retired {
				return "retired_pending", nil
			}
		}
	}
	switch {
	case named:
		return "named", nil
	case upload:
		return "upload_owned", nil
	}
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, it.Tenant).Scan(&exists); err != nil {
		return "", fmt.Errorf("tenant %s: %w", it.Tenant, err)
	}
	if !exists && !strings.HasPrefix(it.Tenant, "_") {
		return "orphan_tenant_gone", nil
	}
	return "orphan", nil
}

func main() {
	backends := flag.String("backends", "sync", "comma-separated target backends to walk: sync, blomp")
	extra := flag.String("tenants", "", "comma-separated tenant ids to walk besides the database's (a deleted tenant on blomp)")
	all := flag.Bool("all", false, "print every file, named ones too")
	timeout := flag.Duration("timeout", 2*time.Hour, "the whole run's deadline")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	db, err := openDB(ctx)
	if err != nil {
		fail("database: %v", err)
	}
	defer func() { _ = db.Close() }()

	baseTenants, err := dbTenants(ctx, db)
	if err != nil {
		fail("tenants: %v", err)
	}
	baseTenants = append(baseTenants, packTenant, probeTenant)
	for _, t := range strings.Split(*extra, ",") {
		if t = strings.TrimSpace(t); t != "" {
			baseTenants = append(baseTenants, t)
		}
	}

	out := json.NewEncoder(os.Stdout)
	counts := map[string]int{}
	failed := false
	for _, name := range strings.Split(*backends, ",") {
		name = strings.TrimSpace(name)
		d, tenants, err := buildDriver(ctx, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			failed = true
			continue
		}
		tenants = uniq(append(tenants, baseTenants...))
		w, ok := d.(engine.TenantWalker)
		if !ok {
			fail("%s does not walk tenants", name)
		}
		for _, tn := range tenants {
			n := 0
			err := w.WalkTenant(common.WithTenantID(ctx, tn), tn, func(o engine.TenantObject) error {
				n++
				it := classify(tn, o)
				v, err := verdict(ctx, db, name, it)
				if err != nil {
					return err
				}
				if v == "orphan" || v == "orphan_tenant_gone" {
					// A listing lags deletes too (Blomp listed a deleted
					// object on 2026-10-22): an orphan must also exist.
					ok, err := d.Exists(common.WithTenantID(ctx, tn), o.Container, o.Artifact)
					if err != nil {
						return fmt.Errorf("exists %s/%s: %w", o.Container, o.Artifact, err)
					}
					if !ok {
						v = "listed_missing"
					}
				}
				counts[name+"\t"+string(it.Kind)+"\t"+v]++
				if *all || (v != "named" && v != "reaper_owned" && v != "canary") {
					return out.Encode(struct {
						Backend string `json:"backend"`
						item
						Verdict string `json:"verdict"`
					}{name, it, v})
				}
				return nil
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: walk %s: %v\n", name, tn, err)
				failed = true
			} else if n > 0 {
				fmt.Fprintf(os.Stderr, "%s: %s: %d files\n", name, tn, n)
			}
		}
	}

	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintln(os.Stderr, "backend\tkind\tverdict\tfiles")
	for _, k := range keys {
		fmt.Fprintf(os.Stderr, "%s\t%d\n", k, counts[k])
	}
	if failed {
		os.Exit(1)
	}
}

// buildDriver builds the backend's driver from the service's env, and on a
// WebDAV backend lists the tenant folders at its root.
func buildDriver(ctx context.Context, name string) (engine.Driver, []string, error) {
	logger := zap.NewNop()
	switch name {
	case "sync":
		cfg, ok, err := drivers.SyncWebDAVConfigFromEnv(os.Getenv)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("SYNC_WEBDAV_* not set")
		}
		tenants, err := webdavTenants(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		d, err := drivers.NewMultiWebDAVDriver(name, cfg, logger)
		return d, tenants, err
	case "blomp":
		cfg, ok, err := drivers.BlompConfigFromEnv(os.Getenv)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("BLOMP_* not set")
		}
		d, err := drivers.NewSwiftDriver(name, cfg, logger)
		return d, nil, err
	}
	return nil, nil, fmt.Errorf("unknown target backend %q (sync, blomp)", name)
}

func dbTenants(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func uniq(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if _, dup := seen[s]; !dup {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func openDB(ctx context.Context) (*sql.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		port, _ := strconv.Atoi(getenv("DB_PORT", "5432"))
		cfg := database.Config{Host: getenv("DB_HOST", "localhost"), Port: port, Database: getenv("DB_NAME", "vaultaire"),
			User: getenv("DB_USER", "viera"), Password: os.Getenv("DB_PASSWORD"), SSLMode: getenv("DB_SSLMODE", "disable")}
		dsn = cfg.DSN()
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one session: the read-only SET applies to every query
	if _, err := db.ExecContext(ctx, `SET default_transaction_read_only = on`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
