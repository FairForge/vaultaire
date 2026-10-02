package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R8-7: one helper builds the context of every chunk blob call; no call
// site builds its own. This reads the source: a storage call that names the
// chunk container lives in chunk_store.go or chunk_move.go, or carries
// engine.ChunkContext on the same line — and the container's name is spelled
// in two places only.
func TestChunkBlobCalls_GoThroughTheChunkStore(t *testing.T) {
	root := filepath.Join("..", "..")
	storageCall := regexp.MustCompile(`\.(Put|Get|GetRange|Delete|Exists|List|PutOn|GetOn|DeleteOn|ExistsOn)\(`)
	var offenders, literals []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for n, line := range strings.Split(string(src), "\n") {
			code := line
			if i := strings.Index(code, "//"); i >= 0 {
				code = code[:i]
			}
			if strings.Contains(code, `"_global"`) {
				switch rel {
				case filepath.Join("internal", "crypto", "gci.go"), filepath.Join("internal", "engine", "chunk_address.go"):
				default:
					literals = append(literals, rel+":"+strconv.Itoa(n+1))
				}
			}
			if !strings.Contains(code, "hunkContainer") || !storageCall.MatchString(code) {
				continue
			}
			switch rel {
			case filepath.Join("internal", "api", "chunk_store.go"), filepath.Join("internal", "api", "chunk_move.go"):
				continue
			}
			if strings.Contains(code, "ChunkContext(") {
				continue
			}
			offenders = append(offenders, rel+":"+strconv.Itoa(n+1)+": "+strings.TrimSpace(code))
		}
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, offenders, "a chunk blob call outside the chunk store: use chunkStore (internal/api) or engine.ChunkContext")
	assert.Empty(t, literals, "the chunk container's name is engine.ChunkContainer / crypto.GlobalDedupScope")
}
