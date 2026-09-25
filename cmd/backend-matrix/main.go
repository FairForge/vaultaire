// backend-matrix: pairwise "does data move between our backends the way the
// engine moves it" check. For every ordered pair (src → dst) it PUTs a random
// object on src, streams Get(src) into Put(dst) (exactly what the Smart
// demotion job, tiering and restore paths do), reads it back from dst,
// verifies the SHA-256, and deletes both. Prints one table.
//
// Backends come from env (same vars as prod): local (DATA_PATH or a temp dir),
// idrive (IDRIVE_*), lyve (LYVE_*), geyser (GEYSER_*), onedrive/permafrost
// (TENANT_1_*...), wasabi (WASABI_*), r2 (R2_ACCOUNT_ID + R2_ACCESS_KEY/SECRET +
// R2_BENCH_BUCKET — a scratch bucket, never the public one). Missing creds =
// backend skipped. Tooling only.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

type backend struct {
	name string
	drv  engine.Driver
}

func main() {
	sizeMB := flag.Int("mb", 64, "object size in MiB")
	only := flag.String("only", "", "comma-separated backend names to include")
	skip := flag.String("skip", "", "comma-separated backend names to exclude")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-transfer timeout")
	flag.Parse()
	logger := zap.NewNop()

	var bes []backend
	add := func(name string, d engine.Driver, err error) {
		if err != nil {
			fmt.Printf("skip %-9s %v\n", name, err)
			return
		}
		bes = append(bes, backend{name, d})
	}
	dir := os.Getenv("MATRIX_LOCAL_DIR")
	if dir == "" {
		dir, _ = os.MkdirTemp("", "matrix-local-*")
		defer func() { _ = os.RemoveAll(dir) }()
	}
	add("local", drivers.NewLocalDriver(dir, logger), nil)
	if ak := os.Getenv("IDRIVE_ACCESS_KEY"); ak != "" {
		d, err := drivers.NewIDriveDriver(ak, os.Getenv("IDRIVE_SECRET_KEY"), os.Getenv("IDRIVE_ENDPOINT"), os.Getenv("IDRIVE_REGION"), logger)
		add("idrive", d, err)
	} else {
		fmt.Println("skip idrive    no IDRIVE_ACCESS_KEY")
	}
	if ak := os.Getenv("LYVE_ACCESS_KEY"); ak != "" {
		d, err := drivers.NewLyveDriver(ak, os.Getenv("LYVE_SECRET_KEY"), "matrix", os.Getenv("LYVE_REGION"), logger)
		add("lyve", d, err)
	} else {
		fmt.Println("skip lyve      no LYVE_ACCESS_KEY")
	}
	if ak := os.Getenv("GEYSER_ACCESS_KEY"); ak != "" {
		d, err := drivers.NewGeyserDriver(ak, os.Getenv("GEYSER_SECRET_KEY"), os.Getenv("GEYSER_BUCKET"), "matrix", logger)
		add("geyser", d, err)
	} else {
		fmt.Println("skip geyser    no GEYSER_ACCESS_KEY")
	}
	if os.Getenv("TENANT_1_ID") != "" {
		d, err := drivers.NewOneDriveFleetDriver(logger)
		add("onedrive", d, err)
	} else {
		fmt.Println("skip onedrive  no TENANT_1_ID")
	}
	if ak := os.Getenv("WASABI_ACCESS_KEY"); ak != "" {
		d, err := drivers.NewS3Driver(os.Getenv("WASABI_ENDPOINT"), ak, os.Getenv("WASABI_SECRET_KEY"), os.Getenv("WASABI_REGION"), logger)
		if err == nil {
			add("wasabi", s3WithHealth{d, os.Getenv("WASABI_BUCKET"), "wasabi"}, nil)
		} else {
			add("wasabi", nil, err)
		}
	} else {
		fmt.Println("skip wasabi    no WASABI_ACCESS_KEY")
	}
	if acct := os.Getenv("R2_ACCOUNT_ID"); acct != "" && os.Getenv("R2_ACCESS_KEY") != "" {
		bucket := os.Getenv("R2_BENCH_BUCKET")
		if bucket == "" {
			bucket = "vt-ecbench"
		}
		d, err := drivers.NewS3Driver("https://"+acct+".r2.cloudflarestorage.com", os.Getenv("R2_ACCESS_KEY"), os.Getenv("R2_SECRET_KEY"), "auto", logger)
		if err == nil {
			add("r2", s3WithHealth{d, bucket, "r2"}, nil)
		} else {
			add("r2", nil, err)
		}
	} else {
		fmt.Println("skip r2        no R2_ACCOUNT_ID/R2_ACCESS_KEY")
	}
	inc := map[string]bool{}
	for _, n := range strings.Split(*only, ",") {
		if n != "" {
			inc[n] = true
		}
	}
	exc := map[string]bool{}
	for _, n := range strings.Split(*skip, ",") {
		if n != "" {
			exc[n] = true
		}
	}
	var sel []backend
	for _, b := range bes {
		if (len(inc) == 0 || inc[b.name]) && !exc[b.name] {
			sel = append(sel, b)
		}
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i].name < sel[j].name })
	names := make([]string, 0, len(sel))
	for _, b := range sel {
		names = append(names, b.name)
	}
	fmt.Printf("backends: %s  size: %d MiB\n\n", strings.Join(names, ", "), *sizeMB)

	size := int64(*sizeMB) << 20
	payload := make([]byte, size)
	_, _ = rand.Read(payload)
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	container := fmt.Sprintf("matrix-%d", time.Now().Unix())
	ctx := common.WithTenantID(context.Background(), "matrix")
	mbps := func(n int64, d time.Duration) string {
		if d <= 0 {
			return "-"
		}
		return fmt.Sprintf("%.0f", float64(n)/(1<<20)/d.Seconds())
	}

	// 1. Solo health + direct PUT/GET per backend.
	fmt.Println("== solo: health, direct PUT, direct GET (MB/s) ==")
	for _, b := range sel {
		hctx, hc := context.WithTimeout(ctx, 30*time.Second)
		herr := b.drv.HealthCheck(hctx)
		hc()
		key := "solo.bin"
		pctx, pc := context.WithTimeout(ctx, *timeout)
		t0 := time.Now()
		perr := b.drv.Put(pctx, container, key, strings.NewReader(string(payload)), engine.WithContentLength(size))
		pd := time.Since(t0)
		pc()
		gres := "-"
		if perr == nil {
			gctx, gc := context.WithTimeout(ctx, *timeout)
			t1 := time.Now()
			rc, gerr := b.drv.Get(gctx, container, key)
			if gerr == nil {
				h := sha256.New()
				n, cerr := io.Copy(h, rc)
				_ = rc.Close()
				gd := time.Since(t1)
				if cerr != nil {
					gres = "read err: " + cerr.Error()
				} else if hex.EncodeToString(h.Sum(nil)) != want {
					gres = fmt.Sprintf("HASH MISMATCH (%d bytes)", n)
				} else {
					gres = mbps(n, gd) + " ok"
				}
			} else {
				gres = "get err: " + trunc(gerr)
			}
			gc()
			_ = b.drv.Delete(ctx, container, key)
		}
		hs := "ok"
		if herr != nil {
			hs = "FAIL " + trunc(herr)
		}
		ps := mbps(size, pd)
		if perr != nil {
			ps = "put err: " + trunc(perr)
		}
		fmt.Printf("  %-9s health=%-6s put=%-8s get=%s\n", b.name, hs, ps, gres)
	}

	// 2. Pairwise: seed src, stream src→dst, verify on dst, delete.
	fmt.Println("\n== pairwise src → dst: stream MB/s (Get(src) piped into Put(dst)), then verify on dst ==")
	fmt.Printf("  %-9s", "src\\dst")
	for _, b := range sel {
		fmt.Printf(" %-14s", b.name)
	}
	fmt.Println()
	for _, src := range sel {
		fmt.Printf("  %-9s", src.name)
		key := "pair-" + src.name + ".bin"
		sctx, sc := context.WithTimeout(ctx, *timeout)
		serr := src.drv.Put(sctx, container, key, strings.NewReader(string(payload)), engine.WithContentLength(size))
		sc()
		for _, dst := range sel {
			if dst.name == src.name {
				fmt.Printf(" %-14s", "·")
				continue
			}
			if serr != nil {
				fmt.Printf(" %-14s", "seed-fail")
				continue
			}
			cell := runPair(ctx, src, dst, container, key, size, want, *timeout, mbps)
			fmt.Printf(" %-14s", cell)
		}
		_ = src.drv.Delete(ctx, container, key)
		fmt.Println()
	}
	fmt.Println("\ncells: '<MB/s> ok' = streamed copy + hash verified on dst; errors abbreviated")
}

