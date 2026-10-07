package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/webdav"
)

const testPass = "bench-s3cret"

// davServer is an in-memory WebDAV server behind Basic auth — the shape of
// the Sync bridge. wrap (optional) misbehaves on purpose.
func davServer(t *testing.T, wrap func(http.Handler) http.Handler) (*httptest.Server, webdav.FileSystem) {
	t.Helper()
	fs := webdav.NewMemFS()
	var h http.Handler = &webdav.Handler{FileSystem: fs, LockSystem: webdav.NewMemLS()}
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "sync" || p != testPass {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, fs
}

// tinyArgs runs every suite in sizes a test can afford.
func tinyArgs(url, out, suites string) []string {
	return []string{
		"-url", url, "-run", suites, "-out", out,
		"-n", "6", "-small-sizes", "1KiB,5000", "-small-conc", "1,3",
		"-large-sizes", "300KiB,1MiB", "-large-conc", "1,2",
		"-range-object", "2500KiB", "-range-sizes", "1KiB,1100KiB", "-range-count", "6",
		"-consistency-n", "3",
		"-list-counts", "7,25,40", "-list-max", "30", "-list-conc", "4",
		"-parity-sizes", "64KiB,1MiB", "-parity-small-n", "12", "-parity-small-size", "3KiB", "-parity-small-conc", "4",
		"-limits-paths", "60,120,300", "-limits-folder", "-limits-folder-max", "40", "-limits-conc", "8",
		"-sample-every", "20ms",
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestSmoke_EverySuiteAgainstXNetWebDAV(t *testing.T) {
	srv, fs := davServer(t, nil)
	out := filepath.Join(t.TempDir(), "results.json")
	cfg, err := parseFlags(tinyArgs(srv.URL, out, strings.Join(append(append([]string(nil), defaultSuites...), "limits"), ",")), env(map[string]string{"WEBDAV_PASSWORD": testPass}))
	require.NoError(t, err)

	var log bytes.Buffer
	rep, err := run(context.Background(), cfg, &log)
	require.NoError(t, err)
	require.NoError(t, writeJSON(out, rep))
	var table bytes.Buffer
	printTable(&table, rep)
	t.Log("\n" + table.String())

	assert.Empty(t, rep.Mismatches, log.String())
	assert.Empty(t, rep.Errors, log.String())
	assert.NotContains(t, log.String()+table.String(), testPass, "the password is never printed")

	ops := map[string]int{}
	for _, r := range rep.Rows {
		ops[r.Suite+"/"+r.Op]++
		assert.Zero(t, r.Errors, "%+v", r)
	}
	for _, want := range []string{
		"small/PUT", "small/GET", "small/Exists", "small/Delete",
		"large/PUT", "large/GET", "large/GET TTFB", "range/GET range",
		"consistency/GET new", "consistency/GET gone",
		"listing/List drv", "listing/PROPFIND d1",
		"parity/write m", "parity/read m", "parity/degraded k", "parity/PUT", "parity/GET",
	} {
		assert.Positive(t, ops[want], want)
	}
	assert.Equal(t, 4, ops["small/PUT"], "2 sizes × 2 concurrencies")
	assert.Equal(t, 2, ops["listing/List drv"], "40 is above -list-max")
	require.NotNil(t, rep.Consistency)
	assert.Zero(t, rep.Consistency.StaleRAW+rep.Consistency.StaleOverwrite+rep.Consistency.StaleDelete)
	require.NotNil(t, rep.Limits)
	assert.Len(t, rep.Limits.Names, len(limitNames))
	require.NotNil(t, rep.Limits.Folder)
	assert.Equal(t, 40, rep.Limits.Folder.Created)
	assert.Equal(t, -1, rep.Limits.Folder.FirstFailure)
	for _, p := range rep.Limits.Paths {
		if p.Length >= 120 {
			assert.True(t, p.ReadBack, "%+v", p)
		}
	}
	require.NotNil(t, rep.Resources)

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	var back Report
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, rep.RunID, back.RunID)
	assert.NotContains(t, string(b), testPass)

	// Cleanup removed the run folder; only the empty tenant folder is left.
	assert.Contains(t, rep.Cleanup, "deleted")
	f, err := fs.OpenFile(context.Background(), "/_bench/t-bench", os.O_RDONLY, 0)
	require.NoError(t, err)
	left, err := f.Readdir(-1)
	_ = f.Close()
	require.NoError(t, err)
	assert.Empty(t, left)
}

// A server that returns other bytes than were written is reported loudly.
func TestSmoke_CorruptReadIsAMismatch(t *testing.T) {
	srv, _ := davServer(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				h.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			body := rec.Body.Bytes()
			if len(body) > 10 {
				body[10] ^= 0xff
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = io.Copy(w, bytes.NewReader(body))
		})
	})
	cfg, err := parseFlags(tinyArgs(srv.URL, "", "small,range"), env(map[string]string{"WEBDAV_PASSWORD": testPass}))
	require.NoError(t, err)
	var log bytes.Buffer
	rep, err := run(context.Background(), cfg, &log)
	require.NoError(t, err)
	assert.NotEmpty(t, rep.Mismatches)
	assert.Contains(t, log.String(), "MISMATCH")
}

