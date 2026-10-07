// cmd/tools/webdav-bench — benchmark a WebDAV endpoint for Stored's
// workloads, the Sync.com bridge (`sync-webdav`) first.
//
// Object traffic goes through the real driver (drivers.NewWebDAVDriver with
// a tenant in the context), so MKCOL caching, the post-PUT size check, Range
// handling and the PROPFIND walk are on the measured path; raw WebDAV
// requests are used where the driver hides what is measured (a Depth 1
// PROPFIND on its own, exact names and deep paths for the limits probes,
// zero-byte files for the folder limit, the run folder's recursive DELETE).
// Every read is byte-verified (sha256 against what was generated); a
// difference is printed as MISMATCH and the exit status is 2.
//
// Usage (on the bridge's host):
//
//	WEBDAV_PASSWORD=$(cat ~/.config/sync-webdav/password) \
//	  ./webdav-bench -run small,large,range,consistency,parity -out results.json
//
// Several bridges of one folder (Sync: one bridge process per device
// profile) are driven through the multi-bridge driver with -urls and
// WEBDAV_PASSWORDS (comma-separated, same order); the opt-in crossbridge
// suite writes through bridge i and measures when bridge j sees the write,
// the overwrite and the delete:
//
//	WEBDAV_PASSWORDS=p0,p1,p2,p3,p4 ./webdav-bench \
//	  -urls http://127.0.0.1:4918,http://127.0.0.1:4919,http://127.0.0.1:4920,http://127.0.0.1:4921,http://127.0.0.1:4922 \
//	  -run small,large,crossbridge -out multi.json
//
// The password is read from WEBDAV_PASSWORD(S) only (never a flag, never
// printed). Everything is written under <root>/t-<tenant>/run-<timestamp>/
// and deleted at the end (-cleanup=false keeps it). See cmd/tools/README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/FairForge/vaultaire/internal/drivers"
)

func main() {
	cfg, err := parseFlags(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "webdav-bench:", err)
		os.Exit(64)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep, err := run(ctx, cfg, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "webdav-bench:", err)
		os.Exit(1)
	}
	fmt.Println()
	printTable(os.Stdout, rep)
	if cfg.Out != "" {
		if err := writeJSON(cfg.Out, rep); err != nil {
			fmt.Fprintln(os.Stderr, "webdav-bench:", err)
			os.Exit(1)
		}
		fmt.Println("results:", cfg.Out)
	}
	switch {
	case len(rep.Mismatches) > 0:
		os.Exit(2)
	case len(rep.Errors) > 0:
		os.Exit(1)
	}
}

