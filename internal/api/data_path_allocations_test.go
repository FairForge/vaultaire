package api

// WP-R2-6 (Stage 1a Prompt 14 Part C): every allocation of a megabyte or
// more that a request can make on the data path is under a budget — the
// process-wide GET budget (LARGE_GET_PARALLEL_BUDGET_BYTES), the process-wide
// PUT budget (CHUNK_PUT_BUDGET_BYTES), the multipart staging guard (disk) —
// or is a bounded per-stream working set named here with its bound. The
// test walks every `make([]byte, N)` in the data-path packages whose N is
// not a literal under 1 MiB and demands a row for it.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allocationSites: "<package>/<file>:<function>" → the budget it is under,
// or the per-request bound that makes it safe without one.
var allocationSites = map[string]string{
	// The PUT budget (CHUNK_PUT_BUDGET_BYTES, chunk_put_budget.go): every chunk's
	// buffers are held from it before the chunk is stored.
	"crypto/chunker.go:ChunkContextDepth":  "PUT budget: each chunk (≤ 16 MiB) is held before it is stored; the chunker's own look-ahead is chunkerLookahead + 1 chunks + its 16 MiB work buffer (≤ 64 MiB per write)",
	"crypto/chunk_encryption.go:sealChunk": "PUT budget: the ciphertext is part of the chunk's hold (chunk + compressed + sealed, shrunk to the real sizes)",
	"crypto/chunker.go:Chunk":              "not a request path: the whole-buffer chunker (cmd/tools/dedup-migrate); the server uses ChunkContextDepth",
	"crypto/chunker.go:ChunkBytes":         "not a request path: the whole-buffer chunker (tests, tools)",
	// The GET budget (LARGE_GET_PARALLEL_BUDGET_BYTES, s3_large_get.go).
	"api/s3_large_get.go:openParallelGet":    "GET budget: parts of the parallel ranged reader (parfetch)",
	"api/s3_large_get.go:openParallelSealed": "GET budget: segment-aligned parts of the parallel sealed reader (parfetch)",
	// Bounded per stream, under no budget (each with its bound).
	"crypto/stream.go:newStreamSealer":       "per stream ≤ 2 MiB: one 1 MiB plaintext segment and its ciphertext (WP-R8-5)",
	"crypto/stream.go:newStreamOpener":       "per stream ≤ 2 MiB: one sealed segment and its plaintext",
	"crypto/stream.go:newStreamHeaderFormat": "per stream: the 1,105-byte header",
	"crypto/stream.go:ReadStreamHeader":      "per stream: the header (crypto.MaxStreamHeader)",
	"crypto/ssec.go:SSECSeal":                "per stream ≤ 2 MiB: SSE-C on the streaming codec",
	"drivers/geyser_get_ranges.go:fetch":     "per GET ≤ GEYSER_GET_CONCURRENCY × 8 MiB (64 MiB): Geyser's ordered ranges",
	"api/vault_parity.go:Open":               "per stream: one RS stripe (8 × 1 MiB shards) — the vault parity read fallback",
	"api/vault_parity_codec.go:decodeNext":   "per stream: one stripe's shards (8 × 1 MiB)",
	"api/vault_parity_codec.go:encodeParity": "per protect: one stripe (the vault_parity job, one protect at a time)",
	"drivers/webdav.go:replayableBody":       "per PUT ≤ 8 MiB: the Sync bridge's retryable bodies (webdav_resilience.go)",
	"drivers/onedrive.go:newODChunkBuffer":   "per upload in flight: one 60 MiB Graph upload fragment (permafrost, the fleet's chunk size), pooled (odChunkPool) — no process budget: permafrost is written only by the vault parity job (one protect at a time; WP-R7-4)",
	"drivers/onedrive.go:fetchRange":         "per GET ≤ odAdaptiveStreams × 8 MiB (64 MiB): permafrost's ordered ranges through parfetch's window (WP-R7-4, R7-07: every range was read whole before)",
	// The unknown-length spool of Geyser, the fixed-bucket S3 driver and
	// permafrost (RP17-2): rare on the data path — every S3 PUT carries its
	// length; the Vault writes pass it (WP-R7-2).
	"drivers/geyser.go:materializeSpool": "per unknown-length PUT: one 64 MiB spool, allocated once (it was a bytes.Buffer grown to ~256 MiB), then the temp file; a body under 1 MiB never takes it",
	"drivers/geyser.go:materialize":      "per unknown-length PUT: the 1 MiB probe (materializeProbe)",
	// The S3 uploader (idrive, lyve, wasabi, r2, s3compat): bounded per
	// request, unbounded in number — the WP-R2-6 residual named in WP-P14.
	"drivers/s3upload.go:putSinglePartIfSmall": "per PUT ≤ 16 MiB (a body of one part), and the manager's 8 × 16 MiB parts above it: no process budget — residual (WP-P14)",
	// Small: a constant or a key-sized buffer that the walker cannot size.
	"api/blob_address.go:newBlobAddress":          "17 bytes: a blob address's random bits",
	"api/synthetic_check.go:cycle":                "a few KiB: the canary's body",
	"crypto/chunk_encryption.go:deriveChunkNonce": "12 bytes: a nonce",
	"crypto/encryption.go:Encrypt":                "legacy whole-buffer AES-GCM (the 0x01/0x02 compatibility path; no request-path writer since WP-R8-5)",
	"crypto/encryption.go:GenerateRandomKey":      "32 bytes: a key",
	"crypto/tenant_keys.go:wrapKey":               "a wrapped key",
	"drivers/wasm.go:Transform":                   "the WASM transform's buffer (inert: no caller on the data path)",
	"drivers/xattr_unix.go:GetXAttr":              "an xattr value (the local driver)",
	"drivers/xattr_unix.go:ListXAttrs":            "an xattr list (the local driver)",
}

