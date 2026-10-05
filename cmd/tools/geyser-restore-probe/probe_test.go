package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- the S3 ETag
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeArchive is an S3 endpoint with the archive tier's states as the
// customer sees them through Vaultaire: staged (the landing zone — every
// GET works), tape (HEAD works, GET is refused: 403 InvalidObjectState, or
// 503 + Retry-After when refuse503 is set), restoring (POST ?restore was
// accepted; HEAD says ongoing-request="true" for pollsUntilReady HEADs),
// restored (HEAD says ongoing-request="false", GET works again).
type fakeArchive struct {
	mu              sync.Mutex
	data            []byte
	afterRestore    []byte // when set, the bytes served once restored
	phase           string // staged | tape | restoring | restored
	refuse503       bool
	pollsUntilReady int
	neverReady      bool
	wholeGets       int
	rangeGets       int
	heads           int
	restores        int
	restoreStatus   int
	etag            string // default "etag-1" (not an MD5)
}

func newFakeArchive(size int) *fakeArchive {
	b := make([]byte, size)
	_, _ = rand.Read(b)
	return &fakeArchive{data: b, phase: "staged", pollsUntilReady: 2}
}

func (f *fakeArchive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Query().Has("restore") {
		f.restores++
		if f.phase == "tape" {
			f.phase = "restoring"
		}
		st := f.restoreStatus
		if st == 0 {
			st = http.StatusAccepted
		}
		w.WriteHeader(st)
		return
	}
	body := f.data
	if f.phase == "restored" && f.afterRestore != nil {
		body = f.afterRestore
	}
	etag := f.etag
	if etag == "" {
		etag = "etag-1"
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("x-amz-storage-class", "GLACIER")
	switch r.Method {
	case http.MethodHead:
		f.heads++
		if f.phase == "restoring" {
			if !f.neverReady {
				f.pollsUntilReady--
			}
			if f.pollsUntilReady <= 0 && !f.neverReady {
				f.phase = "restored"
			} else {
				w.Header().Set("x-amz-restore", `ongoing-request="true"`)
			}
		}
		if f.phase == "restored" {
			w.Header().Set("x-amz-restore", `ongoing-request="false", expiry-date="Tue, 06 Oct 2026 18:00:00 GMT"`)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		rng := r.Header.Get("Range")
		if rng != "" {
			f.rangeGets++
		} else {
			f.wholeGets++
		}
		if f.phase == "tape" || f.phase == "restoring" {
			if f.refuse503 {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>SlowDown</Code><Message>this object is being brought back from cold storage; retry shortly</Message></Error>`))
				return
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidObjectState</Code><Message>This object is archived on tape. Request a restore.</Message></Error>`))
			return
		}
		if rng != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(body)))
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[:1])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeArchive) set(phase string) {
	f.mu.Lock()
	f.phase = phase
	f.mu.Unlock()
}

type probeFixture struct {
	fake   *fakeArchive
	p      *Probe
	dir    string
	sleeps int
}

func newProbeFixture(t *testing.T, size int) *probeFixture {
	t.Helper()
	f := &probeFixture{fake: newFakeArchive(size), dir: t.TempDir()}
	srv := httptest.NewServer(f.fake)
	t.Cleanup(srv.Close)
	p, err := NewProbe(Config{
		Endpoint: srv.URL, Region: "us-east-1", AccessKey: "ak", SecretKey: "sk",
		StateDir: filepath.Join(f.dir, "state"), ReportPath: filepath.Join(f.dir, "report.jsonl"),
		PollEvery: time.Second, RestoreTimeout: time.Minute, RestoreDays: 1, Baseline: true,
	})
	require.NoError(t, err)
	p.sleep = func(context.Context, time.Duration) error { f.sleeps++; return nil }
	p.logf = func(string, ...any) {}
	f.p = p
	return f
}

func (f *probeFixture) reports(t *testing.T) []Report {
	t.Helper()
	fh, err := os.Open(filepath.Join(f.dir, "report.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	defer func() { _ = fh.Close() }()
	var out []Report
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r Report
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r))
		out = append(out, r)
	}
	return out
}

