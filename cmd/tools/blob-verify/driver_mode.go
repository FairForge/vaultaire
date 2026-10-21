package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

type driverOpts struct {
	name, tenant, list, keysFile, mode string
	walk                               bool
	rangeBytes                         int64
	conc, attempts, limit, sample      int
	seed                               int64
	timeout                            time.Duration
	progress                           int
}

// buildDriver builds one registered driver from the service's env, the way
// cmd/vaultaire/main.go does.
func buildDriver(name string, logger *zap.Logger) (engine.Driver, error) {
	switch name {
	case "lyve":
		region := os.Getenv("LYVE_REGION")
		if region == "" {
			region = "us-west-1"
		}
		return drivers.NewLyveDriver(os.Getenv("LYVE_ACCESS_KEY"), os.Getenv("LYVE_SECRET_KEY"), "", region, logger)
	case "idrive":
		return drivers.NewIDriveDriver(os.Getenv("IDRIVE_ACCESS_KEY"), os.Getenv("IDRIVE_SECRET_KEY"), os.Getenv("IDRIVE_ENDPOINT"), os.Getenv("IDRIVE_REGION"), logger)
	case "wasabi":
		endpoint, region, bucket := drivers.WasabiConfigFromEnv(os.Getenv)
		return drivers.NewWasabiDriver(os.Getenv("WASABI_ACCESS_KEY"), os.Getenv("WASABI_SECRET_KEY"), endpoint, region, bucket, logger)
	case "geyser":
		var opts []drivers.GeyserOption
		if ep := os.Getenv("GEYSER_ENDPOINT"); ep != "" {
			opts = append(opts, drivers.WithGeyserEndpoint(ep))
		}
		return drivers.NewGeyserDriver(os.Getenv("GEYSER_ACCESS_KEY"), os.Getenv("GEYSER_SECRET_KEY"), os.Getenv("GEYSER_BUCKET"), "", logger, opts...)
	case "permafrost":
		return drivers.NewOneDriveFleetDriver(logger)
	case "sync":
		cfg, ok, err := drivers.SyncWebDAVConfigFromEnv(os.Getenv)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("SYNC_WEBDAV_* not set")
		}
		return drivers.NewMultiWebDAVDriver("sync", cfg, logger)
	case "blomp":
		cfg, ok, err := drivers.BlompConfigFromEnv(os.Getenv)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("BLOMP_* not set")
		}
		return drivers.NewSwiftDriver("blomp", cfg, logger)
	}
	return nil, fmt.Errorf("unknown driver %q", name)
}

type driverKey struct {
	container, artifact string
	size                int64 // -1 = unknown
}

func runDriverMode(ctx context.Context, o driverOpts) {
	if o.tenant == "" {
		fmt.Fprintln(os.Stderr, "driver mode needs -tenant")
		os.Exit(2)
	}
	logger := zap.NewNop()
	d, err := buildDriver(o.name, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s driver: %v\n", o.name, err)
		os.Exit(1)
	}
	tctx := common.WithTenantID(ctx, o.tenant)

	var keys []driverKey
	switch {
	case o.walk:
		w, ok := d.(engine.TenantWalker)
		if !ok {
			fmt.Fprintf(os.Stderr, "%s does not walk tenants; use -list container\n", o.name)
			os.Exit(2)
		}
		wctx, cancel := context.WithTimeout(tctx, 30*time.Minute)
		err := w.WalkTenant(wctx, o.tenant, func(t engine.TenantObject) error {
			keys = append(keys, driverKey{t.Container, t.Artifact, -1})
			return nil
		})
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "walk:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "walked %d objects of %s on %s\n", len(keys), o.tenant, o.name)
	case o.list != "":
		container, prefix, _ := strings.Cut(o.list, "/")
		lctx, cancel := context.WithTimeout(tctx, 30*time.Minute)
		names, err := d.List(lctx, container, prefix)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "list:", err)
			os.Exit(1)
		}
		for _, n := range names {
			keys = append(keys, driverKey{container, n, -1})
		}
		fmt.Fprintf(os.Stderr, "listed %d objects under %s on %s\n", len(keys), o.list, o.name)
	case o.keysFile != "":
		lines, err := readLines(o.keysFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, l := range lines {
			f := strings.Split(l, "\t")
			if len(f) < 2 {
				fmt.Fprintf(os.Stderr, "want container<TAB>artifact[<TAB>size]: %q\n", l)
				os.Exit(2)
			}
			k := driverKey{f[0], f[1], -1}
			if len(f) > 2 {
				k.size, _ = strconv.ParseInt(f[2], 10, 64)
			}
			keys = append(keys, k)
		}
	default:
		fmt.Fprintln(os.Stderr, "driver mode needs -keys, -walk or -list")
		os.Exit(2)
	}
	byName := map[string]driverKey{}
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		n := k.container + "\t" + k.artifact
		byName[n] = k
		names = append(names, n)
	}
	names = pick(names, o.sample, o.seed, o.limit)
	fmt.Fprintf(os.Stderr, "verifying %d objects of %s on %s, mode %s, %d at a time, %d attempts\n", len(names), o.tenant, o.name, o.mode, o.conc, o.attempts)
	runAll(ctx, names, o.conc, o.progress, func(ctx context.Context, n string) result {
		return verifyThroughDriver(tctx, d, byName[n], o)
	})
}

