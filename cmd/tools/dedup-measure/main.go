// Command dedup-measure is the F-3 number (WP-EDGE-2 Part B): how much of a
// corpus's bytes would deduplicate ACROSS tenants and nowhere else — what a
// global convergent index (refused, contract line 5) would buy over the
// per-tenant keyed index the edge codec has. It chunks every file with the
// frozen chunker (crypto.DefaultChunker, the origin's and the Worker's
// boundaries), hashes each chunk (sha256), and reports, by tenant (the
// first path segment: a top directory, or a key's first segment), the total
// bytes, the bytes unique within each tenant, the bytes unique across all,
// and
//
//	cross_tenant_only_fraction = (Σ per-tenant unique − global unique) / total
//
// the share of the corpus only a cross-tenant index would remove. It never
// writes. Sources: -dir <path> (a local tree), or -bucket <name> -endpoint
// <url> (an S3 listing; credentials from AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY, never a flag).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/FairForge/vaultaire/internal/crypto"
)

// TenantReport is one tenant's part of a Report.
type TenantReport struct {
	Files       int   `json:"files"`
	TotalBytes  int64 `json:"total_bytes"`
	UniqueBytes int64 `json:"unique_bytes"` // distinct chunks within the tenant
}

// Report is the measurement.
type Report struct {
	Tenants                 int                      `json:"tenants"`
	Files                   int                      `json:"files"`
	TotalBytes              int64                    `json:"total_bytes"`
	PerTenantUniqueBytes    int64                    `json:"per_tenant_unique_bytes"` // Σ over tenants
	GlobalUniqueBytes       int64                    `json:"global_unique_bytes"`
	CrossTenantOnlyFraction float64                  `json:"cross_tenant_only_fraction"`
	ByTenant                map[string]*TenantReport `json:"by_tenant"`
}

// measurer accumulates chunk identities.
type measurer struct {
	chunker  *crypto.RabinChunker
	global   map[[32]byte]int64
	byTenant map[string]map[[32]byte]int64
	rep      Report
}

func newMeasurer() (*measurer, error) {
	ch, err := crypto.DefaultChunker()
	if err != nil {
		return nil, err
	}
	return &measurer{chunker: ch, global: map[[32]byte]int64{}, byTenant: map[string]map[[32]byte]int64{},
		rep: Report{ByTenant: map[string]*TenantReport{}}}, nil
}

// add chunks one file of a tenant.
func (m *measurer) add(ctx context.Context, tenant string, r io.Reader) error {
	results, err := m.chunker.ChunkContext(ctx, r)
	if err != nil {
		return err
	}
	tr := m.rep.ByTenant[tenant]
	if tr == nil {
		tr = &TenantReport{}
		m.rep.ByTenant[tenant] = tr
		m.byTenant[tenant] = map[[32]byte]int64{}
	}
	seen := m.byTenant[tenant]
	for res := range results {
		if res.Err != nil {
			return res.Err
		}
		h := sha256.Sum256(res.Chunk.Data)
		n := int64(res.Chunk.Size)
		tr.TotalBytes += n
		m.rep.TotalBytes += n
		if _, ok := seen[h]; !ok {
			seen[h] = n
			tr.UniqueBytes += n
			m.rep.PerTenantUniqueBytes += n
		}
		if _, ok := m.global[h]; !ok {
			m.global[h] = n
			m.rep.GlobalUniqueBytes += n
		}
	}
	tr.Files++
	m.rep.Files++
	return nil
}

func (m *measurer) report() Report {
	r := m.rep
	r.Tenants = len(r.ByTenant)
	if r.TotalBytes > 0 {
		r.CrossTenantOnlyFraction = float64(r.PerTenantUniqueBytes-r.GlobalUniqueBytes) / float64(r.TotalBytes)
	}
	return r
}

