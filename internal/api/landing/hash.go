package landing

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SourcesHash recomputes the sha256 that build.py stamps into every file it
// generates (landing.html, the dashboard's generated/house.html): every file
// under dir except browser/, __pycache__/ and *.pyc, in os.walk order with
// sorted names (a directory's files first, then its subdirectories), each
// hashed as "<rel path>\n<bytes>\n". The generated files' guard tests compare
// this against the stamp so a hand edit, or a source edit without `make
// landing`, fails CI instead of silently drifting.
func SourcesHash(dir string) (string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && (d.Name() == "browser" || d.Name() == "__pycache__") {
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
	if err != nil {
		return "", err
	}
	sort.Slice(paths, func(i, j int) bool { return walkKey(dir, paths[i]) < walkKey(dir, paths[j]) })

	h := sha256.New()
	for _, p := range paths {
		rel, _ := filepath.Rel(dir, p)
		h.Write([]byte(filepath.ToSlash(rel) + "\n"))
		b, err := os.ReadFile(p) // #nosec G304 -- walking the generator's own source tree
		if err != nil {
			return "", err
		}
		h.Write(b)
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// walkKey orders like Python's os.walk with sorted names: all files of a
// directory before any of its subdirectories, both alphabetical.
func walkKey(root, p string) string {
	rel, _ := filepath.Rel(root, p)
	dir, file := filepath.Split(filepath.ToSlash(rel))
	return dir + "\x00" + file
}
