// erasure-bench: what would Phase 11 (Reed-Solomon erasure coding across
// backends) cost and deliver on today's rented backends? Tooling only — the
// engine has no erasure path yet; this is the driver-level dress rehearsal.
//
// Per run: random payload → RS(k,m) split+encode → every shard PUT in
// parallel to the backend the layout assigns it → three read shapes, each
// reconstructing the payload and verifying its SHA-256:
//
//	first-k   all n shards requested in parallel, the first k to land win,
//	          the rest are cancelled (what a real read path does)
//	lose:<b>  every shard on backend b is treated as lost (backend outage)
//	          and the payload is rebuilt from the survivors
//
// Layout syntax: "lyve:6,geyser:4,onedrive:6" — shard slots are handed out
// in layout order, data shards (0..k-1) first, so the last entries carry the
// parity. Any backend holding more than m shards is a single point of
// failure and is flagged.
//
// Backends come from env (same vars as prod): lyve (LYVE_*), geyser
// (GEYSER_* + GEYSER_BUCKET or GEYSER_LA_BUCKET), onedrive/permafrost
// (TENANT_N_*), idrive (IDRIVE_*), local. Usage on SLC:
//
//	set -a; . ~/vaultaire-bench/.env.bench; set +a
//	GEYSER_BUCKET=$GEYSER_LA_BUCKET ./erasure-bench -mb 64 -runs 2
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"github.com/klauspost/reedsolomon"
	"go.uber.org/zap"
)

// slot is one shard's home.
type slot struct {
	idx     int
	backend string
}

// parseLayout turns "lyve:6,geyser:4,onedrive:6" into n slots in layout
// order. It errors when the counts don't add up to n.
func parseLayout(spec string, n int) ([]slot, error) {
	var slots []slot
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, cnt, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("layout entry %q: want backend:count", part)
		}
		c, err := strconv.Atoi(cnt)
		if err != nil || c <= 0 {
			return nil, fmt.Errorf("layout entry %q: bad count", part)
		}
		for i := 0; i < c; i++ {
			slots = append(slots, slot{idx: len(slots), backend: strings.TrimSpace(name)})
		}
	}
	if len(slots) != n {
		return nil, fmt.Errorf("layout assigns %d shards, scheme needs %d", len(slots), n)
	}
	return slots, nil
}

// perBackend counts shards per backend in a layout.
func perBackend(slots []slot) map[string]int {
	m := map[string]int{}
	for _, s := range slots {
		m[s.backend]++
	}
	return m
}

type result struct {
	dur time.Duration
	err error
}

func main() {
	sizeMB := flag.Int("mb", 64, "payload size in MiB")
	k := flag.Int("data", 10, "data shards")
	m := flag.Int("parity", 6, "parity shards")
	layout := flag.String("layout", "lyve:6,geyser:4,onedrive:6", "shard placement backend:count,... (data shards first)")
	runs := flag.Int("runs", 1, "repetitions (pair runs — single Lyve runs are noise)")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-shard transfer timeout")
	keep := flag.Bool("keep", false, "leave shards on the backends")
	flag.Parse()
	logger := zap.NewNop()
	n := *k + *m

	slots, err := parseLayout(*layout, n)
	if err != nil {
		fmt.Println("layout:", err)
		os.Exit(2)
	}
	bes := buildBackends(logger)
	for name, cnt := range perBackend(slots) {
		if _, ok := bes[name]; !ok {
			fmt.Printf("layout needs backend %q but it is not configured\n", name)
			os.Exit(2)
		}
		if cnt > *m {
			fmt.Printf("WARNING: %s holds %d shards > %d parity — losing it alone breaks reconstruction\n", name, cnt, *m)
		}
	}

	enc, err := reedsolomon.New(*k, *m)
	if err != nil {
		fmt.Println("reedsolomon:", err)
		os.Exit(2)
	}
	size := int64(*sizeMB) << 20
	fmt.Printf("scheme RS(%d,%d) = %d shards, payload %d MiB, shard %.1f MiB, overhead %.2fx\n",
		*k, *m, n, *sizeMB, float64(size)/float64(*k)/(1<<20), float64(n)/float64(*k))
	fmt.Printf("layout: %s\n", describe(slots, *k))

	ctx := common.WithTenantID(context.Background(), "ecbench")
	container := fmt.Sprintf("ec-%d", time.Now().Unix())

	for r := 1; r <= *runs; r++ {
		fmt.Printf("\n== run %d/%d ==\n", r, *runs)
		runOnce(ctx, enc, bes, slots, *k, size, container, r, *timeout, *keep)
	}
}