func TestParseFlags(t *testing.T) {
	_, err := parseFlags(nil, env(nil))
	assert.ErrorContains(t, err, "WEBDAV_PASSWORD")
	_, err = parseFlags([]string{"-password", "x"}, env(map[string]string{"WEBDAV_PASSWORD": "p"}))
	assert.Error(t, err, "there is no password flag")

	c, err := parseFlags(nil, env(map[string]string{"WEBDAV_PASSWORD": "p"}))
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:4918", c.URL)
	assert.Equal(t, "sync", c.User)
	assert.Equal(t, "_bench", c.Root)
	assert.True(t, c.Cleanup)
	assert.Equal(t, defaultSuites, c.Suites)
	assert.NotContains(t, c.Suites, "limits")
	assert.Equal(t, []int64{4 << 10, 64 << 10, 1 << 20}, c.SmallSizes)
	assert.Equal(t, []int64{16 << 20, 256 << 20, 1 << 30}, c.LargeSizes)
	assert.Equal(t, int64(256<<20), c.RangeObject)
	assert.Equal(t, 200, c.SmallN)

	_, err = run(context.Background(), config{Password: "p", Suites: []string{"nope"}}, io.Discard)
	assert.ErrorContains(t, err, "unknown suite")
}

func TestGenerator(t *testing.T) {
	const size = 3*genBlock + 12345
	all, err := io.ReadAll(newGen(42, size))
	require.NoError(t, err)
	require.Len(t, all, size)
	for _, r := range [][2]int64{{0, 10}, {genBlock - 5, 10}, {2*genBlock + 7, genBlock}, {size - 3, 3}} {
		assert.Equal(t, all[r[0]:r[0]+r[1]], expectedRange(42, size, r[0], r[1]))
	}
	g := newGen(42, size).hashing()
	_, _ = io.CopyN(io.Discard, g, 1000)
	_, err = g.Seek(0, io.SeekStart)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, g)
	assert.Equal(t, expectedHash(42, size), g.Sum(), "a rewind restarts the hash")
	assert.NotEqual(t, all[:64], func() []byte { b, _ := io.ReadAll(newGen(43, 64)); return b }())

	for in, want := range map[string]int64{"4KiB": 4096, "16MiB": 16 << 20, "1GiB": 1 << 30, "5000": 5000, "1G": 1 << 30} {
		got, err := parseSize(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
}

// stallingServer stops reading every PUT body larger than 1 MiB after
// 62 KB and never answers (the Sync bridge, live, 2026-10-06).
func stallingServer(t *testing.T) *httptest.Server {
	release := make(chan struct{})
	srv, _ := davServer(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut && r.ContentLength > 1<<20 {
				_, _ = io.CopyN(io.Discard, r.Body, 62<<10)
				select {
				case <-r.Context().Done():
				case <-release:
				}
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	t.Cleanup(func() { close(release) }) // before the server's Close (LIFO)
	return srv
}

// A stalled PUT fails its op (the driver's idle timeout) and is counted;
// the suite carries on and the run ends.
func TestSmoke_StalledPutFailsTheOp(t *testing.T) {
	srv := stallingServer(t)
	args := append(tinyArgs(srv.URL, "", "large"), "-large-sizes", "300KiB,32MiB", "-large-conc", "1",
		"-idle-timeout", "200ms", "-retries", "2")
	cfg, err := parseFlags(args, env(map[string]string{"WEBDAV_PASSWORD": testPass}))
	require.NoError(t, err)
	assert.Equal(t, 200*time.Millisecond, cfg.IdleTimeout)
	assert.Equal(t, 2, cfg.Attempts)

	var log bytes.Buffer
	start := time.Now()
	rep, err := run(context.Background(), cfg, &log)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 30*time.Second)
	assert.Empty(t, rep.Mismatches)
	assert.NotEmpty(t, rep.Errors, "the stalled PUT is an error")
	assert.Equal(t, int64(2), rep.Stalls.UploadStalls, "one stall per attempt")
	assert.Equal(t, int64(1), rep.Stalls.Retries)
	var table bytes.Buffer
	printTable(&table, rep)
	assert.Contains(t, table.String(), "stalls: upload 2")
}

// With the driver's idle timeout off, -op-timeout still ends the op.
func TestSmoke_OpTimeoutEndsAHungOp(t *testing.T) {
	srv := stallingServer(t)
	args := append(tinyArgs(srv.URL, "", "large"), "-large-sizes", "4MiB", "-large-conc", "1",
		"-idle-timeout", "0", "-op-timeout", "300ms")
	cfg, err := parseFlags(args, env(map[string]string{"WEBDAV_PASSWORD": testPass}))
	require.NoError(t, err)

	start := time.Now()
	rep, err := run(context.Background(), cfg, io.Discard)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 30*time.Second)
	assert.Positive(t, rep.Stalls.OpTimeouts)
	assert.NotEmpty(t, rep.Errors)
}

// sharedBridges are n WebDAV servers on ONE file system (Sync's bridges all
// mount the same folder), bridge i with password "bridge-pw-<i>".
func sharedBridges(t *testing.T, n int) []string {
	t.Helper()
	fs := webdav.NewMemFS()
	h := &webdav.Handler{FileSystem: fs, LockSystem: webdav.NewMemLS()}
	var urls []string
	for i := 0; i < n; i++ {
		pass := fmt.Sprintf("bridge-pw-%d", i)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != "sync" || p != pass {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		urls = append(urls, srv.URL)
	}
	return urls
}

func TestParseFlags_Bridges(t *testing.T) {
	c, err := parseFlags([]string{"-urls", "http://127.0.0.1:4918,http://127.0.0.1:4919"},
		env(map[string]string{"WEBDAV_PASSWORDS": "a,b"}))
	require.NoError(t, err)
	assert.Equal(t, []string{"http://127.0.0.1:4918", "http://127.0.0.1:4919"}, c.URLs)
	assert.Equal(t, []string{"a", "b"}, c.Passwords)
	assert.Equal(t, "http://127.0.0.1:4918", c.URL, "the raw client uses the first bridge")
	assert.NotContains(t, c.Suites, "crossbridge", "opt-in")

	_, err = parseFlags([]string{"-urls", "http://127.0.0.1:4918,http://127.0.0.1:4919"}, env(map[string]string{"WEBDAV_PASSWORDS": "a"}))
	assert.ErrorContains(t, err, "WEBDAV_PASSWORDS")
	_, err = parseFlags([]string{"-urls", "http://127.0.0.1:4918"}, env(map[string]string{"WEBDAV_PASSWORD": "a"}))
	assert.ErrorContains(t, err, "WEBDAV_PASSWORDS")
	_, err = parseFlags([]string{"-run", "crossbridge"}, env(map[string]string{"WEBDAV_PASSWORD": "a"}))
	assert.ErrorContains(t, err, "-urls", "crossbridge needs two bridges")
}

// -urls drives the multi-bridge driver; crossbridge writes through bridge i
// and measures when bridge j sees the write, the overwrite and the delete.
func TestSmoke_MultiBridgeAndCrossbridge(t *testing.T) {
	urls := sharedBridges(t, 3)
	args := append(tinyArgs(urls[0], "", "small,consistency,crossbridge"),
		"-urls", strings.Join(urls, ","), "-crossbridge-n", "4", "-crossbridge-poll", "10ms")
	cfg, err := parseFlags(args, env(map[string]string{"WEBDAV_PASSWORDS": "bridge-pw-0,bridge-pw-1,bridge-pw-2"}))
	require.NoError(t, err)
	var log bytes.Buffer
	rep, err := run(context.Background(), cfg, &log)
	require.NoError(t, err)
	assert.Empty(t, rep.Mismatches, log.String())
	assert.Empty(t, rep.Errors, log.String())
	assert.NotContains(t, log.String(), "bridge-pw-", "no password is printed")
	ops := map[string]int{}
	for _, r := range rep.Rows {
		ops[r.Suite+"/"+r.Op] += r.N
	}
	for _, want := range []string{"crossbridge/visible new", "crossbridge/visible overwrite", "crossbridge/visible delete"} {
		assert.Equal(t, 4, ops[want], want)
	}
	assert.Positive(t, ops["small/PUT"])
	assert.Contains(t, rep.Cleanup, "deleted")
}
