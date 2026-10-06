// cmd/tools/routing-truth — the READ-ONLY plan for reconciling head rows
// whose bytes are not where object_head_cache.backend_name says (WP-R7-5).
//
// It connects to the database (DB_* or DATABASE_URL, the server's own
// variables) and prints, per recorded backend and tenant, the rows and what
// the operator may do with them. With the backend's credentials in the
// environment it also SAMPLES the rows through the real driver — a signed
// HEAD on iDrive, a stat on DATA_PATH, a Graph lookup on the permafrost
// fleet — so the plan says whether the bytes are still there before anyone
// renames or deletes a row. It writes nothing, anywhere. The decisions and
// the commands are in docs/reviews/WP-R7-5.md; this prints the numbers for
// them.
//
// Usage (on the box, as the service user, with the service's env):
//
//	set -a; . /opt/vaultaire/configs/.env; set +a
//	go run ./cmd/tools/routing-truth            # or the cross-compiled binary
//	go run ./cmd/tools/routing-truth -sample 50 # HEADs per (backend, tenant) class
//	go run ./cmd/tools/routing-truth -no-probe  # the database only
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
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

// registeredNames are the registration keys cmd/vaultaire/main.go uses; a
// row on any other name is served by nobody.
var registeredNames = map[string]bool{"local": true, "idrive": true, "lyve": true, "geyser": true, "r2": true, "permafrost": true, "s3": true, "quotaless": true, "wasabi": true, "sync": true}

type class struct {
	backend, tenant, tenantName string
	rows                        int64
	bytes                       int64
	minDate, maxDate            string
	buckets                     int64
}

type row struct{ tenant, bucket, key string }