func runOnce(ctx context.Context, enc reedsolomon.Encoder, bes map[string]engine.Driver, slots []slot, k int, size int64, container string, run int, timeout time.Duration, keep bool) {
	payload := make([]byte, size)
	_, _ = rand.Read(payload)
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])

	// Encode.
	t0 := time.Now()
	shards, err := enc.Split(payload)
	if err != nil {
		fmt.Println("split:", err)
		return
	}
	if err := enc.Encode(shards); err != nil {
		fmt.Println("encode:", err)
		return
	}
	encDur := time.Since(t0)
	shardLen := int64(len(shards[0]))
	fmt.Printf("encode      %6s  (%s)\n", fmtDur(encDur), mbps(size, encDur))

	key := func(i int) string { return fmt.Sprintf("r%d-shard-%02d.bin", run, i) }

	// Store all shards in parallel.
	putRes := make([]result, len(slots))
	var wg sync.WaitGroup
	t1 := time.Now()
	for _, s := range slots {
		wg.Add(1)
		go func(s slot) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			st := time.Now()
			e := bes[s.backend].Put(c, container, key(s.idx), bytes.NewReader(shards[s.idx]), engine.WithContentLength(shardLen))
			putRes[s.idx] = result{time.Since(st), e}
		}(s)
	}
	wg.Wait()
	putWall := time.Since(t1)
	fmt.Printf("PUT all %2d  %6s  payload %s, wire %s   %s\n", len(slots), fmtDur(putWall),
		mbps(size, putWall), mbps(shardLen*int64(len(slots)), putWall), perBackendSummary(slots, putRes))
	failed := 0
	for i, pr := range putRes {
		if pr.err != nil {
			failed++
			fmt.Printf("  shard %02d on %-8s PUT FAILED: %s\n", i, slots[i].backend, trunc(pr.err, 80))
		}
	}
	if failed > len(slots)-k {
		fmt.Println("  too many shard writes failed — skipping reads")
		cleanup(ctx, bes, slots, container, key, keep)
		return
	}

	// Read 1: first-k.
	readFirstK(ctx, enc, bes, slots, k, container, key, shardLen, size, want, timeout)

	// Read 2..: lose each backend in turn.
	names := make([]string, 0)
	for name := range perBackend(slots) {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, lost := range names {
		readDegraded(ctx, enc, bes, slots, k, container, key, shardLen, size, want, timeout, lost)
	}

	cleanup(ctx, bes, slots, container, key, keep)
}

