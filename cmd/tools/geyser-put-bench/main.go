// cmd/tools/geyser-put-bench measures Geyser Puts through the driver: the
// streamed single PUT against the parallel multipart upload, per shape
// (part size × concurrency) and object size. Each object is a fresh
// pseudo-random stream behind a reader that cannot seek (as every Vault
// write path hands the driver), read back through the driver's Get, hashed
// both ways, then deleted (-keep leaves the multipart objects in place as
// canaries for the landing-zone → tape check).
//
// Usage (credentials from the environment, as the service reads them):
//
//	GEYSER_ACCESS_KEY=… GEYSER_SECRET_KEY=… GEYSER_BUCKET=… \
//	  geyser-put-bench -sizes 256MiB,1GiB -shapes off,default,256MiB:16:4GiB
//
// A shape is `off` (the streamed PUT), `default`, or
// `<part max>:<concurrency>[:<budget>]`. One
// JSON line per object on stdout.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

type result struct {
	Shape     string  `json:"shape"`
	Size      int64   `json:"size"`
	Key       string  `json:"key"`
	PutSec    float64 `json:"put_s"`
	PutMBps   float64 `json:"put_mb_s"`
	GetSec    float64 `json:"get_s,omitempty"`
	GetMBps   float64 `json:"get_mb_s,omitempty"`
	HashMatch bool    `json:"hash_match"`
	Kept      bool    `json:"kept,omitempty"`
	Error     string  `json:"error,omitempty"`
}

func main() {
	sizes := flag.String("sizes", "256MiB,1GiB", "object sizes, comma-separated")
	shapes := flag.String("shapes", "off,default", "`off`, `default` or <part max>:<concurrency>[:<budget>], comma-separated")
	tenant := flag.String("tenant", "_bench-mpu", "tenant prefix the objects are written under")
	container := flag.String("container", "put-bench-"+time.Now().UTC().Format("20060102T150405"), "container")
	endpoint := flag.String("endpoint", "", "Vail endpoint (default: the driver's LA1)")
	noGet := flag.Bool("no-get", false, "skip the read-back")
	keep := flag.Bool("keep", false, "leave the multipart objects in place (canaries)")
	raw := flag.Bool("raw", false, "time Vail's multipart calls one by one (-raw-part, -raw-parts, -raw-conc) instead")
	rawPart := flag.String("raw-part", "16MiB", "-raw: part size")
	rawParts := flag.String("raw-parts", "4,16", "-raw: part counts")
	rawConc := flag.String("raw-conc", "4,16", "-raw: concurrencies")
	flag.Parse()

	ak, sk, bucket := os.Getenv("GEYSER_ACCESS_KEY"), os.Getenv("GEYSER_SECRET_KEY"), os.Getenv("GEYSER_BUCKET")
	if ak == "" || sk == "" || bucket == "" {
		log.Fatal("GEYSER_ACCESS_KEY, GEYSER_SECRET_KEY and GEYSER_BUCKET are required")
	}
	if *raw {
		ps, err := parseSize(*rawPart)
		if err != nil {
			log.Fatal(err)
		}
		rawMain(ak, sk, bucket, *endpoint, "t-"+*tenant+"/"+*container, ps, ints(*rawParts), ints(*rawConc))
		return
	}
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()
	ctx := common.WithTenantID(context.Background(), *tenant)
	enc := json.NewEncoder(os.Stdout)

	for _, shape := range strings.Split(*shapes, ",") {
		shape = strings.TrimSpace(shape)
		if err := applyShape(shape); err != nil {
			log.Fatal(err)
		}
		var opts []drivers.GeyserOption
		if *endpoint != "" {
			opts = append(opts, drivers.WithGeyserEndpoint(*endpoint))
		}
		d, err := drivers.NewGeyserDriver(ak, sk, bucket, "", logger, opts...)
		if err != nil {
			log.Fatalf("driver: %v", err)
		}
		for _, sz := range strings.Split(*sizes, ",") {
			n, err := parseSize(strings.TrimSpace(sz))
			if err != nil {
				log.Fatal(err)
			}
			r := runOne(ctx, d, *container, shape, n, !*noGet)
			if r.Error == "" && *keep && shape != "off" {
				r.Kept = true
			} else {
				_ = d.Delete(ctx, *container, r.Key)
			}
			_ = enc.Encode(r)
		}
	}
}

func runOne(ctx context.Context, d *drivers.GeyserDriver, container, shape string, n int64, get bool) result {
	key := fmt.Sprintf("%s-%d.bin", strings.NewReplacer(":", "x").Replace(shape), n)
	r := result{Shape: shape, Size: n, Key: key}
	src := io.LimitReader(rand.NewChaCha8([32]byte(sha256.Sum256([]byte(key)))), n)
	h := sha256.New()
	start := time.Now()
	err := d.Put(ctx, container, key, onlyReader{io.TeeReader(src, h)}, engine.WithContentLength(n))
	r.PutSec = time.Since(start).Seconds()
	r.PutMBps = float64(n) / 1e6 / r.PutSec
	if err != nil {
		r.Error = "put: " + err.Error()
		return r
	}
	want := hex.EncodeToString(h.Sum(nil))
	if !get {
		r.HashMatch = true
		return r
	}
	start = time.Now()
	rc, err := d.Get(ctx, container, key)
	if err != nil {
		r.Error = "get: " + err.Error()
		return r
	}
	g := sha256.New()
	got, err := io.Copy(g, rc)
	_ = rc.Close()
	r.GetSec = time.Since(start).Seconds()
	r.GetMBps = float64(got) / 1e6 / r.GetSec
	if err != nil {
		r.Error = "get body: " + err.Error()
		return r
	}
	r.HashMatch = got == n && hex.EncodeToString(g.Sum(nil)) == want
	return r
}

// applyShape sets the env the driver reads at construction: `off` (the
// streamed PUT), `default` (the driver's multipart defaults), or
// `<part max>:<concurrency>[:<budget>]`.
func applyShape(shape string) error {
	for _, k := range []string{"GEYSER_MULTIPART_MIN", "GEYSER_PUT_PART_MAX", "GEYSER_PUT_CONCURRENCY", "GEYSER_PUT_BUDGET"} {
		_ = os.Unsetenv(k)
	}
	switch shape {
	case "off":
		return os.Setenv("GEYSER_MULTIPART_MIN", "off")
	case "default":
		return nil
	}
	f := strings.Split(shape, ":")
	if len(f) < 2 || len(f) > 3 {
		return fmt.Errorf("shape %q: want off, default or <part max>:<concurrency>[:<budget>]", shape)
	}
	env := map[string]string{"GEYSER_PUT_PART_MAX": f[0], "GEYSER_PUT_CONCURRENCY": f[1]}
	if len(f) == 3 {
		env["GEYSER_PUT_BUDGET"] = f[2]
	}
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

func parseSize(s string) (int64, error) {
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if strings.HasSuffix(s, u.suf) {
			s, mult = strings.TrimSuffix(s, u.suf), u.m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("not a size: %q", s)
	}
	return n * mult, nil
}

func ints(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 {
			log.Fatalf("not a count: %q", f)
		}
		out = append(out, n)
	}
	return out
}

// onlyReader hides every interface but Read.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }
