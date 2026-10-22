package modes

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

// K3's one write door: after a bucket's creation, buckets.visibility and
// buckets.ssec_required change only through SetBucket (docs/reviews/
// K3-PROBE.md: the writers W4 and W5 collapse onto it). A create writes
// Standard — the three INSERTs W1–W3, each with the literal 'private' and
// no ssec_required; a create asking for Private (RP33-1 c) is born Standard
// and goes through the door in its own transaction. This test reads every
// non-test Go file under internal/ and cmd/vaultaire and fails on any other
// SQL that writes either column, and on a call of the door from anywhere
// but its known callers.

var (
	updateBuckets = regexp.MustCompile(`(?is)UPDATE\s+buckets\b(.{0,600}?)(\bWHERE\b|` + "`" + `)`)
	setsModeCol   = regexp.MustCompile(`(?i)\b(visibility|ssec_required)\s*=`)
	insertBuckets = regexp.MustCompile(`(?is)INSERT\s+INTO\s+buckets\s*\(([^)]*)\)(.{0,300})`)
	callsTheDoor  = regexp.MustCompile(`\bmodes\.SetBucket\(([^\n]*\n?[^\n]*)`)
)

// doorCallers are the files that call the door: the dashboard settings, the
// management PATCH, and the create asking for Private (RP33-1 c), which may
// only require a key — never set a visibility.
var doorCallers = map[string]string{
	"internal/dashboard/handlers/bucket_settings.go": "",
	"internal/api/management_routes.go":              "",
	"internal/api/bucket_create.go":                  "modes.Change{SSECRequired: &opts.private}",
}

// modeDoor is the one file allowed to UPDATE the mode columns.
const modeDoor = "internal/modes/bucket.go"

// bucketCreates are the creates (W1–W3), each born Standard.
var bucketCreates = map[string]bool{
	"internal/api/bucket_create.go":  true,
	"internal/auth/backfill.go":      true,
	"internal/api/account_export.go": true,
}

func TestBucketModeWritersOnlyThroughTheDoor(t *testing.T) {
	root := filepath.Join("..", "..")
	seenCreates, seenDoor := map[string]bool{}, map[string]bool{}
	for _, dir := range []string{"internal", "cmd/vaultaire"} {
		require.NoError(t, filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			b, err := os.ReadFile(p)
			require.NoError(t, err)
			src := string(b)
			for _, m := range updateBuckets.FindAllStringSubmatch(src, -1) {
				if setsModeCol.MatchString(m[1]) && rel != modeDoor {
					assert.Failf(t, "a bucket's mode written outside the door",
						"%s updates buckets.visibility / ssec_required — use modes.SetBucket", rel)
				}
			}
			for _, m := range callsTheDoor.FindAllStringSubmatch(src, -1) {
				want, ok := doorCallers[rel]
				if !ok {
					assert.Failf(t, "the door called from an unknown place", "%s calls modes.SetBucket — add it to doorCallers with a reason", rel)
					continue
				}
				seenDoor[rel] = true
				if want != "" {
					assert.Contains(t, m[1], want, "%s: the create may only make the bucket Private", rel)
				}
			}
			for _, m := range insertBuckets.FindAllStringSubmatch(src, -1) {
				cols := strings.ToLower(m[1])
				if !strings.Contains(cols, "visibility") && !strings.Contains(cols, "ssec_required") {
					continue
				}
				if !bucketCreates[rel] {
					assert.Failf(t, "a bucket created outside the known creates", "%s inserts a bucket with a mode column", rel)
					continue
				}
				seenCreates[rel] = true
				assert.NotContains(t, cols, "ssec_required", "%s: a create is born Standard", rel)
				assert.Contains(t, m[2], "'private'", "%s: a create is born Standard (visibility 'private')", rel)
			}
			return nil
		}))
	}
	for f := range bucketCreates {
		assert.True(t, seenCreates[f], "%s no longer creates a bucket — remove it from bucketCreates", f)
	}
	for f := range doorCallers {
		assert.True(t, seenDoor[f], "%s no longer calls the door — remove it from doorCallers", f)
	}
}