func runPair(ctx context.Context, src, dst backend, container, key string, size int64, want string, timeout time.Duration, mbps func(int64, time.Duration) string) string {
	dkey := key + "." + dst.name
	gctx, gc := context.WithTimeout(ctx, timeout)
	defer gc()
	rc, err := src.drv.Get(gctx, container, key)
	if err != nil {
		return "srcget:" + trunc(err)
	}
	t0 := time.Now()
	perr := dst.drv.Put(gctx, container, dkey, rc, engine.WithContentLength(size))
	_ = rc.Close()
	d := time.Since(t0)
	if perr != nil {
		return "dstput:" + trunc(perr)
	}
	vctx, vc := context.WithTimeout(ctx, timeout)
	defer vc()
	vrc, verr := dst.drv.Get(vctx, container, dkey)
	if verr != nil {
		_ = dst.drv.Delete(ctx, container, dkey)
		return "verify:" + trunc(verr)
	}
	h := sha256.New()
	n, cerr := io.Copy(h, vrc)
	_ = vrc.Close()
	_ = dst.drv.Delete(ctx, container, dkey)
	if cerr != nil {
		return "verifyread:" + trunc(cerr)
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return fmt.Sprintf("HASH-MISMATCH %d", n)
	}
	return mbps(size, d) + " ok"
}

