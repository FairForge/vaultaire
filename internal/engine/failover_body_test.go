package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// partialConsumeDriver reads part of the body, then fails like a dropped
// connection — the shape of a real mid-upload backend failure.
type partialConsumeDriver struct {
	name    string
	consume int64
	calls   int
}

func (d *partialConsumeDriver) Name() string { return d.name }

func (d *partialConsumeDriver) Put(_ context.Context, _, _ string, data io.Reader, _ ...PutOption) error {
	d.calls++
	_, _ = io.CopyN(io.Discard, data, d.consume)
	return fmt.Errorf("write tcp: connection reset by peer")
}

func (d *partialConsumeDriver) Get(_ context.Context, _, _ string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}
func (d *partialConsumeDriver) Delete(_ context.Context, _, _ string) error { return nil }
func (d *partialConsumeDriver) List(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}
func (d *partialConsumeDriver) Exists(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (d *partialConsumeDriver) HealthCheck(_ context.Context) error { return nil }

// recordingDriver stores what it receives in memory, like a backend that does
// not validate Content-Length (e.g. the local driver).
type recordingDriver struct {
	name  string
	calls int
	got   []byte
}

func (d *recordingDriver) Name() string { return d.name }

func (d *recordingDriver) Put(_ context.Context, _, _ string, data io.Reader, _ ...PutOption) error {
	d.calls++
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	d.got = b
	return nil
}

func (d *recordingDriver) Get(_ context.Context, _, _ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(d.got)), nil
}
func (d *recordingDriver) Delete(_ context.Context, _, _ string) error           { return nil }
func (d *recordingDriver) List(_ context.Context, _, _ string) ([]string, error) { return nil, nil }
func (d *recordingDriver) Exists(_ context.Context, _, _ string) (bool, error)   { return false, nil }
func (d *recordingDriver) HealthCheck(_ context.Context) error                   { return nil }

// lengthValidatingDriver rejects bodies shorter than the declared
// ContentLength, like every real S3-compatible backend.
type lengthValidatingDriver struct {
	name  string
	calls int
}

func (d *lengthValidatingDriver) Name() string { return d.name }

func (d *lengthValidatingDriver) Put(_ context.Context, _, _ string, data io.Reader, opts ...PutOption) error {
	d.calls++
	options := ApplyPutOptions(opts...)
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	if options.ContentLength > 0 && int64(len(b)) != options.ContentLength {
		return fmt.Errorf("write tcp: broken pipe (body %d bytes, declared %d)",
			len(b), options.ContentLength)
	}
	return nil
}

func (d *lengthValidatingDriver) Get(_ context.Context, _, _ string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}
func (d *lengthValidatingDriver) Delete(_ context.Context, _, _ string) error { return nil }
func (d *lengthValidatingDriver) List(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}
func (d *lengthValidatingDriver) Exists(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (d *lengthValidatingDriver) HealthCheck(_ context.Context) error { return nil }

// nonSeekable hides any Seek method the underlying reader may have — the
// shape of a streamed client body.
type nonSeekable struct{ io.Reader }

// TestEnginePut_SeekableBodyRewindsOnFailover: chunk stores hand the engine a
// *bytes.Reader. When the first backend dies mid-upload, the retry must
// rewind and deliver the FULL body to the next backend — not the drained
// remainder.
func TestEnginePut_SeekableBodyRewindsOnFailover(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "idrive"})
	flaky := &partialConsumeDriver{name: "idrive", consume: 5}
	healthy := &recordingDriver{name: "lyve"}
	eng.AddDriver("idrive", flaky)
	eng.AddDriver("lyve", healthy)

	content := []byte("full chunk body content")
	backend, err := eng.Put(context.Background(), "bucket", "obj", bytes.NewReader(content))

	require.NoError(t, err, "failover to the healthy backend must succeed")
	assert.Equal(t, "lyve", backend)
	require.Equal(t, 1, healthy.calls)
	assert.Equal(t, content, healthy.got,
		"the retried backend must receive the complete body, not the drained remainder")
}

// TestEnginePut_NonSeekableConsumedBodyFailsInsteadOfTruncating: once a
// streamed (non-rewindable) body has been partially consumed, retrying on a
// backend that does not validate Content-Length would silently store a
// TRUNCATED object and report success. The engine must fail the request
// instead.
func TestEnginePut_NonSeekableConsumedBodyFailsInsteadOfTruncating(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "idrive"})
	flaky := &partialConsumeDriver{name: "idrive", consume: 5}
	lax := &recordingDriver{name: "quotaless"}
	eng.AddDriver("idrive", flaky)
	eng.AddDriver("quotaless", lax)

	content := []byte("streamed body that cannot be replayed")
	_, err := eng.Put(context.Background(), "bucket", "obj",
		&nonSeekable{Reader: bytes.NewReader(content)})

	require.Error(t, err,
		"a consumed non-rewindable body must fail the PUT, never store a truncated object as success")
	assert.Zero(t, lax.calls,
		"no further backend may be attempted with a drained body")
}

// TestEnginePut_NonSeekableBodyDoesNotChargeHealthyBreakers: with a drained
// body, every doomed retry fails on a length check and charges a HEALTHY
// backend's circuit breaker. Five large failed PUTs inside the breaker window
// then open every breaker — one flaky client turns into a total write outage.
func TestEnginePut_NonSeekableBodyDoesNotChargeHealthyBreakers(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "idrive"})
	flaky := &partialConsumeDriver{name: "idrive", consume: 5}
	strict := &lengthValidatingDriver{name: "lyve"}
	eng.AddDriver("idrive", flaky)
	eng.AddDriver("lyve", strict)

	content := []byte("body long enough to be partially consumed")
	// Breaker threshold is 5 failures / 60s: drive the flaky primary past it.
	for i := 0; i < 5; i++ {
		_, err := eng.Put(context.Background(), "bucket", fmt.Sprintf("obj-%d", i),
			&nonSeekable{Reader: bytes.NewReader(content)},
			WithContentLength(int64(len(content))))
		require.Error(t, err)
	}

	statuses := eng.GetFailoverStatus()
	assert.NotEqual(t, "closed", statuses["idrive"],
		"the genuinely failing backend must trip its breaker")
	assert.Equal(t, "closed", statuses["lyve"],
		"a healthy backend must not be charged for failures caused by a drained body")
	assert.Zero(t, strict.calls,
		"the healthy backend must not receive doomed drained-body attempts at all")
}

// TestEnginePut_NonSeekableUnconsumedBodyStillFailsOver: a failure BEFORE any
// body byte is read (connection refused, breaker probe) leaves the stream
// intact — failover must still work for non-seekable bodies in that case.
func TestEnginePut_NonSeekableUnconsumedBodyStillFailsOver(t *testing.T) {
	eng := NewEngine(nil, zap.NewNop(), &Config{DefaultBackend: "idrive"})
	refusing := &mockDriver{name: "idrive", putErr: fmt.Errorf("dial tcp: connection refused")}
	healthy := &recordingDriver{name: "lyve"}
	eng.AddDriver("idrive", refusing)
	eng.AddDriver("lyve", healthy)

	content := "untouched streamed body"
	backend, err := eng.Put(context.Background(), "bucket", "obj",
		&nonSeekable{Reader: strings.NewReader(content)})

	require.NoError(t, err)
	assert.Equal(t, "lyve", backend)
	assert.Equal(t, []byte(content), healthy.got)
}