// measureDir measures a local tree whose top directories are the tenants.
func measureDir(dir string) (Report, error) {
	m, err := newMeasurer()
	if err != nil {
		return Report{}, err
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
		if len(parts) < 2 {
			return fmt.Errorf("%s is at the top of the tree: tenants are its top directories", rel)
		}
		f, err := os.Open(filepath.Clean(path))
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if err := m.add(context.Background(), parts[0], f); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	return m.report(), nil
}

// measureBucket measures an S3 bucket whose keys' first segments are the tenants.
func measureBucket(ctx context.Context, client *s3.Client, bucket, prefix string) (Report, error) {
	m, err := newMeasurer()
	if err != nil {
		return Report{}, err
	}
	p := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return Report{}, fmt.Errorf("list %s: %w", bucket, err)
		}
		for _, o := range page.Contents {
			key := strings.TrimPrefix(aws.ToString(o.Key), prefix)
			parts := strings.SplitN(key, "/", 2)
			if len(parts) < 2 || parts[1] == "" {
				continue // no tenant segment (or a folder marker)
			}
			out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: o.Key})
			if err != nil {
				return Report{}, fmt.Errorf("get %s: %w", aws.ToString(o.Key), err)
			}
			aerr := m.add(ctx, parts[0], out.Body)
			_ = out.Body.Close()
			if aerr != nil {
				return Report{}, fmt.Errorf("%s: %w", aws.ToString(o.Key), aerr)
			}
		}
	}
	return m.report(), nil
}

// writeTable prints the report as a table.
func (r Report) writeTable(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "tenant\tfiles\ttotal_bytes\tunique_bytes")
	names := make([]string, 0, len(r.ByTenant))
	for n := range r.ByTenant {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := r.ByTenant[n]
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", n, t.Files, t.TotalBytes, t.UniqueBytes)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\ntenants %d, files %d\ntotal_bytes %d\nper_tenant_unique_bytes %d (sum)\nglobal_unique_bytes %d\ncross_tenant_only_fraction %.4f\n",
		r.Tenants, r.Files, r.TotalBytes, r.PerTenantUniqueBytes, r.GlobalUniqueBytes, r.CrossTenantOnlyFraction)
}

func main() {
	dir := flag.String("dir", "", "a local tree whose top directories are the tenants")
	bucket := flag.String("bucket", "", "an S3 bucket whose keys' first segments are the tenants")
	endpoint := flag.String("endpoint", "", "the S3 endpoint (with -bucket)")
	region := flag.String("region", "us-east-1", "the S3 region (with -bucket)")
	prefix := flag.String("prefix", "", "only keys under this prefix (with -bucket); the tenant is the segment after it")
	tenantsBy := flag.String("tenants-by", "topdir", "how files are grouped into tenants: topdir (the first path segment) — the only rule")
	asJSON := flag.Bool("json", false, "print the report as JSON")
	flag.Parse()
	if *tenantsBy != "topdir" {
		fmt.Fprintln(os.Stderr, "-tenants-by: only topdir")
		os.Exit(2)
	}
	var rep Report
	var err error
	switch {
	case *dir != "" && *bucket == "":
		rep, err = measureDir(*dir)
	case *bucket != "" && *dir == "":
		if *endpoint == "" {
			fmt.Fprintln(os.Stderr, "-bucket needs -endpoint")
			os.Exit(2)
		}
		ak, sk := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
		if ak == "" || sk == "" {
			fmt.Fprintln(os.Stderr, "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY required (never a flag)")
			os.Exit(2)
		}
		ctx := context.Background()
		cfg, cerr := config.LoadDefaultConfig(ctx, config.WithRegion(*region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")))
		if cerr != nil {
			fmt.Fprintln(os.Stderr, "sdk config:", cerr)
			os.Exit(1)
		}
		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(*endpoint)
			o.UsePathStyle = true
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		})
		rep, err = measureBucket(ctx, client, *bucket, *prefix)
	default:
		err = errors.New("one of -dir or -bucket")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dedup-measure:", err)
		os.Exit(1)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}
	rep.writeTable(os.Stdout)
}