// fetch pulls one shard; returns the bytes or an error.
func fetch(ctx context.Context, d engine.Driver, container, key string, shardLen int64) ([]byte, error) {
	rc, err := d.Get(ctx, container, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	buf := make([]byte, 0, shardLen)
	b := bytes.NewBuffer(buf)
	if _, err := io.Copy(b, rc); err != nil {
		return nil, err
	}
	if int64(b.Len()) != shardLen {
		return nil, fmt.Errorf("short shard: %d of %d bytes", b.Len(), shardLen)
	}
	return b.Bytes(), nil
}

type fetched struct {
	idx  int
	data []byte
	dur  time.Duration
	err  error
}

// readFirstK requests every shard and reconstructs from the first k that
// arrive, cancelling the rest.
func readFirstK(ctx context.Context, enc reedsolomon.Encoder, bes map[string]engine.Driver, slots []slot, k int, container string, key func(int) string, shardLen, size int64, want string, timeout time.Duration) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ch := make(chan fetched, len(slots))
	t0 := time.Now()
	for _, s := range slots {
		go func(s slot) {
			st := time.Now()
			data, err := fetch(rctx, bes[s.backend], container, key(s.idx), shardLen)
			ch <- fetched{s.idx, data, time.Since(st), err}
		}(s)
	}
	have := make([][]byte, len(slots))
	got, errs := 0, 0
	winners := map[string]int{}
	var lastErr error
	for got < k && got+errs < len(slots) {
		f := <-ch
		if f.err != nil {
			errs++
			lastErr = f.err
			continue
		}
		have[f.idx] = f.data
		winners[slots[f.idx].backend]++
		got++
	}
	fetchWall := time.Since(t0)
	cancel()
	if got < k {
		fmt.Printf("READ first-k  FAILED: only %d/%d shards (%s)\n", got, k, trunc(lastErr, 80))
		return
	}
	dec, ok := reconstruct(enc, have, k, shardLen, size, want)
	fmt.Printf("READ first-%d %6s  %s  fetch %s + decode %s  winners %s  %s\n", k, fmtDur(fetchWall+dec),
		mbps(int64(k)*shardLen, fetchWall+dec), fmtDur(fetchWall), fmtDur(dec), fmtMap(winners), okStr(ok))
}

// readDegraded rebuilds the payload with every shard on `lost` missing.
func readDegraded(ctx context.Context, enc reedsolomon.Encoder, bes map[string]engine.Driver, slots []slot, k int, container string, key func(int) string, shardLen, size int64, want string, timeout time.Duration, lost string) {
	var survivors []slot
	for _, s := range slots {
		if s.backend != lost {
			survivors = append(survivors, s)
		}
	}
	if len(survivors) < k {
		fmt.Printf("READ lose:%-8s IMPOSSIBLE: %d survivors < %d needed\n", lost, len(survivors), k)
		return
	}
	// Fetch first-k among survivors (same shape as the real path under an outage).
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ch := make(chan fetched, len(survivors))
	t0 := time.Now()
	for _, s := range survivors {
		go func(s slot) {
			st := time.Now()
			data, err := fetch(rctx, bes[s.backend], container, key(s.idx), shardLen)
			ch <- fetched{s.idx, data, time.Since(st), err}
		}(s)
	}
	have := make([][]byte, len(slots))
	got, errs := 0, 0
	winners := map[string]int{}
	var lastErr error
	for got < k && got+errs < len(survivors) {
		f := <-ch
		if f.err != nil {
			errs++
			lastErr = f.err
			continue
		}
		have[f.idx] = f.data
		winners[slots[f.idx].backend]++
		got++
	}
	fetchWall := time.Since(t0)
	cancel()
	if got < k {
		fmt.Printf("READ lose:%-8s FAILED: only %d/%d shards (%s)\n", lost, got, k, trunc(lastErr, 80))
		return
	}
	dec, ok := reconstruct(enc, have, k, shardLen, size, want)
	fmt.Printf("READ lose:%-8s %6s  %s  fetch %s + decode %s  from %s  %s\n", lost, fmtDur(fetchWall+dec),
		mbps(int64(k)*shardLen, fetchWall+dec), fmtDur(fetchWall), fmtDur(dec), fmtMap(winners), okStr(ok))
}

// reconstruct fills missing shards, joins the data shards and checks the hash.
// size is the original payload length: Split zero-pads the last shard, so
// Join must stop at size, not at shardLen*k.
func reconstruct(enc reedsolomon.Encoder, have [][]byte, k int, shardLen, size int64, want string) (time.Duration, bool) {
	t0 := time.Now()
	if err := enc.ReconstructData(have); err != nil {
		fmt.Println("  reconstruct:", err)
		return time.Since(t0), false
	}
	var out bytes.Buffer
	out.Grow(int(size))
	if err := enc.Join(&out, have, int(size)); err != nil {
		fmt.Println("  join:", err)
		return time.Since(t0), false
	}
	sum := sha256.Sum256(out.Bytes())
	return time.Since(t0), hex.EncodeToString(sum[:]) == want
}