func verifyThroughDriver(ctx context.Context, d engine.Driver, k driverKey, o driverOpts) result {
	r := result{Key: k.container + "/" + k.artifact, Size: k.size}
	start := time.Now()
	defer func() { r.MS = float64(time.Since(start)) / float64(time.Millisecond) }()
	if o.mode == "head" {
		for i := 0; i < o.attempts; i++ {
			r.Attempts++
			ectx, cancel := context.WithTimeout(ctx, o.timeout)
			ok, err := d.Exists(ectx, k.container, k.artifact)
			cancel()
			switch {
			case err == nil && ok:
				r.Outcome, r.Transient = "ok", r.Attempts-1
				return r
			case err == nil:
				r.Outcome = "missing"
				return r
			default:
				r.Code, r.Error = "driver", err.Error()
				if i+1 < o.attempts {
					time.Sleep(backoff(i))
				}
			}
		}
		r.Outcome = "head_error"
		return r
	}
	for i := 0; i < o.attempts; i++ {
		r.Attempts++
		gctx, cancel := context.WithTimeout(ctx, o.timeout)
		var rc io.ReadCloser
		var err error
		if o.mode == "range" {
			rg, ok := d.(engine.RangeGetter)
			if !ok {
				cancel()
				r.Outcome, r.Error = "get_error", "driver has no GetRange"
				return r
			}
			rc, err = rg.GetRange(gctx, k.container, k.artifact, 0, o.rangeBytes)
		} else {
			rc, err = d.Get(gctx, k.container, k.artifact)
		}
		if err == nil {
			h := sha256.New()
			n, cerr := io.Copy(h, rc)
			_ = rc.Close()
			cancel()
			r.BytesRead += n
			if cerr != nil {
				r.Code, r.Error = "body", cerr.Error()
				if i+1 < o.attempts {
					time.Sleep(backoff(i))
				}
				continue
			}
			r.Transient = r.Attempts - 1
			r.MD5 = "sha256:" + hex.EncodeToString(h.Sum(nil))
			if o.mode == "full" && k.size >= 0 && n != k.size {
				r.Outcome, r.Error = "short_body", fmt.Sprintf("read %d of %d bytes", n, k.size)
				return r
			}
			if o.mode == "range" && n == 0 && k.size != 0 {
				r.Outcome, r.Error = "short_body", "empty range"
				return r
			}
			r.Outcome, r.Code, r.Error = "ok", "", ""
			return r
		}
		cancel()
		r.Code, r.Error = "driver", err.Error()
		if isMissing(err) {
			r.Outcome = "missing"
			return r
		}
		if i+1 < o.attempts {
			time.Sleep(backoff(i))
		}
	}
	r.Outcome = "get_error"
	return r
}

func isMissing(err error) bool {
	var nf engine.NotFoundError
	var nfp *engine.NotFoundError
	if errors.As(err, &nf) || errors.As(err, &nfp) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "not found") || strings.Contains(s, "NoSuchKey") || strings.Contains(s, "404")
}