// literalBytes evaluates a size expression made of integer literals, shifts
// and products (e.g. 32 << 10, 4 * 1024); ok is false for anything else.
func literalBytes(e ast.Expr) (int64, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(v.Value, "_", ""), 0, 64)
		return n, err == nil
	case *ast.ParenExpr:
		return literalBytes(v.X)
	case *ast.BinaryExpr:
		l, okL := literalBytes(v.X)
		r, okR := literalBytes(v.Y)
		if !okL || !okR {
			return 0, false
		}
		switch v.Op {
		case token.SHL:
			return l << uint(r), true
		case token.MUL:
			return l * r, true
		case token.ADD:
			return l + r, true
		case token.SUB:
			return l - r, true
		}
	}
	return 0, false
}

func TestDataPathBounds_EveryLargeAllocationIsUnderABudget(t *testing.T) {
	pkgs := []string{".", "../crypto", "../parfetch", "../drivers", "../engine"}
	fset := token.NewFileSet()
	found := map[string]string{}
	for _, dir := range pkgs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		require.NoError(t, err)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f) // #nosec G304 -- this repository's own sources
			require.NoError(t, err)
			file, err := parser.ParseFile(fset, f, src, 0)
			require.NoError(t, err)
			pkg := filepath.Base(filepath.Clean(filepath.Join("internal/api", dir)))
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || len(call.Args) < 2 {
						return true
					}
					if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "make" {
						return true
					}
					at, ok := call.Args[0].(*ast.ArrayType)
					if !ok || at.Len != nil {
						return true
					}
					if el, ok := at.Elt.(*ast.Ident); !ok || el.Name != "byte" {
						return true
					}
					if n, ok := literalBytes(call.Args[1]); ok && n < 1<<20 {
						return true
					}
					site := fmt.Sprintf("%s/%s:%s", pkg, filepath.Base(f), fn.Name.Name)
					found[site] = fset.Position(call.Pos()).String()
					return true
				})
			}
		}
	}
	var unknown, stale []string
	for site, pos := range found {
		if _, ok := allocationSites[site]; !ok {
			unknown = append(unknown, site+" ("+pos+")")
		}
	}
	for site := range allocationSites {
		if _, ok := found[site]; !ok {
			stale = append(stale, site)
		}
	}
	sort.Strings(unknown)
	sort.Strings(stale)
	assert.Empty(t, unknown, "a []byte allocation of unknown or ≥ 1 MiB size with no budget named for it")
	assert.Empty(t, stale, "rows for allocations that no longer exist")
}
