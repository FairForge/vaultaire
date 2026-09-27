package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// landing.html is GENERATED from internal/api/landing/ (make landing). The
// generator stamps a sha256 of its sources into the file's first comment;
// this test recomputes it so a hand edit to landing.html, or a source edit
// without a rebuild, fails CI instead of silently drifting.
func TestLanding_GeneratedFromSources(t *testing.T) {
	stamp := regexp.MustCompile(`sources sha256:([0-9a-f]{64})`).FindStringSubmatch(landingTemplateSrc)
	require.Len(t, stamp, 2, "landing.html must carry the generator's sha256 stamp; run `make landing`")

	root := filepath.Join("landing")
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (d.Name() == "browser" || d.Name() == "__pycache__") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".pyc") {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	require.NoError(t, err)
	// os.walk in the generator sorts per directory and recurses depth-first;
	// mirror that order exactly: files of a directory first, then subdirs.
	sort.Slice(paths, func(i, j int) bool { return walkKey(root, paths[i]) < walkKey(root, paths[j]) })

	h := sha256.New()
	for _, p := range paths {
		rel, _ := filepath.Rel(root, p)
		h.Write([]byte(filepath.ToSlash(rel) + "\n"))
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		h.Write(b)
		h.Write([]byte("\n"))
	}
	require.Equal(t, stamp[1], hex.EncodeToString(h.Sum(nil)),
		"internal/api/landing/ sources changed but landing.html was not rebuilt (or was edited by hand); run `make landing`")
}

// walkKey orders like Python's os.walk with sorted names: all files of a
// directory before any of its subdirectories, both alphabetical.
func walkKey(root, p string) string {
	rel, _ := filepath.Rel(root, p)
	dir, file := filepath.Split(filepath.ToSlash(rel))
	return dir + "\x00" + file
}
