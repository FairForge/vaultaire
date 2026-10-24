package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 — the ETag and the SSE-C key MD5 are MD5 by protocol
	"database/sql"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/modes"
	"github.com/FairForge/vaultaire/internal/produce"
	"github.com/FairForge/vaultaire/internal/testutil"
)

// Plan 43.4 (Stage 1a Prompt 38 Part A): the "what we can produce" page and
// the law-enforcement policy state only code facts (contract rule 8). Each
// row of internal/api/landing/produce.json is held here to the code that
// makes it true; change one and the other together.

// TestProduceAccountRowsAreTheExportsSections: "what an account holds" is
// the GDPR export — its sections in order, each with the fields its SQL
// selects.
func TestProduceAccountRowsAreTheExportsSections(t *testing.T) {
	rows := produce.All().Account
	require.Len(t, rows, len(exportSections), "one row per export section")
	for i, s := range exportSections {
		assert.Equal(t, s.Name, rows[i].Section, "row %d", i)
		assert.Equal(t, exportSectionFields(s), rows[i].Fields, "section %s", s.Name)
	}
}

// TestProduceRetentionRowsAreTheRetentionJobs: every period the page states
// is the retention job's constant — one row per policy, same table, days,
// action and scrubbed columns; the export file's life is exportTTL; a table
// stated as kept is pruned by no policy.
func TestProduceRetentionRowsAreTheRetentionJobs(t *testing.T) {
	f := produce.All()
	pol := defaultRetentionPolicies()
	require.Len(t, f.Retention, len(pol), "one row per retention policy")
	byTable := map[string]produce.RetentionRow{}
	for _, r := range f.Retention {
		byTable[r.Table] = r
	}
	for _, p := range pol {
		r, ok := byTable[p.Table]
		require.True(t, ok, "policy %s has no row", p.Table)
		assert.Equal(t, p.MaxAge, time.Duration(r.Days)*24*time.Hour, "%s period", p.Table)
		want := "delete"
		if p.Action == retentionScrubColumns {
			want = "scrub"
		}
		assert.Equal(t, want, r.Action, "%s action", p.Table)
		assert.Equal(t, p.ScrubColumns, nilIfEmpty(r.Scrub), "%s scrubbed columns", p.Table)
	}
	assert.Equal(t, retentionExportsTTL, time.Duration(f.ExportsDays)*24*time.Hour, "export file life")
	for _, k := range f.Kept {
		for _, p := range pol {
			assert.NotEqual(t, p.Table, k.Table, "%s is stated as kept but a policy prunes it", k.Table)
		}
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// TestProduceBackupsAreTheBackupScripts: the backup lives the page states
// are deploy/scripts/pg-backup.sh's (the box's find -mtime of the dumps, the
// off-box OFFBOX_KEEP_DAYS).
func TestProduceBackupsAreTheBackupScripts(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "scripts", "pg-backup.sh"))
	require.NoError(t, err)
	onBox := regexp.MustCompile(`find "\$BACKUP_DIR" -name "vaultaire_\*\.sql\.gz" -mtime \+(\d+) -delete`).FindSubmatch(b)
	offBox := regexp.MustCompile(`(?m)^OFFBOX_KEEP_DAYS=(\d+)$`).FindSubmatch(b)
	require.NotNil(t, onBox, "the on-box retention line moved")
	require.NotNil(t, offBox, "OFFBOX_KEEP_DAYS moved")
	f := produce.All()
	assert.Equal(t, string(onBox[1]), strconv.Itoa(f.Backups.OnBoxDays))
	assert.Equal(t, string(offBox[1]), strconv.Itoa(f.Backups.OffBoxDays))
}

// TestProducePITRIsTheInstallScript (Stage 1a Prompt 40): the point-in-time
// archive the page describes is deploy/scripts/pg-pitr-install.sh's — the
// vendor its endpoint names, the cipher it configures, the archive_timeout
// it sets, and how far back it reaches: PITR_RETENTION_FULL weekly fulls
// (the full timer names one day of the week).
func TestProducePITRIsTheInstallScript(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "scripts", "pg-pitr-install.sh"))
	require.NoError(t, err)
	fact := func(name string) string {
		m := regexp.MustCompile(`(?m)^` + name + `=(.+)$`).FindSubmatch(b)
		require.NotNil(t, m, "%s moved out of pg-pitr-install.sh's facts", name)
		return strings.Trim(string(m[1]), `'"`)
	}
	p := produce.All().Backups.PITR

	vendors := map[string]string{"r2.cloudflarestorage.com": "Cloudflare R2"}
	vendor, ok := vendors[fact("PITR_REPO_VENDOR_ENDPOINT")]
	require.True(t, ok, "no vendor name for endpoint %s", fact("PITR_REPO_VENDOR_ENDPOINT"))
	assert.Equal(t, vendor, p.Vendor)

	ciphers := map[string]string{"aes-256-cbc": "AES-256"}
	cipher, ok := ciphers[fact("PITR_CIPHER_TYPE")]
	require.True(t, ok, "no cipher name for %s", fact("PITR_CIPHER_TYPE"))
	assert.Equal(t, cipher, p.Cipher)

	every, err := time.ParseDuration(fact("PITR_PG_ARCHIVE_TIMEOUT"))
	require.NoError(t, err)
	assert.Equal(t, every, time.Duration(p.EverySeconds)*time.Second, "archive_timeout = how far behind the archive runs")

	assert.Regexp(t, `^(Mon|Tue|Wed|Thu|Fri|Sat|Sun) \*-\*-\* \d\d:\d\d:\d\d$`, fact("PITR_FULL_CALENDAR"), "the full backup is weekly")
	fulls, err := strconv.Atoi(fact("PITR_RETENTION_FULL"))
	require.NoError(t, err)
	assert.Equal(t, fulls*7, p.Days, "count retention of weekly fulls: the oldest restorable point is at most fulls × 7 days old")
	assert.LessOrEqual(t, p.Days, produce.All().Backups.UpToDays())

	assert.Regexp(t, `(?m)^repo1-retention-full-type=count$`, string(b), "a time retention keeps the full older than the window")
	assert.Regexp(t, `(?m)^repo1-cipher-type=\$PITR_CIPHER_TYPE$`, string(b))
	assert.NotContains(t, string(b), "vaultaire-public", "the archive never goes to the public bucket")
}

