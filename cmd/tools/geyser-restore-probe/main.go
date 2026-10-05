// geyser-restore-probe waits for an archive object to leave Geyser's landing
// zone for tape and then times the customer's restore path, once
// (WP-VAULT-1 part 3; probe.go has the method). An operator tool: never
// linked into cmd/vaultaire.
//
//	source ~/vaultaire-bench/.env.bench
//	geyser-restore-probe                      # hourly, until every object is reported
//	geyser-restore-probe -once                # one visit (cron)
//
// Credentials: VAULTAIRE_BENCH_ACCESS_KEY / VAULTAIRE_BENCH_SECRET_KEY (a
// customer key of the tenant that owns the objects), else AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func main() {
	home, _ := os.UserHomeDir()
	def := filepath.Join(home, "vaultaire-bench", "restore-probe")
	endpoint := flag.String("endpoint", "https://s3.stored.ge", "the customer S3 endpoint")
	region := flag.String("region", "us-east-1", "signing region")
	objects := flag.String("objects", "tier-archive-20261004/obj8.bin,vault-bench-20261004/v256.bin", "bucket/key[,bucket/key…]")
	interval := flag.Duration("interval", time.Hour, "time between visits")
	once := flag.Bool("once", false, "one visit and exit (for cron)")
	stateDir := flag.String("state-dir", filepath.Join(def, "state"), "per-object state files")
	report := flag.String("report", filepath.Join(def, "report.jsonl"), "report file, one JSON line per timed restore")
	poll := flag.Duration("poll", 30*time.Second, "time between HEADs while a restore runs")
	timeout := flag.Duration("restore-timeout", 6*time.Hour, "give up on one restore after this (the next visit resumes)")
	days := flag.Int("days", 1, "RestoreObject Days")
	noBaseline := flag.Bool("no-baseline", false, "do not download the object while it is readable (no byte comparison after the restore)")
	flag.Parse()

	objs, err := parseObjects(*objects)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	p, err := NewProbe(Config{
		Endpoint: *endpoint, Region: *region,
		AccessKey: firstEnv("VAULTAIRE_BENCH_ACCESS_KEY", "AWS_ACCESS_KEY_ID"),
		SecretKey: firstEnv("VAULTAIRE_BENCH_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"),
		StateDir:  *stateDir, ReportPath: *report,
		PollEvery: *poll, RestoreTimeout: *timeout, RestoreDays: int32(*days), // #nosec G115 -- operator flag
		Baseline: !*noBaseline,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "geyser-restore-probe:", err, "(set VAULTAIRE_BENCH_ACCESS_KEY / VAULTAIRE_BENCH_SECRET_KEY)")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for {
		failed, pending := 0, 0
		for _, o := range objs {
			res, err := p.Check(ctx, o)
			if err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "%s: %v\n", o, err)
			}
			if res.State != "done" && res.State != "reported" {
				pending++
			}
		}
		if *once || pending == 0 || ctx.Err() != nil {
			if failed > 0 {
				os.Exit(1)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}
