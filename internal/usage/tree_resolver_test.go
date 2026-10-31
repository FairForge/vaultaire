package usage

// WP-ACCT-1 (Stage 1a Prompt 53, PR A): pool_root_id is the pool's ONE
// resolver. The expression it replaced — the COALESCE of parent and self —
// appears nowhere in the code any more, and the code that reads
// pool_root_id outside this package is a short, reasoned list.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// poolRootReaders are the files outside internal/usage that name
// pool_root_id, and why.
var poolRootReaders = map[string]string{
	"internal/account/erase.go":   "the erasure: the key handover's heir (the pool root's owner) and an erased child's bytes leaving its root's counter (the ledger's own move, in the erase transaction)",
	"internal/auth/platform.go":   "CreateChildTenant writes a child's pool_root_id (its parent's root)",
	"internal/account/service.go": "a sponsored child's detach resets its pool_root_id to itself (PR C)",
	"internal/auth/claimable.go":  "a provisional tenant takes its partner's pool root, and a claim writes the claimer's (WP-APP-1 PR D; the bytes move through usage.MovePoolTx)",
	"internal/modes/bucket.go":    "the door refuses Public for an app whose pool root is unverified (WP-APP-1 PR C): reads the root's owner, never resolves it",
}

func TestTree_PoolRootIsTheOneResolver(t *testing.T) {
	root := filepath.Join("..", "..")
	coalesce := regexp.MustCompile(`COALESCE\(\s*(\w+\.)?parent_tenant_` + `id\s*,\s*(\w+\.)?id\s*\)`)
	var stale, unlisted []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch d.Name() {
			case "vendor", ".git", "node_modules", "migrations", "edge", "sdk":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, err := os.ReadFile(path) // #nosec G304 -- the repository's own files
		if err != nil {
			return err
		}
		src := string(raw)
		if coalesce.MatchString(src) {
			stale = append(stale, rel)
		}
		if !strings.HasSuffix(path, "_test.go") && strings.Contains(src, "pool_root_"+"id") &&
			!strings.HasPrefix(filepath.ToSlash(rel), "internal/usage/") {
			if poolRootReaders[filepath.ToSlash(rel)] == "" {
				unlisted = append(unlisted, rel)
			}
		}
		return nil
	}))
	assert.Empty(t, stale, "the pool's old resolver (the COALESCE of parent and self): read the root through usage (PoolRoot, lockPoolOwner)")
	assert.Empty(t, unlisted, "pool_root_id named outside internal/usage: route the read through usage, or list the file with its reason")
}
