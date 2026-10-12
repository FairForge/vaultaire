package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-R2-1 (2B): every read, check, move or delete of a whole object's bytes
// in internal/api names the blob's ADDRESS — objectBlobAddress /
// blobArtifact of the row's storage_key — never the raw object key. This
// reads the source, like TestChunkBlobCalls_GoThroughTheChunkStore: a storage
// call whose artifact argument is a bare key/object/artifact variable fails
// the test unless it is one of the sites listed below with the reason it may.
//
// The writers are listed: they write IN PLACE at <key> in this build and 2C
// gives each its own address (this list is 2C's checklist). Calls split over
// several lines are not seen — keep the artifact on the call's line.
func TestWholeObjectReads_GoThroughTheBlobAddress(t *testing.T) {
	// call name → index of the artifact argument.
	artifactArg := map[string]int{
		"Get": 2, "GetRange": 2, "GetRangeInfo": 2, "Exists": 2, "Delete": 2, "Put": 2,
		"RestoreObject": 2, "RestoreStatus": 2, "ParallelRangeSource": 2,
		"GetOn": 3, "ExistsOn": 3, "DeleteOn": 3, "PutOn": 3,
		"HintBackend": 1,
	}
	call := regexp.MustCompile(`\.(Get|GetRange|GetRangeInfo|Exists|Delete|Put|RestoreObject|RestoreStatus|ParallelRangeSource|GetOn|ExistsOn|DeleteOn|PutOn|HintBackend)\(`)
	rawKey := regexp.MustCompile(`^(\w+\.)?(key|object|artifact|srcKey|destKey|Key|Object|objectKey)$`)

	// file → trimmed code fragments allowed to pass a raw key, and why.
	allowed := map[string]map[string]string{
		"object_write_shared.go": {
			"drv.Put(ctx, container, key, body, opts...)":  "writer (placeObject): in place at <key> until 2C",
			"ce.HintBackend(container, key, regionDriver)": "writer (placeObject)",
			"eng.Put(ctx, container, key, body, opts...)":  "writer (placeObject)",
		},
		"s3_engine_adapter.go": {
			"a.engine.Delete(ctx, t.NamespaceContainer(bucket), artifact)":              "chunked PUT: the legacy whole blob at <key> (a displaced addressed blob is retired in the upsert tx)",
			"ce.HintBackend(t.NamespaceContainer(bucket), artifact, displaced.Backend)": "chunked PUT: the same legacy <key> blob",
		},
		"s3_copy.go": {
			"s.engine.HintBackend(t.NamespaceContainer(destBucket), destKey, displaced.Backend)": "chunked copy: the legacy whole blob at <key>",
			"s.engine.Delete(pctx, t.NamespaceContainer(destBucket), destKey)":                   "chunked copy: the legacy whole blob at <key>",
		},
		"direct_upload.go": {
			"drv.Delete(ctx, container, req.Key)": "direct upload over quota: the bytes the client just PUT at <key> on R2 (writer)",
		},
		"backend_canary.go": {
			"drv.Exists(cctx, probeCanaryContainer, key)":                                                                 "the probe canary, not a customer object",
			"drv.Put(cctx, probeCanaryContainer, key, bytes.NewReader(body), engine.WithContentLength(int64(len(body))))": "the probe canary",
		},
	}

	var offenders []string
	used := map[string]bool{}
	seen := 0 // storage calls inspected: the walk saw the package
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) // #nosec G304 -- the package's own sources
		require.NoError(t, err)
		for n, line := range strings.Split(string(src), "\n") {
			code := line
			if i := strings.Index(code, "//"); i >= 0 {
				code = code[:i]
			}
			if strings.Contains(code, "hunkContainer") || strings.Contains(code, "shard") || strings.Contains(code, "parityContainer") {
				continue // chunk blobs (chunk store) and parity shards have their own addresses
			}
			for _, loc := range call.FindAllStringSubmatchIndex(code, -1) {
				name := code[loc[2]:loc[3]]
				argList, ok := callArgs(code[loc[1]:])
				if !ok {
					continue
				}
				seen++
				args := splitArgs(argList)
				idx := artifactArg[name]
				if idx >= len(args) || !rawKey.MatchString(strings.TrimSpace(args[idx])) {
					continue
				}
				frag := receiverCall(code, code[loc[0]:loc[1]]+argList+")")
				if why, ok := allowed[path][frag]; ok && why != "" {
					used[path+": "+frag] = true
					continue
				}
				offenders = append(offenders, path+":"+strconv.Itoa(n+1)+": "+frag)
			}
		}
	}
	assert.Greater(t, seen, 40, "the walk saw the package's storage calls")
	sort.Strings(offenders)
	assert.Empty(t, offenders, "a whole-object storage call on a raw key: resolve the address (objectBlobAddress / blobArtifact of the row's storage_key), or list the site here with why it may")
	var stale []string
	for f, frags := range allowed {
		for frag := range frags {
			if !used[f+": "+frag] {
				stale = append(stale, f+": "+frag)
			}
		}
	}
	sort.Strings(stale)
	assert.Empty(t, stale, "allowed sites that no longer exist: remove them from the list")
}

// receiverCall is the call with its receiver ("drv.Put(…)") as it appears
// in code.
func receiverCall(code, match string) string {
	i := strings.Index(code, match)
	start := i
	for start > 0 {
		c := code[start-1]
		if c == '.' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			start--
			continue
		}
		break
	}
	return strings.TrimSpace(code[start : i+len(match)])
}

// callArgs is the argument list of a call whose "(" was just consumed, up to
// its matching ")"; false when the call does not close on this line.
func callArgs(rest string) (string, bool) {
	depth := 1
	for i, c := range rest {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return rest[:i], true
			}
		}
	}
	return "", false
}

// splitArgs splits a call's argument list at its top-level commas.
func splitArgs(s string) []string {
	var out []string
	depth, last := 0, 0
	for i, c := range s {
		switch c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	return append(out, s[last:])
}