func main() {
	sample := flag.Int("sample", 20, "rows per (backend, tenant) to check through the driver")
	noProbe := flag.Bool("no-probe", false, "database only: no driver call")
	onlyBackend := flag.String("backend", "", "probe only this recorded backend ('' for the NULL rows is not probeable; others are still listed)")
	onlyTenant := flag.String("tenant", "", "probe only this tenant's rows")
	one := flag.String("one", "", "probe ONE object through a backend's driver and exit: backend:tenant/bucket/key (HEAD, then a 1-byte GET — iDrive answers 403 to a HEAD a GET of the same key serves)")
	list := flag.String("list", "", "list what a backend holds for a tenant's bucket and exit: backend:tenant/bucket (first 40 keys)")
	flag.Parse()
	logger := zap.NewNop()

	db, err := openDB()
	if err != nil {
		fmt.Fprintln(os.Stderr, "database:", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := db.ExecContext(ctx, `SET default_transaction_read_only = on`); err != nil {
		fmt.Fprintln(os.Stderr, "read-only session:", err)
		os.Exit(1)
	}

	fmt.Printf("routing truth — read-only plan, %s\n\n", time.Now().UTC().Format(time.RFC3339))

	if *one != "" || *list != "" {
		probeOne(ctx, logger, *one, *list)
		return
	}

	classes, err := loadClasses(ctx, db)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range []string{"smart_demotions", "object_versions"} {
		if err := printOtherTable(ctx, db, name); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}

	// The drivers the environment allows us to ask. Each is built the way
	// main.go builds it; no server wiring is duplicated beyond that.
	probes := map[string]engine.Driver{}
	if !*noProbe {
		probes = buildProbes(logger)
	}

	fmt.Println("== head rows per recorded backend and tenant")
	fmt.Printf("%-12s %-28s %-16s %8s %12s %-10s %-10s %s\n", "backend", "tenant", "name", "rows", "bytes", "first", "last", "probe (sample)")
	plan := map[string][]string{}
	for _, c := range classes {
		verdict := "-"
		switch d, ok := probes[c.backend]; {
		case c.backend == "":
			verdict = "no backend recorded: POST /api/v1/admin/routing-truth/resolve-null"
		case (*onlyBackend != "" && c.backend != *onlyBackend) || (*onlyTenant != "" && c.tenant != *onlyTenant):
			verdict = "not probed (filtered)"
		case ok:
			present, missing, errs := probe(ctx, db, d, c, *sample)
			verdict = fmt.Sprintf("%d present, %d missing, %d error of %d", present, missing, errs, present+missing+errs)
		case !registeredNames[c.backend]:
			verdict = "no driver answers to this name"
		case !*noProbe:
			verdict = "no credentials in env"
		}
		fmt.Printf("%-12s %-28s %-16s %8d %12d %-10s %-10s %s\n", label(c.backend), c.tenant, trunc(c.tenantName, 16), c.rows, c.bytes, c.minDate, c.maxDate, verdict)
		plan[c.backend] = append(plan[c.backend], fmt.Sprintf("%s (%s): %d rows, %s", c.tenant, c.tenantName, c.rows, verdict))
	}

	fmt.Println()
	fmt.Println("== the plan (nothing above was changed; the decisions are in docs/reviews/WP-R7-5.md)")
	keys := make([]string, 0, len(plan))
	for k := range plan {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("\n%s:\n", label(k))
		for _, line := range plan[k] {
			fmt.Println("  ", line)
		}
		switch k {
		case "":
			fmt.Println("   → NULL rows: POST /api/v1/admin/routing-truth/resolve-null (dry run first; ?dry_run=false writes the rows ONE backend holds; 'nowhere' rows are deleted through S3 DELETE or the tenant's erasure)")
		case "onedrive":
			fmt.Println("   → rename to 'permafrost' ONLY if the probe above says present: UPDATE object_head_cache SET backend_name='permafrost' WHERE backend_name='onedrive' AND tenant_id='<tenant>'; otherwise the bytes are gone — delete through DELETE (quota released) or erase the tenant")
		case "local":
			fmt.Println("   → rows whose file is missing: delete the OBJECT (S3 DELETE as the tenant, or the tenant's erasure) — never the row alone, quota must be released")
		case "idrive":
			fmt.Println("   → rows the signed HEAD misses point at the dead reseller account (before 2026-09-21): same — delete the object or erase the tenant; the bytes cannot come back")
		default:
			if !registeredNames[k] {
				fmt.Println("   → no driver has this name: find the backend that holds the bytes (resolve-null's fan-out does not cover named rows) or treat as gone")
			}
		}
	}
}

func label(backend string) string {
	if backend == "" {
		return "(NULL)"
	}
	return backend
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func openDB() (*sql.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		port, _ := strconv.Atoi(getenv("DB_PORT", "5432"))
		cfg := database.Config{Host: getenv("DB_HOST", "localhost"), Port: port, Database: getenv("DB_NAME", "vaultaire"),
			User: getenv("DB_USER", "viera"), Password: os.Getenv("DB_PASSWORD"), SSLMode: getenv("DB_SSLMODE", "disable")}
		dsn = fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s", cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password, cfg.SSLMode)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one session: the read-only SET applies to every query
	return db, db.Ping()
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadClasses(ctx context.Context, db *sql.DB) ([]class, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(h.backend_name, ''), h.tenant_id, COALESCE(t.name, '?'), COUNT(*), COALESCE(SUM(h.size_bytes), 0),
		       MIN(h.created_at)::date::text, MAX(h.created_at)::date::text, COUNT(DISTINCT h.bucket)
		  FROM object_head_cache h LEFT JOIN tenants t ON t.id = h.tenant_id
		 GROUP BY 1, 2, 3 ORDER BY 1, 4 DESC`)
	if err != nil {
		return nil, fmt.Errorf("head rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []class
	for rows.Next() {
		var c class
		if err := rows.Scan(&c.backend, &c.tenant, &c.tenantName, &c.rows, &c.bytes, &c.minDate, &c.maxDate, &c.buckets); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func printOtherTable(ctx context.Context, db *sql.DB, table string) error {
	var q string
	switch table {
	case "smart_demotions":
		q = `SELECT b, COUNT(*) FROM (SELECT hot_backend AS b FROM smart_demotions WHERE hot_deleted_at IS NULL
		     UNION ALL SELECT cold_backend FROM smart_demotions WHERE hot_deleted_at IS NULL) x GROUP BY 1 ORDER BY 1`
	default:
		q = `SELECT COALESCE(backend_name, ''), COUNT(*) FROM object_versions WHERE NOT is_delete_marker GROUP BY 1 ORDER BY 1`
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("%s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	fmt.Printf("== %s rows per backend\n", table)
	for rows.Next() {
		var b string
		var n int64
		if err := rows.Scan(&b, &n); err != nil {
			return err
		}
		flag := ""
		if !registeredNames[b] {
			flag = "   ← no driver answers to this name"
		}
		fmt.Printf("  %-12s %8d%s\n", label(b), n, flag)
	}
	fmt.Println()
	return rows.Err()
}

// probe asks the driver, with the tenant in the context, about a sample of
// the class's rows — the recorded backend only (never a fan-out).
func probe(ctx context.Context, db *sql.DB, d engine.Driver, c class, n int) (present, missing, errs int) {
	var b any = c.backend
	if c.backend == "" {
		b = nil
	}
	rows, err := db.QueryContext(ctx, `SELECT tenant_id, bucket, object_key FROM object_head_cache
		WHERE tenant_id = $1 AND NOT is_chunked AND ($2::text IS NULL AND backend_name IS NULL OR backend_name = $2::text)
		ORDER BY random() LIMIT $3`, c.tenant, b, n) // #nosec G404 — a sample
	if err != nil {
		fmt.Fprintln(os.Stderr, "sample:", err)
		return 0, 0, 1
	}
	var refs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.tenant, &r.bucket, &r.key); err == nil {
			refs = append(refs, r)
		}
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "sample:", err)
		errs++
	}
	_ = rows.Close()
	for _, r := range refs {
		cctx, cancel := context.WithTimeout(common.WithTenantID(ctx, r.tenant), 30*time.Second)
		ok, err := d.Exists(cctx, r.tenant+"_"+r.bucket, r.key)
		cancel()
		switch {
		case err != nil:
			errs++
			if errs <= 2 {
				fmt.Fprintf(os.Stderr, "  probe error %s/%s on %s: %v\n", r.bucket, r.key, label(c.backend), err)
			}
		case ok && present < 3:
			present++
			fmt.Fprintf(os.Stderr, "  present: %s/%s on %s\n", r.bucket, r.key, label(c.backend))
		case ok:
			present++
		default:
			missing++
		}
	}
	return present, missing, errs
}

// buildProbes builds the drivers the environment allows, the way main.go does.
func buildProbes(logger *zap.Logger) map[string]engine.Driver {
	probes := map[string]engine.Driver{}
	if p := os.Getenv("DATA_PATH"); p != "" {
		probes["local"] = drivers.NewLocalDriver(p, logger)
	}
	if ak := os.Getenv("IDRIVE_ACCESS_KEY"); ak != "" {
		d, err := drivers.NewIDriveDriver(ak, os.Getenv("IDRIVE_SECRET_KEY"), os.Getenv("IDRIVE_ENDPOINT"), os.Getenv("IDRIVE_REGION"), logger)
		if err != nil {
			fmt.Fprintln(os.Stderr, "idrive driver:", err)
		} else {
			probes["idrive"] = d
		}
	}
	if os.Getenv("TENANT_1_ID") != "" {
		d, err := drivers.NewOneDriveFleetDriver(logger)
		if err != nil {
			fmt.Fprintln(os.Stderr, "permafrost driver:", err)
		} else {
			// The fleet answers to "permafrost" — and is the only candidate
			// for rows still named "onedrive" (R7-10).
			probes["permafrost"] = d
			probes["onedrive"] = d
		}
	}
	return probes
}

// probeOne: -one backend:tenant/bucket/key (HEAD then GET) or -list backend:tenant/bucket.
func probeOne(ctx context.Context, logger *zap.Logger, one, list string) {
	probes := buildProbes(logger)
	spec := one
	if spec == "" {
		spec = list
	}
	backend, rest, ok := strings.Cut(spec, ":")
	if !ok {
		fmt.Fprintln(os.Stderr, "want backend:tenant/bucket[/key]")
		os.Exit(2)
	}
	d, ok := probes[backend]
	if !ok {
		fmt.Fprintln(os.Stderr, "no driver for", backend, "in this environment")
		os.Exit(2)
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		fmt.Fprintln(os.Stderr, "want backend:tenant/bucket[/key]")
		os.Exit(2)
	}
	tctx := common.WithTenantID(ctx, parts[0])
	container := parts[0] + "_" + parts[1]
	if list != "" {
		keys, err := d.List(tctx, container, "")
		fmt.Printf("list %s %s: %d keys, err=%v\n", backend, container, len(keys), err)
		for i, k := range keys {
			if i >= 40 {
				fmt.Println("  …")
				break
			}
			fmt.Println("  ", k)
		}
		return
	}
	if len(parts) < 3 {
		fmt.Fprintln(os.Stderr, "want backend:tenant/bucket/key")
		os.Exit(2)
	}
	key := parts[2]
	exists, err := d.Exists(tctx, container, key)
	fmt.Printf("HEAD %s %s/%s: exists=%v err=%v\n", backend, container, key, exists, err)
	rc, err := d.Get(tctx, container, key)
	if err != nil {
		fmt.Printf("GET  %s %s/%s: err=%v\n", backend, container, key, err)
		return
	}
	buf := make([]byte, 1)
	n, rerr := rc.Read(buf)
	_ = rc.Close()
	fmt.Printf("GET  %s %s/%s: ok, read %d byte(s), err=%v\n", backend, container, key, n, rerr)
}
