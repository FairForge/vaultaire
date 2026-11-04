package api

// WP-VAULT-2 Part 2 PR B: the bucket page says "Every file here was read
// back and checked on <date>" once a full cycle of the blob scrub finished
// and nothing it found in the bucket is left unrepaired — "…; N files were
// repaired" when some were — and nothing while a cycle has not finished.
// The sentences are internal/api/landing/durability.json's.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/dashboard/handlers"
)

func TestDashboard_BucketSaysVerifiedAfterACleanCycle(t *testing.T) {
	assert.Equal(t, blobScrubJobName, handlers.ScrubJobName, "the page reads the job's own row")
	f := setupBlobScrubFixture(t, 24*time.Hour)
	prior := handlers.ScrubJobName
	handlers.ScrubJobName = f.job.JobName
	t.Cleanup(func() { handlers.ScrubJobName = prior })
	line := func() string { return handlers.BucketVerifiedLine(context.Background(), f.db, f.tenantID, f.bucket) }

	f.putObject("a.bin", testBytes(1000), nil)
	f.putObject("b.bin", testBytes(1000), nil)
	assert.Empty(t, line(), "never scrubbed: nothing claimed")
	// The flag was set before any cycle began (the fixture set it now).
	f.exec(`UPDATE feature_flags SET updated_at = NOW() - interval '1 hour' WHERE flag_key = $1 AND tenant_id = $2`, flagBlobScrub, f.tenantID)

	// One blob a run: the first run leaves the cycle in progress.
	f.job.Every = 12 * time.Hour
	f.run()
	assert.Empty(t, line(), "a cycle in progress claims nothing")
	res := f.run()
	require.False(t, res.Backends["edgefake"].CycleCompletedAt.IsZero())
	date := res.Backends["edgefake"].CycleCompletedAt.UTC().Format("2 January 2006")
	assert.Equal(t, "Every file here was read back and checked on "+date+".", line())

	// A blob found wrong and not repaired: nothing is claimed.
	f.exec(`INSERT INTO scrub_findings (tenant_id, bucket, object_key, address, backend, kind, verdict) VALUES ($1, $2, 'a.bin', 'a.bin@x', 'edgefake', 'whole', 'mismatch')`,
		f.tenantID, f.bucket)
	assert.Empty(t, line(), "an unrepaired finding")
	// Repaired from a second copy: said so.
	f.exec(`INSERT INTO scrub_findings (tenant_id, bucket, object_key, address, backend, kind, verdict, source) VALUES ($1, $2, 'a.bin', 'a.bin@x', 'edgefake', 'whole', 'repaired', 'edgemirror')`,
		f.tenantID, f.bucket)
	assert.Equal(t, "Every file here was read back and checked on "+date+"; 1 file was repaired from a second copy.", line())

	// A flag set AFTER the counted cycle began (mid-cycle): nothing claimed.
	f.exec(`UPDATE feature_flags SET updated_at = NOW() + interval '1 hour' WHERE flag_key = $1 AND tenant_id = $2`, flagBlobScrub, f.tenantID)
	assert.Empty(t, line(), "the tenant joined the scrub after the cycle began")
	f.exec(`UPDATE feature_flags SET updated_at = NOW() - interval '1 hour' WHERE flag_key = $1 AND tenant_id = $2`, flagBlobScrub, f.tenantID)

	// Another bucket of the tenant is not this one's findings; a tenant
	// without the flag gets nothing.
	assert.Equal(t, "Every file here was read back and checked on "+date+".",
		handlers.BucketVerifiedLine(context.Background(), f.db, f.tenantID, "other-bucket"))
	require.NoError(t, f.fl.Set(context.Background(), flagBlobScrub, f.tenantID, false, "test"))
	assert.Empty(t, line(), "out of the scrub: nothing claimed")

	// The template shows the line and never types the sentence.
	tmpl, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "dashboard", "templates", "customer", "bucket_objects.html"))
	require.NoError(t, err)
	assert.Contains(t, string(tmpl), "{{.VerifiedLine}}")
	assert.False(t, strings.Contains(string(tmpl), "read back"), "the sentence lives in durability.json")
}