var obj = Object{Bucket: "tier-archive-20261004", Key: "obj8.bin"}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestProbe_LandingZoneTakesTheBaselineOnce(t *testing.T) {
	f := newProbeFixture(t, 200_000)
	ctx := context.Background()

	res, err := f.p.Check(ctx, obj)
	require.NoError(t, err)
	assert.Equal(t, "readable", res.State)
	st, err := f.p.loadState(obj)
	require.NoError(t, err)
	assert.Equal(t, sha(f.fake.data), st.SHA256, "the bytes are hashed while they are readable: the later comparison needs them")
	assert.Equal(t, int64(200_000), st.Size)
	assert.Equal(t, 1, st.Checks)
	assert.Nil(t, f.reports(t), "no report while the object is in the landing zone")

	// The hourly check after it is one HEAD and one 1-byte range — the
	// whole object is not downloaded every hour.
	_, err = f.p.Check(ctx, obj)
	require.NoError(t, err)
	assert.Equal(t, 1, f.fake.wholeGets, "one baseline download, ever")
	assert.Equal(t, 2, f.fake.rangeGets)
	assert.Equal(t, 0, f.fake.restores)
}

func TestProbe_FirstTimeOnTapeRunsTheCustomerPath(t *testing.T) {
	f := newProbeFixture(t, 300_000)
	ctx := context.Background()
	_, err := f.p.Check(ctx, obj) // baseline
	require.NoError(t, err)
	f.fake.set("tape")

	res, err := f.p.Check(ctx, obj)
	require.NoError(t, err)
	assert.Equal(t, "reported", res.State)

	reps := f.reports(t)
	require.Len(t, reps, 1, "one report line")
	r := reps[0]
	assert.Equal(t, "tier-archive-20261004/obj8.bin", r.Object)
	assert.Equal(t, int64(300_000), r.Size)
	require.NotNil(t, r.GetRefused)
	assert.Equal(t, 403, r.GetRefused.Status)
	assert.Equal(t, "InvalidObjectState", r.GetRefused.Code)
	require.NotNil(t, r.RestoreRequested)
	assert.Equal(t, 202, r.RestoreRequested.Status)
	require.NotNil(t, r.RestoreOngoingSeenAt, "the poll saw ongoing-request=\"true\"")
	require.NotNil(t, r.RestoreReadyAt)
	assert.Contains(t, r.RestoreHeader, `ongoing-request="false"`)
	require.NotNil(t, r.GetOK)
	assert.Equal(t, 200, r.GetOK.Status)
	assert.Equal(t, sha(f.fake.data), r.GetOK.SHA256)
	require.NotNil(t, r.Identical)
	assert.True(t, *r.Identical, "the restored bytes are the bytes that went in")
	assert.Equal(t, "sha256-baseline", r.IdenticalBy)
	assert.Empty(t, r.Error)
	// Timestamps are in the order of the customer path.
	assert.False(t, r.GetRefused.At.Before(r.OnTapeSeenAt))
	assert.False(t, r.RestoreRequested.At.Before(r.GetRefused.At))
	assert.False(t, r.RestoreReadyAt.Before(r.RestoreRequested.At))
	assert.False(t, r.GetOK.At.Before(*r.RestoreReadyAt))
	assert.GreaterOrEqual(t, r.RestoreSeconds, 0.0)
	assert.Equal(t, 1, f.fake.restores)
	assert.Greater(t, f.sleeps, 0, "the poll waited between HEADs")

	// Reported once: later checks do nothing.
	res, err = f.p.Check(ctx, obj)
	require.NoError(t, err)
	assert.Equal(t, "done", res.State)
	assert.Len(t, f.reports(t), 1)
	assert.Equal(t, 1, f.fake.restores)
}

func TestProbe_Records503WithRetryAfter(t *testing.T) {
	f := newProbeFixture(t, 100_000)
	ctx := context.Background()
	_, err := f.p.Check(ctx, obj)
	require.NoError(t, err)
	f.fake.refuse503 = true
	f.fake.set("tape")

	_, err = f.p.Check(ctx, obj)
	require.NoError(t, err)
	r := f.reports(t)[0]
	require.NotNil(t, r.GetRefused)
	assert.Equal(t, 503, r.GetRefused.Status)
	assert.Equal(t, "30", r.GetRefused.RetryAfter)
	assert.Equal(t, "SlowDown", r.GetRefused.Code)
	assert.True(t, *r.Identical)
}

func TestProbe_DifferentBytesAfterRestoreIsNotIdentical(t *testing.T) {
	f := newProbeFixture(t, 100_000)
	ctx := context.Background()
	_, err := f.p.Check(ctx, obj)
	require.NoError(t, err)
	other := bytes.Repeat([]byte{7}, 100_000)
	f.fake.afterRestore = other
	f.fake.set("tape")

	_, err = f.p.Check(ctx, obj)
	require.Error(t, err, "bytes that differ after a restore fail the probe's run")
	r := f.reports(t)[0]
	require.NotNil(t, r.Identical)
	assert.False(t, *r.Identical)
	assert.Equal(t, sha(other), r.GetOK.SHA256)
	assert.Equal(t, sha(f.fake.data), r.BaselineSHA256)
}