func trunc(err error) string {
	s := err.Error()
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

// s3WithHealth adapts the generic S3Driver (bucket = container, no
// HealthCheck, non-variadic Put) to engine.Driver using one fixed bucket
// (WASABI_BUCKET) and container/artifact as the key.
type s3WithHealth struct {
	*drivers.S3Driver
	bucket string
	name   string
}

func (s s3WithHealth) Name() string { return s.name }
func (s s3WithHealth) HealthCheck(ctx context.Context) error {
	_, err := s.S3Driver.List(ctx, s.bucket, "")
	return err
}
func (s s3WithHealth) Put(ctx context.Context, container, artifact string, data io.Reader, _ ...engine.PutOption) error {
	return s.S3Driver.Put(ctx, s.bucket, container+"/"+artifact, data)
}
func (s s3WithHealth) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	return s.S3Driver.Get(ctx, s.bucket, container+"/"+artifact)
}
func (s s3WithHealth) Delete(ctx context.Context, container, artifact string) error {
	return s.S3Driver.Delete(ctx, s.bucket, container+"/"+artifact)
}
func (s s3WithHealth) Exists(ctx context.Context, container, artifact string) (bool, error) {
	return s.S3Driver.Exists(ctx, s.bucket, container+"/"+artifact)
}
func (s s3WithHealth) List(ctx context.Context, container, prefix string) ([]string, error) {
	return s.S3Driver.List(ctx, s.bucket, container+"/"+prefix)
}