// parseFlags builds the config from args and the environment.
func parseFlags(args []string, getenv func(string) string) (config, error) {
	fs := flag.NewFlagSet("webdav-bench", flag.ContinueOnError)
	var c config
	fs.StringVar(&c.URL, "url", "http://127.0.0.1:4918", "WebDAV server URL (no credentials)")
	urls := fs.String("urls", "", "comma-separated bridge URLs of ONE folder: drive the multi-bridge driver (passwords from WEBDAV_PASSWORDS, same order); overrides -url")
	fs.IntVar(&c.CrossN, "crossbridge-n", 20, "crossbridge: rounds (write via bridge i, wait until bridge j sees it; then overwrite, then delete)")
	crossSize := fs.String("crossbridge-size", "64KiB", "crossbridge: object size")
	fs.DurationVar(&c.CrossPoll, "crossbridge-poll", 250*time.Millisecond, "crossbridge: poll interval on the reading bridge")
	fs.DurationVar(&c.CrossTimeout, "crossbridge-timeout", 10*time.Minute, "crossbridge: give up on one visibility wait after this long")
	fs.IntVar(&c.CrossConc, "crossbridge-conc", 4, "crossbridge: rounds in parallel")
	fs.IntVar(&c.LargeConcurrency, "large-concurrency", 0, "multi-bridge driver: transfers ≥ 16 MiB per bridge and direction (0 = the default, 3; SYNC_WEBDAV_LARGE_CONCURRENCY in the server)")
	fs.StringVar(&c.User, "user", "sync", "Basic auth user (password from WEBDAV_PASSWORD)")
	fs.StringVar(&c.Root, "root", "_bench", "folder under the URL the bench writes in")
	fs.StringVar(&c.Tenant, "tenant", "bench", "driver tenant (folder t-<tenant> under -root)")
	runList := fs.String("run", strings.Join(defaultSuites, ","), "suites: "+strings.Join(allSuites, ",")+" (limits and crossbridge are opt-in; crossbridge needs -urls)")
	fs.StringVar(&c.Out, "out", "", "JSON results file")
	fs.BoolVar(&c.Cleanup, "cleanup", true, "delete the run folder at the end")
	fs.Int64Var(&c.Seed, "seed", 1, "data generator seed")

	smallSizes := fs.String("small-sizes", "4KiB,64KiB,1MiB", "small: object sizes")
	smallConc := fs.String("small-conc", "1,8,32", "small: concurrencies")
	fs.IntVar(&c.SmallN, "n", 200, "small: objects per size×concurrency cell")
	largeSizes := fs.String("large-sizes", "16MiB,256MiB,1GiB", "large: object sizes")
	largeConc := fs.String("large-conc", "1,4", "large: concurrencies (= objects per cell)")
	rangeObj := fs.String("range-object", "256MiB", "range: object size")
	rangeSizes := fs.String("range-sizes", "64KiB,4MiB", "range: read sizes")
	fs.IntVar(&c.RangeCount, "range-count", 100, "range: random reads per size")
	fs.IntVar(&c.ConsistencyN, "consistency-n", 50, "consistency: rounds (PUT v1, GET, PUT v2, GET, DELETE, GET)")
	listCounts := fs.String("list-counts", "1000,10000", "listing: files in one folder at each listing (cumulative)")
	fs.IntVar(&c.ListMax, "list-max", 10000, "listing: skip counts above this")
	fs.IntVar(&c.ListConc, "list-conc", 32, "listing: concurrency while creating files")
	limitPaths := fs.String("limits-paths", "200,248,249,300,1000", "limits: total decoded path lengths to probe")
	fs.BoolVar(&c.LimitsFolder, "limits-folder", false, "limits: also fill one folder up to -limits-folder-max files (slow)")
	fs.IntVar(&c.LimitsFolderMax, "limits-folder-max", 50001, "limits: files for the folder probe")
	fs.IntVar(&c.LimitsConc, "limits-conc", 64, "limits: concurrency of the folder probe")
	paritySizes := fs.String("parity-sizes", "64MiB,256MiB,1GiB", "parity: object sizes (shard = size/k)")
	fs.IntVar(&c.ParityK, "parity-k", 4, "parity: data shards")
	fs.IntVar(&c.ParityM, "parity-m", 4, "parity: parity shards written")
	fs.IntVar(&c.ParitySmallN, "parity-small-n", 1000, "parity: small shards per layout (0 = skip)")
	paritySmallSize := fs.String("parity-small-size", "1MiB", "parity: small shard size")
	fs.IntVar(&c.ParitySmallConc, "parity-small-conc", 16, "parity: small shard concurrency")
	fs.StringVar(&c.Proc, "proc", "sync-webdav", "process to sample from /proc (same host only; empty = off)")
	fs.StringVar(&c.SpillDir, "spill-dir", "", "directory whose size is sampled (the bridge's --upload-temp-dir)")
	fs.DurationVar(&c.SampleEvery, "sample-every", time.Second, "resource sampling interval")
	fs.DurationVar(&c.IdleTimeout, "idle-timeout", drivers.WebDAVDefaultIdleTimeout, "driver: fail a transfer the server makes no progress on for this long (0 = off)")
	fs.DurationVar(&c.PutTimeout, "put-timeout", drivers.WebDAVDefaultPutTimeout, "driver: PUT deadline per started 64 MiB (0 = off)")
	fs.IntVar(&c.MaxConcurrency, "max-concurrency", drivers.WebDAVDefaultMaxConcurrency, "driver: requests in flight to the server (SYNC_WEBDAV_MAX_CONCURRENCY in the server)")
	fs.IntVar(&c.Attempts, "retries", drivers.WebDAVDefaultAttempts, "driver: attempts for a transient failure (1 = no retry)")
	fs.DurationVar(&c.OpTimeout, "op-timeout", 30*time.Minute, "fail one operation (and one raw request) after this long (0 = off)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments %v (the password is read from WEBDAV_PASSWORD only)", fs.Args())
	}
	if *urls != "" {
		for _, u := range strings.Split(*urls, ",") {
			c.URLs = append(c.URLs, strings.TrimSpace(u))
		}
		if pw := getenv("WEBDAV_PASSWORDS"); pw != "" {
			c.Passwords = strings.Split(pw, ",")
		}
		if len(c.Passwords) != len(c.URLs) {
			return config{}, fmt.Errorf("-urls names %d bridges: set WEBDAV_PASSWORDS to as many comma-separated passwords (it has %d)", len(c.URLs), len(c.Passwords))
		}
		c.URL, c.Password = c.URLs[0], c.Passwords[0]
	} else {
		c.Password = getenv("WEBDAV_PASSWORD")
		if c.Password == "" {
			return config{}, fmt.Errorf("set WEBDAV_PASSWORD")
		}
	}

	var err error
	if c.CrossSize, err = parseSize(*crossSize); err != nil {
		return config{}, err
	}
	for _, s := range strings.Split(*runList, ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.Suites = append(c.Suites, s)
		}
	}
	sizes := []struct {
		dst *[]int64
		src string
	}{{&c.SmallSizes, *smallSizes}, {&c.LargeSizes, *largeSizes}, {&c.RangeSizes, *rangeSizes}, {&c.ParitySizes, *paritySizes}}
	for _, s := range sizes {
		if *s.dst, err = parseSizes(s.src); err != nil {
			return config{}, err
		}
	}
	ints := []struct {
		dst *[]int
		src string
	}{{&c.SmallConc, *smallConc}, {&c.LargeConc, *largeConc}, {&c.ListCounts, *listCounts}, {&c.LimitPaths, *limitPaths}}
	for _, s := range ints {
		if *s.dst, err = parseInts(s.src); err != nil {
			return config{}, err
		}
	}
	if c.RangeObject, err = parseSize(*rangeObj); err != nil {
		return config{}, err
	}
	if c.ParitySmallSize, err = parseSize(*paritySmallSize); err != nil {
		return config{}, err
	}
	if c.ParityK < 1 || c.ParityM < 1 {
		return config{}, fmt.Errorf("-parity-k and -parity-m must be ≥ 1")
	}
	if c.MaxConcurrency < 1 || c.Attempts < 1 || c.IdleTimeout < 0 || c.PutTimeout < 0 || c.OpTimeout < 0 {
		return config{}, fmt.Errorf("-max-concurrency and -retries must be ≥ 1, the timeouts ≥ 0")
	}
	if contains(c.Suites, "crossbridge") && len(c.URLs) < 2 {
		return config{}, fmt.Errorf("the crossbridge suite needs -urls with two bridges or more")
	}
	if c.CrossN < 1 || c.CrossConc < 1 || c.CrossPoll <= 0 || c.CrossTimeout <= 0 || c.LargeConcurrency < 0 {
		return config{}, fmt.Errorf("-crossbridge-n/-conc must be ≥ 1, -crossbridge-poll/-timeout > 0, -large-concurrency ≥ 0")
	}
	if c.SampleEvery <= 0 {
		c.SampleEvery = time.Second
	}
	return c, nil
}
