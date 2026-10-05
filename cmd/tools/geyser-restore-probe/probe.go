package main

import (
	"context"
	"crypto/md5" // #nosec G501 -- an S3 ETag of a single-part upload IS the MD5 of the bytes; compared, not trusted
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// The probe (WP-VAULT-1 part 3). The restore path of the archive tier is
// unit-tested and untimed on prod: Geyser migrates an object from its
// landing zone to tape on its own schedule (~13 days), and until it does a
// GET simply works. The probe waits for that moment and then times the
// customer path, once:
//
//   - every run: HEAD the object (what the customer's HEAD says) and GET
//     one byte (`bytes=0-0`). The one-byte read is the signal: a HEAD cannot
//     tell the landing zone from tape (the class is GLACIER either way, and
//     x-amz-restore is absent for an object nobody has restored).
//   - readable: on the first sighting the whole object is downloaded once
//     and its SHA-256 kept in the state file — the bytes a restore must
//     give back.
//   - refused (403 InvalidObjectState for an attic object, 503 +
//     Retry-After for a Smart-demoted one): the object is on tape. GET it
//     whole and record the refusal; RestoreObject; poll HEAD for
//     x-amz-restore until `ongoing-request="false"` (or the one-byte read
//     works); GET it whole, hash it, compare. One JSON line with every
//     timestamp goes to the report file and the object is done.
//
// It talks to the customer endpoint with a customer key and nothing else.

// Object names one object to watch.
type Object struct {
	Bucket, Key string
}

func (o Object) String() string { return o.Bucket + "/" + o.Key }

// Config is everything the probe needs.
type Config struct {
	Endpoint, Region     string
	AccessKey, SecretKey string
	StateDir, ReportPath string
	PollEvery            time.Duration
	RestoreTimeout       time.Duration
	RestoreDays          int32
	// Baseline downloads the object once while it is readable, for the
	// comparison after the restore.
	Baseline bool
}

// Probe runs checks.
type Probe struct {
	cfg    Config
	client *s3.Client
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
	logf   func(string, ...any)
}

// State is the per-object state file.
type State struct {
	Object       string     `json:"object"`
	Size         int64      `json:"size"`
	ETag         string     `json:"etag"`
	SHA256       string     `json:"sha256,omitempty"`
	FirstSeenAt  time.Time  `json:"first_seen_at"`
	LastCheckAt  time.Time  `json:"last_check_at"`
	Checks       int        `json:"checks"`
	OnTapeSeenAt *time.Time `json:"on_tape_seen_at,omitempty"`
	Done         bool       `json:"done"`
}

// Step is one timed request of the customer path.
type Step struct {
	At         time.Time `json:"at"`
	Seconds    float64   `json:"seconds"`
	Status     int       `json:"status"`
	Code       string    `json:"code,omitempty"`
	RetryAfter string    `json:"retry_after,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
	MD5        string    `json:"md5,omitempty"`
	Bytes      int64     `json:"bytes,omitempty"`
}

// Report is the line written when the customer path has been run.
type Report struct {
	Object               string     `json:"object"`
	Endpoint             string     `json:"endpoint"`
	Size                 int64      `json:"size"`
	ETag                 string     `json:"etag"`
	BaselineSHA256       string     `json:"baseline_sha256,omitempty"`
	FirstSeenAt          time.Time  `json:"first_seen_at"`
	ChecksBeforeTape     int        `json:"checks_before_tape"`
	OnTapeSeenAt         time.Time  `json:"on_tape_seen_at"`
	GetRefused           *Step      `json:"get_refused,omitempty"`
	RestoreRequested     *Step      `json:"restore_requested,omitempty"`
	RestoreOngoingSeenAt *time.Time `json:"restore_ongoing_seen_at,omitempty"`
	RestoreReadyAt       *time.Time `json:"restore_ready_at,omitempty"`
	RestoreHeader        string     `json:"restore_header,omitempty"`
	// RestoreSeconds is RestoreObject accepted → readable.
	RestoreSeconds float64 `json:"restore_seconds,omitempty"`
	Polls          int     `json:"polls"`
	GetOK          *Step   `json:"get_ok,omitempty"`
	// Identical: the restored bytes are the bytes that went in — by the
	// SHA-256 baseline taken while the object was readable, else by the
	// object's ETag when that is the MD5 of a single-part upload
	// (IdenticalBy says which). Null when there is neither: the object was
	// first seen already on tape and its ETag is not an MD5.
	Identical   *bool  `json:"identical"`
	IdenticalBy string `json:"identical_by,omitempty"`
	Error       string `json:"error,omitempty"`
}

// CheckResult says what one check found.
type CheckResult struct {
	// State is readable | reported | done | error.
	State string
}

// NewProbe builds a probe. The SDK's own retries are off: the probe must
// see the first answer, and its timings must be the server's.
func NewProbe(cfg Config) (*Probe, error) {
	if cfg.Endpoint == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("endpoint, access key and secret key are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 30 * time.Second
	}
	if cfg.RestoreTimeout <= 0 {
		cfg.RestoreTimeout = 6 * time.Hour
	}
	if cfg.RestoreDays < 1 {
		cfg.RestoreDays = 1
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	client := s3.New(s3.Options{
		BaseEndpoint:     aws.String(cfg.Endpoint),
		Region:           cfg.Region,
		Credentials:      credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		UsePathStyle:     true,
		RetryMaxAttempts: 1,
	})
	return &Probe{cfg: cfg, client: client, now: time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
		logf: func(format string, args ...any) {
			fmt.Printf(time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", args...)
		}}, nil
}

func (p *Probe) statePath(o Object) string {
	sum := sha256.Sum256([]byte(o.String()))
	return filepath.Join(p.cfg.StateDir, hex.EncodeToString(sum[:8])+".json")
}

func (p *Probe) loadState(o Object) (*State, error) {
	b, err := os.ReadFile(p.statePath(o))
	if errors.Is(err, os.ErrNotExist) {
		return &State{Object: o.String()}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state file %s: %w", p.statePath(o), err)
	}
	return &st, nil
}

func (p *Probe) saveState(o Object, st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.statePath(o) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.statePath(o))
}

func (p *Probe) appendReport(r Report) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p.cfg.ReportPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(b, '\n'))
	return err
}

// wireErr reads the HTTP status, the S3 error code and Retry-After out of an
// SDK error.
func wireErr(err error) (status int, code, retryAfter string) {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		status = re.HTTPStatusCode()
		if re.Response != nil {
			retryAfter = re.Response.Header.Get("Retry-After")
		}
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code = ae.ErrorCode()
	}
	return status, code, retryAfter
}

// refused reports whether an answer means "the bytes are on tape": Glacier's
// 403 InvalidObjectState (an attic object) or a 503 (a Smart-demoted object
// being brought back, with Retry-After).
func refused(status int, code string) bool {
	return (status == 403 && code == "InvalidObjectState") || status == 503
}

// probeByte reads one byte: nil = readable.
func (p *Probe) probeByte(ctx context.Context, o Object) error {
	out, err := p.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &o.Bucket, Key: &o.Key, Range: aws.String("bytes=0-0")})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, out.Body)
	return out.Body.Close()
}

// getWhole downloads the object and hashes it.
func (p *Probe) getWhole(ctx context.Context, o Object) (Step, error) {
	st := Step{At: p.now()}
	start := time.Now()
	out, err := p.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &o.Bucket, Key: &o.Key})
	if err != nil {
		st.Seconds = time.Since(start).Seconds()
		st.Status, st.Code, st.RetryAfter = wireErr(err)
		return st, err
	}
	defer func() { _ = out.Body.Close() }()
	h := sha256.New()
	m := md5.New() // #nosec G401 -- see the import
	n, err := io.Copy(io.MultiWriter(h, m), out.Body)
	st.Seconds = time.Since(start).Seconds()
	st.Bytes = n
	if err != nil {
		return st, fmt.Errorf("read body after %d bytes: %w", n, err)
	}
	st.Status = 200
	st.SHA256 = hex.EncodeToString(h.Sum(nil))
	st.MD5 = hex.EncodeToString(m.Sum(nil))
	return st, nil
}

// Check is one visit to one object.
func (p *Probe) Check(ctx context.Context, o Object) (CheckResult, error) {
	st, err := p.loadState(o)
	if err != nil {
		return CheckResult{State: "error"}, err
	}
	if st.Done {
		return CheckResult{State: "done"}, nil
	}
	now := p.now()
	head, err := p.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &o.Bucket, Key: &o.Key})
	if err != nil {
		status, code, _ := wireErr(err)
		p.logf("%s HEAD failed: %d %s (%v)", o, status, code, err)
		return CheckResult{State: "error"}, fmt.Errorf("head %s: %w", o, err)
	}
	if st.FirstSeenAt.IsZero() {
		st.FirstSeenAt = now
	}
	st.Size, st.ETag = aws.ToInt64(head.ContentLength), strings.Trim(aws.ToString(head.ETag), `"`)
	st.LastCheckAt = now
	st.Checks++

	perr := p.probeByte(ctx, o)
	if perr == nil {
		if st.SHA256 == "" && p.cfg.Baseline {
			step, gerr := p.getWhole(ctx, o)
			if gerr != nil {
				p.logf("%s readable, baseline download failed: %v", o, gerr)
			} else {
				st.SHA256 = step.SHA256
				p.logf("%s baseline: %d bytes in %.1f s, sha256 %s", o, step.Bytes, step.Seconds, step.SHA256)
			}
		}
		p.logf("%s readable (landing zone or restored): class %s, x-amz-restore %q, check %d", o,
			head.StorageClass, aws.ToString(head.Restore), st.Checks)
		return CheckResult{State: "readable"}, p.saveState(o, st)
	}
	status, code, _ := wireErr(perr)
	if !refused(status, code) {
		p.logf("%s one-byte read failed with neither a read nor a tape refusal: %d %s", o, status, code)
		_ = p.saveState(o, st)
		return CheckResult{State: "error"}, fmt.Errorf("probe %s: %w", o, perr)
	}

	// On tape: the customer path, timed.
	if st.OnTapeSeenAt == nil {
		t := now
		st.OnTapeSeenAt = &t
	}
	if err := p.saveState(o, st); err != nil {
		return CheckResult{State: "error"}, err
	}
	p.logf("%s is on tape (%d %s) — running the customer path", o, status, code)
	rep := Report{Object: o.String(), Endpoint: p.cfg.Endpoint, Size: st.Size, ETag: st.ETag, BaselineSHA256: st.SHA256,
		FirstSeenAt: st.FirstSeenAt, ChecksBeforeTape: st.Checks - 1, OnTapeSeenAt: *st.OnTapeSeenAt}
	runErr := p.customerPath(ctx, o, &rep)
	if runErr != nil {
		rep.Error = runErr.Error()
	}
	if err := p.appendReport(rep); err != nil {
		return CheckResult{State: "error"}, fmt.Errorf("write report: %w", err)
	}
	p.logf("%s report written: restore %.0f s, identical %v, error %q", o, rep.RestoreSeconds, fmtBool(rep.Identical), rep.Error)
	if rep.GetOK != nil {
		st.Done = true // the path ran to the end; a mismatch is a finding, not a retry
		if err := p.saveState(o, st); err != nil {
			return CheckResult{State: "error"}, err
		}
	}
	if runErr != nil {
		return CheckResult{State: "error"}, runErr
	}
	return CheckResult{State: "reported"}, nil
}

func fmtBool(b *bool) string {
	if b == nil {
		return "n/a (no baseline)"
	}
	return fmt.Sprint(*b)
}

// customerPath is GET (refused) → RestoreObject → poll → GET (bytes).
func (p *Probe) customerPath(ctx context.Context, o Object, rep *Report) error {
	// 1. The GET a customer would make.
	first, err := p.getWhole(ctx, o)
	if err == nil {
		// It became readable between the one-byte read and now.
		rep.GetOK = &first
		return p.compare(rep)
	}
	if !refused(first.Status, first.Code) {
		return fmt.Errorf("the GET was neither served nor refused as archived: %d %s: %w", first.Status, first.Code, err)
	}
	rep.GetRefused = &first

	// 2. RestoreObject.
	rs := Step{At: p.now()}
	start := time.Now()
	_, err = p.client.RestoreObject(ctx, &s3.RestoreObjectInput{Bucket: &o.Bucket, Key: &o.Key,
		RestoreRequest: &types.RestoreRequest{Days: aws.Int32(p.cfg.RestoreDays)}})
	rs.Seconds = time.Since(start).Seconds()
	if err != nil {
		rs.Status, rs.Code, _ = wireErr(err)
		rep.RestoreRequested = &rs
		// A restore someone already asked for is the same wait.
		if rs.Code != "RestoreAlreadyInProgress" && rs.Status != 409 {
			return fmt.Errorf("RestoreObject: %d %s: %w", rs.Status, rs.Code, err)
		}
	} else {
		rs.Status = 202
		rep.RestoreRequested = &rs
	}

	// 3. Poll: x-amz-restore on HEAD, and the one-byte read as the proof.
	deadline := rs.At.Add(p.cfg.RestoreTimeout)
	for {
		rep.Polls++
		head, herr := p.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &o.Bucket, Key: &o.Key})
		if herr == nil {
			h := aws.ToString(head.Restore)
			if h != "" {
				rep.RestoreHeader = h
			}
			if strings.Contains(h, `ongoing-request="true"`) && rep.RestoreOngoingSeenAt == nil {
				t := p.now()
				rep.RestoreOngoingSeenAt = &t
			}
			if strings.Contains(h, `ongoing-request="false"`) || p.probeByte(ctx, o) == nil {
				t := p.now()
				rep.RestoreReadyAt = &t
				rep.RestoreSeconds = t.Sub(rs.At).Seconds()
				break
			}
		}
		if !p.now().Before(deadline) {
			return fmt.Errorf("restore not ready after %s (%d polls, last x-amz-restore %q)", p.cfg.RestoreTimeout, rep.Polls, rep.RestoreHeader)
		}
		if err := p.sleep(ctx, p.cfg.PollEvery); err != nil {
			return err
		}
	}

	// 4. The GET that must give the bytes back.
	final, err := p.getWhole(ctx, o)
	if err != nil {
		return fmt.Errorf("GET after the restore: %d %s: %w", final.Status, final.Code, err)
	}
	rep.GetOK = &final
	return p.compare(rep)
}