func cleanup(ctx context.Context, bes map[string]engine.Driver, slots []slot, container string, key func(int) string, keep bool) {
	if keep {
		return
	}
	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Add(1)
		go func(s slot) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			_ = bes[s.backend].Delete(c, container, key(s.idx))
		}(s)
	}
	wg.Wait()
}

func buildBackends(logger *zap.Logger) map[string]engine.Driver {
	bes := map[string]engine.Driver{}
	add := func(name string, d engine.Driver, err error) {
		if err != nil {
			fmt.Printf("skip %-9s %v\n", name, err)
			return
		}
		bes[name] = d
	}
	dir, _ := os.MkdirTemp("", "ec-local-*")
	add("local", drivers.NewLocalDriver(dir, logger), nil)
	if ak := os.Getenv("IDRIVE_ACCESS_KEY"); ak != "" {
		d, err := drivers.NewIDriveDriver(ak, os.Getenv("IDRIVE_SECRET_KEY"), os.Getenv("IDRIVE_ENDPOINT"), os.Getenv("IDRIVE_REGION"), logger)
		add("idrive", d, err)
	}
	if ak := os.Getenv("LYVE_ACCESS_KEY"); ak != "" {
		d, err := drivers.NewLyveDriver(ak, os.Getenv("LYVE_SECRET_KEY"), "ecbench", os.Getenv("LYVE_REGION"), logger)
		add("lyve", d, err)
	}
	if ak := os.Getenv("GEYSER_ACCESS_KEY"); ak != "" {
		bucket := os.Getenv("GEYSER_BUCKET")
		if bucket == "" {
			bucket = os.Getenv("GEYSER_LA_BUCKET")
		}
		d, err := drivers.NewGeyserDriver(ak, os.Getenv("GEYSER_SECRET_KEY"), bucket, "ecbench", logger)
		add("geyser", d, err)
	}
	if os.Getenv("TENANT_1_ID") != "" {
		d, err := drivers.NewOneDriveFleetDriver(logger)
		add("onedrive", d, err)
	}
	names := make([]string, 0, len(bes))
	for n := range bes {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("backends configured: %s\n", strings.Join(names, ", "))
	return bes
}

func describe(slots []slot, k int) string {
	var parts []string
	cur, start := "", 0
	flush := func(end int) {
		if cur == "" {
			return
		}
		role := "data"
		if start >= k {
			role = "parity"
		} else if end > k {
			role = "data+parity"
		}
		parts = append(parts, fmt.Sprintf("%s=%d-%d(%s)", cur, start, end-1, role))
	}
	for i, s := range slots {
		if s.backend != cur {
			flush(i)
			cur, start = s.backend, i
		}
	}
	flush(len(slots))
	return strings.Join(parts, " ")
}

func perBackendSummary(slots []slot, res []result) string {
	maxd := map[string]time.Duration{}
	for i, s := range slots {
		if res[i].err == nil && res[i].dur > maxd[s.backend] {
			maxd[s.backend] = res[i].dur
		}
	}
	names := make([]string, 0, len(maxd))
	for n := range maxd {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s slowest %s", n, fmtDur(maxd[n])))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func fmtMap(m map[string]int) string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s:%d", n, m[n]))
	}
	return strings.Join(parts, ",")
}

func mbps(n int64, d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f MB/s", float64(n)/(1<<20)/d.Seconds())
}

func fmtDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func okStr(ok bool) string {
	if ok {
		return "hash ok"
	}
	return "HASH MISMATCH"
}

func trunc(err error, n int) string {
	if err == nil {
		return "-"
	}
	s := err.Error()
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n]
	}
	return s
}
