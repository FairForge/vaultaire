// pack-bench: the pack store (internal/packstore) on a real backend — the
// Sync.com bridges through the multi-bridge driver. Lab tool: its own
// database (migration 080 only) and its own root, never prod's index.
//
// Phases: write (W writers, mixed member sizes, packs sealed at the target
// size) → cold random member reads (sha256-verified by the store) → range
// probe (first vs last member of one pack: does the bridge serve a true
// range?) → delete a fraction + GC (compaction rewrites) → recover a pack's
// index from the file alone → delete everything + GC (nothing left).
//
//	PACKBENCH_DSN=postgres://… SYNC_WEBDAV_URLS=… SYNC_WEBDAV_PASSWORDS=… \
//	SYNC_WEBDAV_ROOT=packbench ./pack-bench -members 6000 -writers 4
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/packstore"
	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

type member struct {
	tenant, key string
	size        int64
	seed        int64
}

// memberSize draws from a photo-library / backup-chunk mix: 70 % 16–128 KiB
// (thumbnails, metadata), 25 % 256 KiB–2 MiB (previews, small files),
// 5 % 4–16 MiB (originals, chunks).
func memberSize(r *rand.Rand) int64 {
	switch p := r.Float64(); {
	case p < 0.70:
		return int64(16<<10 + r.Intn(112<<10))
	case p < 0.95:
		return int64(256<<10 + r.Intn(1792<<10))
	default:
		return int64(4<<20 + r.Intn(12<<20))
	}
}

func body(m member) io.Reader { return io.LimitReader(rand.New(rand.NewSource(m.seed)), m.size) }

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[int(float64(len(d)-1)*p)]
}

func mbps(b int64, d time.Duration) float64 { return float64(b) / 1e6 / d.Seconds() }