func TestProbe_NoBaselineStillReportsTheHash(t *testing.T) {
	// First seen when already on tape: nothing to compare with — the report
	// says so (identical is null), it does not claim a match.
	f := newProbeFixture(t, 50_000)
	f.fake.set("tape")
	_, err := f.p.Check(context.Background(), obj)
	require.NoError(t, err)
	r := f.reports(t)[0]
	assert.Nil(t, r.Identical)
	assert.Empty(t, r.BaselineSHA256)
	assert.Equal(t, sha(f.fake.data), r.GetOK.SHA256)
	assert.Equal(t, int64(50_000), r.GetOK.Bytes)
}

func TestProbe_NoBaselineComparesWithAnMD5ETag(t *testing.T) {
	// An object first seen on tape has no baseline — but a single-part
	// upload's ETag is the MD5 of its bytes, which is a comparison too.
	f := newProbeFixture(t, 50_000)
	sum := md5.Sum(f.fake.data) // #nosec G401
	f.fake.etag = hex.EncodeToString(sum[:])
	f.fake.set("tape")
	_, err := f.p.Check(context.Background(), obj)
	require.NoError(t, err)
	r := f.reports(t)[0]
	require.NotNil(t, r.Identical)
	assert.True(t, *r.Identical)
	assert.Equal(t, "etag-md5", r.IdenticalBy)
	assert.Equal(t, f.fake.etag, r.GetOK.MD5)

	// And it catches different bytes.
	g := newProbeFixture(t, 50_000)
	g.fake.etag = f.fake.etag // the MD5 of OTHER bytes
	g.fake.set("tape")
	_, err = g.p.Check(context.Background(), obj)
	require.Error(t, err)
	r = g.reports(t)[0]
	require.NotNil(t, r.Identical)
	assert.False(t, *r.Identical)

	// A multipart ETag is not a hash of the bytes: no claim.
	assert.False(t, isMD5("907fbfa01a1b70844ad589dbecbb3315-4"))
	assert.True(t, isMD5("907fbfa01a1b70844ad589dbecbb3315"))
}

func TestProbe_RestoreThatNeverCompletesIsReportedAndRetried(t *testing.T) {
	f := newProbeFixture(t, 50_000)
	ctx := context.Background()
	_, err := f.p.Check(ctx, obj)
	require.NoError(t, err)
	f.fake.neverReady = true
	f.fake.set("tape")
	// The fake clock: every poll sleep advances it past the timeout.
	base := time.Now()
	f.p.now = func() time.Time { return base.Add(time.Duration(f.sleeps) * 30 * time.Second) }

	_, err = f.p.Check(ctx, obj)
	require.Error(t, err)
	reps := f.reports(t)
	require.Len(t, reps, 1)
	assert.Contains(t, reps[0].Error, "not ready")
	assert.Nil(t, reps[0].GetOK)
	st, err := f.p.loadState(obj)
	require.NoError(t, err)
	assert.False(t, st.Done, "an unfinished restore is picked up again by the next run")
	require.NotNil(t, st.OnTapeSeenAt)

	// Next run: the restore completes; the first sighting on tape is kept.
	f.fake.neverReady = false
	f.p.now = time.Now
	_, err = f.p.Check(ctx, obj)
	require.NoError(t, err)
	reps = f.reports(t)
	require.Len(t, reps, 2)
	assert.Empty(t, reps[1].Error)
	assert.True(t, *reps[1].Identical)
	assert.True(t, reps[1].OnTapeSeenAt.Equal(*st.OnTapeSeenAt), "on-tape time is the FIRST sighting")
}

func TestParseObjects(t *testing.T) {
	got, err := parseObjects("tier-archive-20261004/obj8.bin, vault-bench-20261004/dir/v256.bin")
	require.NoError(t, err)
	assert.Equal(t, []Object{{"tier-archive-20261004", "obj8.bin"}, {"vault-bench-20261004", "dir/v256.bin"}}, got)
	for _, bad := range []string{"", "nobucket", "/key", "bucket/"} {
		_, err := parseObjects(bad)
		assert.Error(t, err, bad)
	}
}

// The tool is an operator probe: the product binary never links it.
func TestProductBinaryDoesNotLinkTheProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/FairForge/vaultaire/cmd/vaultaire").Output()
	require.NoError(t, err)
	for _, line := range strings.Split(string(out), "\n") {
		assert.NotContains(t, line, "/cmd/tools/", "cmd/vaultaire links an operator tool")
	}
}