func (p *Probe) compare(rep *Report) error {
	if rep.GetOK.Bytes != rep.Size {
		same := false
		rep.Identical = &same
		return fmt.Errorf("restored %d bytes, HEAD says %d", rep.GetOK.Bytes, rep.Size)
	}
	if rep.BaselineSHA256 != "" {
		same := rep.GetOK.SHA256 == rep.BaselineSHA256
		rep.Identical, rep.IdenticalBy = &same, "sha256-baseline"
		if !same {
			return fmt.Errorf("restored bytes differ from the baseline: %s, was %s", rep.GetOK.SHA256, rep.BaselineSHA256)
		}
		return nil
	}
	if isMD5(rep.ETag) {
		same := rep.GetOK.MD5 == strings.ToLower(rep.ETag)
		rep.Identical, rep.IdenticalBy = &same, "etag-md5"
		if !same {
			return fmt.Errorf("restored bytes do not hash to the object's ETag: md5 %s, etag %s", rep.GetOK.MD5, rep.ETag)
		}
	}
	return nil // neither a baseline nor an MD5 ETag: identical stays null
}

// isMD5 reports whether an ETag is the MD5 of a single-part upload: 32 hex
// digits (a multipart ETag carries a "-<parts>" suffix and is not a hash of
// the bytes).
func isMD5(etag string) bool {
	if len(etag) != 32 {
		return false
	}
	_, err := hex.DecodeString(etag)
	return err == nil
}

// parseObjects reads "bucket/key,bucket/key".
func parseObjects(s string) ([]Object, error) {
	var out []Object
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		bucket, key, ok := strings.Cut(part, "/")
		if !ok || bucket == "" || key == "" {
			return nil, fmt.Errorf("object %q is not bucket/key", part)
		}
		out = append(out, Object{Bucket: bucket, Key: key})
	}
	return out, nil
}