func main() {
	nMembers := flag.Int("members", 6000, "members to write")
	writers := flag.Int("writers", 4, "concurrent pack writers")
	target := flag.Int64("target-mb", 256, "pack target size in MiB")
	reads := flag.Int("reads", 300, "random member reads")
	readC := flag.Int("read-concurrency", 16, "concurrent member reads")
	delFrac := flag.Float64("delete-frac", 0.6, "fraction of members deleted before the compaction GC")
	keep := flag.Bool("keep", false, "skip the final delete-everything phase")
	out := flag.String("out", "", "write the results as JSON here")
	flag.Parse()

	ctx := context.Background()
	logger, _ := zap.NewProduction()
	res := map[string]any{}
	say := func(phase string, kv map[string]any) {
		res[phase] = kv
		b, _ := json.Marshal(kv)
		fmt.Printf("%-10s %s\n", phase, b)
	}
	fail := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
		os.Exit(1)
	}

	db, err := sql.Open("postgres", os.Getenv("PACKBENCH_DSN"))
	if err != nil {
		fail("db", err)
	}
	cfg, ok, err := drivers.SyncWebDAVConfigFromEnv(os.Getenv)
	if err != nil || !ok {
		fail("sync config", fmt.Errorf("ok=%v %w", ok, err))
	}
	drv, err := drivers.NewMultiWebDAVDriver("sync", cfg, logger)
	if err != nil {
		fail("driver", err)
	}
	st, err := packstore.New(db, drv, packstore.Options{BackendName: "sync-packlab", TargetSize: *target << 20, Logger: logger})
	if err != nil {
		fail("store", err)
	}

	// --- write
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	run := time.Now().Format("20060102-150405")
	ms := make([]member, *nMembers)
	var total int64
	for i := range ms {
		ms[i] = member{tenant: fmt.Sprintf("t-lab-%d", i%7), key: fmt.Sprintf("%s/m%06d", run, i), size: memberSize(r), seed: r.Int63()}
		total += ms[i].size
	}
	var mu sync.Mutex
	var seals []time.Duration
	packs := 0
	t0 := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			wr, err := st.NewWriter()
			if err != nil {
				fail("writer", err)
			}
			defer func() { _ = wr.Close(ctx) }()
			last := time.Now()
			note := func(sp *packstore.SealedPack) {
				if sp == nil {
					return
				}
				mu.Lock()
				seals = append(seals, time.Since(last))
				packs++
				mu.Unlock()
				last = time.Now()
			}
			for i := w; i < len(ms); i += *writers {
				sp, err := wr.Add(ctx, ms[i].tenant, ms[i].key, ms[i].size, body(ms[i]))
				if err != nil {
					fail("add "+ms[i].key, err)
				}
				note(sp)
			}
			sp, err := wr.Flush(ctx)
			if err != nil {
				fail("flush", err)
			}
			note(sp)
		}(w)
	}
	wg.Wait()
	wd := time.Since(t0)
	say("write", map[string]any{"members": len(ms), "bytes": total, "packs": packs, "seconds": wd.Seconds(),
		"members_per_s": float64(len(ms)) / wd.Seconds(), "MB_per_s": mbps(total, wd),
		"pack_cycle_p50_s": pct(seals, .5).Seconds(), "pack_cycle_max_s": pct(seals, 1).Seconds()})

	// --- cold random reads
	idx := r.Perm(len(ms))[:min(*reads, len(ms))]
	var lat []time.Duration
	var rb int64
	sem := make(chan struct{}, *readC)
	t0 = time.Now()
	errs := 0
	for _, i := range idx {
		wg.Add(1)
		sem <- struct{}{}
		go func(m member) {
			defer wg.Done()
			defer func() { <-sem }()
			s := time.Now()
			rc, err := st.Get(ctx, m.tenant, m.key)
			var n int64
			if err == nil {
				n, err = io.Copy(io.Discard, rc)
				_ = rc.Close()
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil || n != m.size {
				errs++
				fmt.Fprintf(os.Stderr, "read %s: n=%d err=%v\n", m.key, n, err)
				return
			}
			lat = append(lat, time.Since(s))
			rb += n
		}(ms[i])
	}
	wg.Wait()
	rd := time.Since(t0)
	say("read", map[string]any{"reads": len(idx), "concurrency": *readC, "errors": errs, "per_s": float64(len(lat)) / rd.Seconds(),
		"MB_per_s": mbps(rb, rd), "p50_s": pct(lat, .5).Seconds(), "p95_s": pct(lat, .95).Seconds(), "max_s": pct(lat, 1).Seconds()})

	// --- range probe: first vs last member of the largest pack (cold, then warm)
	var packName string
	var firstT, firstK, lastT, lastK string
	var lastOff int64
	if err := db.QueryRowContext(ctx, `SELECT name FROM packs WHERE backend='sync-packlab' AND sealed_at IS NOT NULL AND retired_at IS NULL ORDER BY size DESC LIMIT 1`).Scan(&packName); err != nil {
		fail("probe pack", err)
	}
	q := `SELECT m.tenant_id, m.member_key, m.byte_offset FROM pack_members m JOIN packs p ON p.id=m.pack_id WHERE p.name=$1 AND p.backend='sync-packlab' AND m.byte_length < 200000 ORDER BY m.byte_offset `
	var off int64
	_ = db.QueryRowContext(ctx, q+"ASC LIMIT 1", packName).Scan(&firstT, &firstK, &off)
	_ = db.QueryRowContext(ctx, q+"DESC LIMIT 1", packName).Scan(&lastT, &lastK, &lastOff)
	timeGet := func(t, k string) float64 {
		s := time.Now()
		rc, err := st.Get(ctx, t, k)
		if err != nil {
			return -1
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
		return time.Since(s).Seconds()
	}
	say("range", map[string]any{"pack": packName, "last_offset_MiB": lastOff >> 20,
		"first_cold_s": timeGet(firstT, firstK), "last_cold_s": timeGet(lastT, lastK),
		"first_again_s": timeGet(firstT, firstK), "last_again_s": timeGet(lastT, lastK)})

	// --- recover the index from the file alone
	s := time.Now()
	ft, err := st.ReadPackIndex(ctx, packName)
	if err != nil {
		fail("read index", err)
	}
	say("recover", map[string]any{"members": len(ft.Members), "seconds": time.Since(s).Seconds()})

	// --- delete a fraction, then GC (compaction)
	nDel := int(float64(len(ms)) * *delFrac)
	del := r.Perm(len(ms))[:nDel]
	deleted := map[int]bool{}
	s = time.Now()
	for _, i := range del {
		if err := st.Delete(ctx, ms[i].tenant, ms[i].key); err != nil {
			fail("delete", err)
		}
		deleted[i] = true
	}
	delS := time.Since(s).Seconds()
	s = time.Now()
	g, err := st.GC(ctx)
	if err != nil {
		fail("gc", err)
	}
	say("compact", map[string]any{"deleted": nDel, "delete_rows_s": delS, "gc_s": time.Since(s).Seconds(), "result": g})

	// survivors still read back intact (some moved into new packs)
	bad := 0
	for n, i := range r.Perm(len(ms)) {
		if n >= 100 {
			break
		}
		if deleted[i] {
			continue
		}
		rc, err := st.Get(ctx, ms[i].tenant, ms[i].key)
		if err == nil {
			var n int64
			n, err = io.Copy(io.Discard, rc)
			_ = rc.Close()
			if err == nil && n != ms[i].size {
				err = fmt.Errorf("short read %d", n)
			}
		}
		if err != nil {
			bad++
			fmt.Fprintf(os.Stderr, "survivor %s: %v\n", ms[i].key, err)
		}
	}
	say("survivors", map[string]any{"checked_up_to": 100, "bad": bad})

	if !*keep {
		s = time.Now()
		for i := range ms {
			if !deleted[i] {
				if err := st.Delete(ctx, ms[i].tenant, ms[i].key); err != nil {
					fail("delete", err)
				}
			}
		}
		g, err := st.GC(ctx)
		if err != nil {
			fail("gc", err)
		}
		var left int
		_ = db.QueryRowContext(ctx, `SELECT count(*) FROM packs WHERE backend='sync-packlab'`).Scan(&left)
		say("cleanup", map[string]any{"seconds": time.Since(s).Seconds(), "result": g, "pack_rows_left": left})
	}
	if *out != "" {
		b, _ := json.MarshalIndent(res, "", "  ")
		_ = os.WriteFile(*out, b, 0o600)
	}
}