// TestPITRRetentionAlarmIsThePagesNumber: PGPITRRetentionTooLong fires when
// the oldest restorable point is one day older than the page's "up to N
// days" — the copy rule's alarm.
func TestPITRRetentionAlarmIsThePagesNumber(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "monitoring", "vaultaire-pitr.yml"))
	require.NoError(t, err)
	m := regexp.MustCompile(`time\(\) - vaultaire_pg_pitr_oldest_restorable_timestamp_seconds\) > (\d+) \* 86400`).FindSubmatch(b)
	require.NotNil(t, m, "PGPITRRetentionTooLong's threshold moved")
	assert.Equal(t, strconv.Itoa(produce.All().Backups.UpToDays()+1), string(m[1]))
}

// TestLegalBackupSentencesAreTheFacts: the privacy policy, terms, GDPR page,
// DPA and BAA state the backup periods in words of their own; every such
// sentence carries the produce file's numbers, and none of the sentences
// that stopped being true (7 days, on the production host only) is left.
func TestLegalBackupSentencesAreTheFacts(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "dashboard", "templates", "legal")
	bk := produce.All().Backups
	ageOut := regexp.MustCompile(`backups age out within (\d+) days`)
	tech := regexp.MustCompile(`(\d+) days on the production host and (\d+) days off it, encrypted, and a continuous encrypted archive for point-in-time recovery reaching back (\d+) days`)
	seenAge, seenTech := 0, 0
	for _, name := range []string{"privacy.html", "terms.html", "gdpr.html", "dpa.html", "baa.html"} {
		b, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 — the repo's own templates
		require.NoError(t, err)
		s := string(b)
		for _, m := range ageOut.FindAllStringSubmatch(s, -1) {
			seenAge++
			assert.Equal(t, strconv.Itoa(bk.UpToDays()), m[1], "%s: %q", name, m[0])
		}
		for _, m := range tech.FindAllStringSubmatch(s, -1) {
			seenTech++
			assert.Equal(t, []string{strconv.Itoa(bk.OnBoxDays), strconv.Itoa(bk.OffBoxDays), strconv.Itoa(bk.PITR.Days)}, m[1:], name)
		}
		for _, stale := range []string{"7-day retention", "off-host copies", "age out within 7 days"} {
			assert.NotContains(t, s, stale, "%s still says %q", name, stale)
		}
	}
	assert.Equal(t, 5, seenAge, "one deletion sentence per page")
	assert.Equal(t, 2, seenTech, "the DPA's and the GDPR page's backup measure")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "no go.mod above the test")
		dir = parent
	}
}

// TestProduceTablesAndColumnsExist: every table and column the page names
// exists in the migrated schema (prepared like TestSQLLiteralsMatchSchema).
func TestProduceTablesAndColumnsExist(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — make test-db", err)
	}
	f := produce.All()
	named := map[string][]string{}
	for _, r := range f.Retention {
		named[r.Table] = append(append(named[r.Table], r.Holds...), r.Scrub...)
	}
	for _, k := range f.Kept {
		named[k.Table] = append(named[k.Table], k.Holds...)
	}
	for _, c := range f.PrivateSSEC.Holds {
		named[f.PrivateSSEC.Table] = append(named[f.PrivateSSEC.Table], c.Column)
	}
	for table, cols := range named {
		q := `SELECT ` + quoteIdents(cols) + ` FROM ` + quoteIdent(table) + ` LIMIT 0`
		st, err := db.PrepareContext(context.Background(), q)
		if assert.NoError(t, err, "%s: %v", table, cols) {
			_ = st.Close()
		}
	}
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteIdents(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quoteIdent(c)
	}
	return strings.Join(q, ", ")
}

