package api

// WP-R3-5, the rest (Stage 1a Prompt 8, Part C): multipart parts stage in
// MULTIPART_STAGING_DIR — on prod, a directory on the NVMe data disk instead
// of the 61 GiB tmpfs /tmp (RAM, wiped at boot, shared with the Sync stripe
// and the pack staging). Validated at boot (a rejected value: a Warn and the
// default); the free-space guard measures that filesystem; the reaper
// sweeps that directory; the default is today's /tmp/vaultaire-multipart.

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// useStagingDir stages this test's parts in dir.
func useStagingDir(t *testing.T, dir string) {
	t.Helper()
	old := multipartStagingDir()
	setMultipartStagingDir(dir)
	t.Cleanup(func() { setMultipartStagingDir(old) })
}

func TestStagingDir_TheDefaultIsTodays(t *testing.T) {
	assert.Equal(t, "/tmp/vaultaire-multipart", defaultMultipartStagingDir)
	assert.Equal(t, defaultMultipartStagingDir, stagingDirFromEnv(func(string) string { return "" }, zap.NewNop()))
	assert.Equal(t, filepath.Join(defaultMultipartStagingDir, "upload-0123456789abcdef0123456789abcdef"),
		multipartDir("upload-0123456789abcdef0123456789abcdef"), "nothing set: the default")
}

func TestStagingDir_ValidatedAtBoot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a-file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	readOnly := filepath.Join(root, "read-only")
	require.NoError(t, os.Mkdir(readOnly, 0o500))
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	cases := []struct {
		name, value, want string
		warn              bool
	}{
		{name: "an existing directory", value: root, want: root},
		{name: "created when absent", value: filepath.Join(root, "data", "multipart"), want: filepath.Join(root, "data", "multipart")},
		{name: "cleaned", value: " " + root + "/x/../multipart2/ ", want: filepath.Join(root, "multipart2")},
		{name: "relative", value: "data/multipart", want: defaultMultipartStagingDir, warn: true},
		{name: "a file", value: file, want: defaultMultipartStagingDir, warn: true},
		{name: "not writable", value: readOnly, want: defaultMultipartStagingDir, warn: os.Geteuid() != 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "not writable" && os.Geteuid() == 0 {
				t.Skip("root writes anywhere")
			}
			core, logs := observer.New(zap.InfoLevel)

			got := stagingDirFromEnv(func(k string) string {
				if k == "MULTIPART_STAGING_DIR" {
					return tc.value
				}
				return ""
			}, zap.New(core))

			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.warn, logs.FilterLevelExact(zap.WarnLevel).Len() == 1, "a rejected value is a Warn and the default")
			if !tc.warn {
				info, err := os.Stat(got)
				require.NoError(t, err)
				assert.True(t, info.IsDir())
				entries, _ := os.ReadDir(got)
				for _, e := range entries {
					assert.False(t, strings.HasPrefix(e.Name(), ".vaultaire-staging-probe"), "the write probe is removed")
				}
			}
		})
	}
	info, err := os.Stat(filepath.Join(root, "data", "multipart"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "created private")
}

func TestStagingDir_TheBootLogNamesTheDirectoryAndItsFreeBytesOnce(t *testing.T) {
	dir := t.TempDir()
	useStagingDir(t, defaultMultipartStagingDir)
	old := stagingFS
	var asked []string
	stagingFS = func(d string) (uint64, uint64, error) { asked = append(asked, d); return 3 << 40, 4 << 40, nil }
	t.Cleanup(func() { stagingFS = old })
	core, logs := observer.New(zap.InfoLevel)

	configureMultipartStaging(func(k string) string {
		if k == "MULTIPART_STAGING_DIR" {
			return dir
		}
		return ""
	}, zap.New(core))

	assert.Equal(t, dir, multipartStagingDir())
	lines := logs.FilterMessage("multipart staging").All()
	require.Len(t, lines, 1)
	assert.Equal(t, dir, lines[0].ContextMap()["dir"])
	assert.EqualValues(t, uint64(3<<40), lines[0].ContextMap()["free_bytes"])
	assert.Equal(t, []string{dir}, asked, "the configured directory's filesystem is the one measured")
}

func TestStagingDir_PartsStageThere_TheGuardMeasuresIt_CompleteReadsIt_TheReaperSweepsIt(t *testing.T) {
	f := setupQuotaAccountingFixture(t, 1<<30)
	dir := filepath.Join(t.TempDir(), "multipart")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	useStagingDir(t, dir)
	f.server.multipartStagingMinFree = defaultStagingMinFree
	old := stagingFS
	var asked []string
	free := uint64(1500 << 20)
	stagingFS = func(d string) (uint64, uint64, error) { asked = append(asked, d); return free, 2 << 30, nil } // a 2 GiB tmpfs-like disk
	t.Cleanup(func() { stagingFS = old })

	id := f.createUpload(t, "big.bin", nil)
	part := bytes.Repeat([]byte("p"), 64*kib)
	require.Equal(t, http.StatusOK, f.uploadPart("big.bin", id, 1, bytes.NewReader(part), int64(len(part))).Code)

	_, err := os.Stat(filepath.Join(dir, id, "part-00001"))
	assert.NoError(t, err, "staged in MULTIPART_STAGING_DIR")
	_, err = os.Stat(filepath.Join(defaultMultipartStagingDir, id))
	assert.True(t, os.IsNotExist(err), "nothing under /tmp")
	require.NotEmpty(t, asked)
	for _, d := range asked {
		assert.Equal(t, dir, d, "the guard measures the staging directory's filesystem")
	}

	free = 900 << 20 // below half of the 2 GiB filesystem
	assert.Equal(t, http.StatusServiceUnavailable, f.putPart(t, "big.bin", id, 2, 64*kib).Code)
	free = 1500 << 20

	w := f.completeReq("big.bin", id, `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>`+
		fmt.Sprintf(`"%s"`, md5hex(part))+`</ETag></Part></CompleteMultipartUpload>`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res CompleteMultipartUploadResult
	require.NoError(t, xml.Unmarshal(w.Body.Bytes(), &res))

	orphan := f.createUpload(t, "orphan.bin", nil)
	require.Equal(t, http.StatusOK, f.putPart(t, "orphan.bin", orphan, 1, kib).Code)
	_, err = f.db.ExecContext(context.Background(), `DELETE FROM multipart_uploads WHERE upload_id = $1`, orphan)
	require.NoError(t, err)
	stale := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, orphan), stale, stale))

	r, err := NewMultipartReaper(f.db, zap.NewNop()).RunOnce(context.Background())
	require.NoError(t, err)

	assert.GreaterOrEqual(t, r.OrphanDirs, 1)
	_, err = os.Stat(filepath.Join(dir, orphan))
	assert.True(t, os.IsNotExist(err), "the reaper sweeps the configured directory")
}