// TestProduceGatedSentencesNameRegisteredFlags: a sentence true only while a
// flag is on names that flag, registered and read globally by the pages.
func TestProduceGatedSentencesNameRegisteredFlags(t *testing.T) {
	fl := flags.New(nil, nil)
	registerFlags(fl)
	f := produce.All()
	assert.Equal(t, flagTranslogAdminReads, f.AdminReads.RequiresFlag)
	assert.Equal(t, flagTranslogPublic, f.Builds.RequiresFlag)
	for _, k := range []string{f.AdminReads.RequiresFlag, f.Builds.RequiresFlag, produce.FlagLegalLEPages} {
		assert.True(t, fl.Registered(k), k)
	}
}

// TestLegalLEPagesFlag_DefaultOffGlobalOnly: legal_le_pages is OFF by
// default and has a global row only (like translog_public).
func TestLegalLEPagesFlag_DefaultOffGlobalOnly(t *testing.T) {
	db, err := sql.Open("postgres", testutil.DSN())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		t.Skipf("test database unreachable (%v) — make test-db", err)
	}
	fl := flags.New(db, nil) // nil DB answers ErrNoDatabase before the rule
	registerFlags(fl)
	assert.False(t, fl.Enabled(produce.FlagLegalLEPages, ""))
	assert.ErrorIs(t, fl.Set(context.Background(), produce.FlagLegalLEPages, "t-1", true, "test"), flags.ErrGlobalOnly)
}

// TestProducePrivateSSECRowHoldsWhatItSays: an object written into a
// Private (SSE-C) bucket — the row holds its name, size, content type,
// metadata and the MD5 of the key in the clear, its ETag is the MD5 of the
// plaintext (a single-part write), and the key itself is in no column and
// the plaintext in no stored byte. Every column the page names is checked.
func TestProducePrivateSSECRowHoldsWhatItSays(t *testing.T) {
	f := setupVersioningFixture(t)
	f.requireKeys(t, f.bucket, true)
	plain := []byte("a holiday photo, as bytes")
	h := ssecRequiredKeyHeaders()
	h["x-amz-meta-trip"] = "paris-2026"
	h["Content-Type"] = "image/jpeg"
	w := f.s3(t, "PUT", "/"+f.bucket+"/holiday/photo.jpg", bytes.NewReader(plain), h)
	require.Equal(t, 200, w.Code, w.Body.String())

	var key, etag, ctype, meta, keyMD5, whole string
	var size int64
	var updated, accessed sql.NullTime
	require.NoError(t, f.db.QueryRow(`
		SELECT object_key, size_bytes, etag, content_type, metadata::text, ssec_key_md5, updated_at, last_accessed, row_to_json(h)::text
		  FROM object_head_cache h WHERE tenant_id = $1 AND bucket = $2`, f.tenantID, f.bucket).
		Scan(&key, &size, &etag, &ctype, &meta, &keyMD5, &updated, &accessed, &whole))
	sum := md5.Sum(plain) // #nosec G401
	checks := map[string]func(){
		"object_key":    func() { assert.Equal(t, "holiday/photo.jpg", key) },
		"size_bytes":    func() { assert.Equal(t, int64(len(plain)), size) },
		"etag":          func() { assert.Equal(t, hex.EncodeToString(sum[:]), strings.Trim(etag, `"`), "the plaintext's MD5") },
		"content_type":  func() { assert.Equal(t, "image/jpeg", ctype) },
		"metadata":      func() { assert.Contains(t, meta, "paris-2026") },
		"ssec_key_md5":  func() { assert.Equal(t, testSSECKeyMD5, keyMD5) },
		"updated_at":    func() { assert.True(t, updated.Valid) },
		"last_accessed": func() { assert.True(t, accessed.Valid) },
	}
	for _, c := range produce.All().PrivateSSEC.Holds {
		check, ok := checks[c.Column]
		if assert.True(t, ok, "the page names %s, which this test does not prove", c.Column) {
			check()
		}
	}
	assert.NotContains(t, whole, testSSECKeyB64, "the key is in no column")
	assert.NotContains(t, whole, hex.EncodeToString(testSSECKey), "the key is in no column")
	// The stored bytes are not the plaintext.
	found := false
	require.NoError(t, filepath.WalkDir(f.tempDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p) // #nosec G304 — the test's own temp dir
		require.NoError(t, err)
		found = true
		assert.False(t, bytes.Contains(b, plain), "plaintext stored at %s", p)
		return nil
	}))
	assert.True(t, found, "a blob was written")
}

// TestPrivateModeSentenceNamesWhatTheRowHolds (RP53-10): modes.json's
// Private "what we hold" must name what the SSE-C row above proves we hold
// beyond names, sizes and times — the metadata and the plaintext's MD5.
func TestPrivateModeSentenceNamesWhatTheRowHolds(t *testing.T) {
	m, ok := modes.Get(modes.Private)
	require.True(t, ok)
	s := strings.ToLower(m.ServerHolds.Text)
	for _, want := range []string{"names", "sizes", "content types", "metadata", "md5"} {
		assert.Contains(t, s, want)
	}
}
