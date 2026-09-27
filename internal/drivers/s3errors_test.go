package drivers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Review R7-05: every S3-class Exists classified a miss with
// strings.Contains(err.Error(), "404"). The shapes below are what real vendors
// send; the classification must come from the SDK's types, not the text.

// s3Shape describes one canned response from a fake S3 endpoint.
type s3Shape struct {
	status    int
	body      string // XML error body, "" = empty body (how HEAD misses arrive)
	requestID string // x-amz-request-id; used to smuggle "404" into a non-404 error
	reset     bool   // close the TCP connection without answering
}

func (s s3Shape) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.reset {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.(*net.TCPConn).SetLinger(0)
					_ = conn.Close()
					return
				}
			}
			panic("cannot hijack")
		}
		if s.requestID != "" {
			w.Header().Set("x-amz-request-id", s.requestID)
		}
		if s.body != "" {
			w.Header().Set("Content-Type", "application/xml")
		}
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
	})
}

func s3ErrXML(code, msg string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><RequestId>req-1</RequestId></Error>`, code, msg)
}

// newShapeClient returns a real *s3.Client pointed at a server that always
// answers with the given shape. Retries are off so a 5xx costs one round trip.
func newShapeClient(t *testing.T, shape s3Shape) *s3.Client {
	t.Helper()
	srv := httptest.NewServer(shape.handler())
	t.Cleanup(srv.Close)
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		UsePathStyle: true,
		Retryer:      aws.NopRetryer{},
	})
}

var s3ShapeCases = []struct {
	name         string
	shape        s3Shape
	getNotFound  bool // verdict on the GetObject error (body visible)
	headNotFound bool // verdict on the HeadObject error — HEAD carries no body,
	// so a 404 for a missing BUCKET arrives exactly like a missing key (AWS
	// behaves the same). HealthCheck's HeadBucket is where NoSuchBucket shows.
}{
	{"404 empty body (HEAD miss on every vendor)", s3Shape{status: 404}, true, true},
	{"404 NoSuchKey XML (GET miss)", s3Shape{status: 404, body: s3ErrXML("NoSuchKey", "The specified key does not exist.")}, true, true},
	{"404 NoSuchBucket XML (misconfigured backend)", s3Shape{status: 404, body: s3ErrXML("NoSuchBucket", "The specified bucket does not exist")}, false, true},
	{"403 AccessDenied (key cannot see the object)", s3Shape{status: 403, body: s3ErrXML("AccessDenied", "Access Denied")}, false, false},
	{"403 with a request id containing 404 (the substring trap)", s3Shape{status: 403, body: s3ErrXML("AccessDenied", "Access Denied"), requestID: "A404B"}, false, false},
	{"500 InternalError", s3Shape{status: 500, body: s3ErrXML("InternalError", "We encountered an internal error")}, false, false},
	{"503 SlowDown", s3Shape{status: 503, body: s3ErrXML("SlowDown", "Please reduce your request rate.")}, false, false},
	// Edge proxies (Cloudflare, HAProxy) answer with HTML, not S3 XML.
	{"404 HTML page from an edge proxy", s3Shape{status: 404, body: "<html><body>404 Not Found</body></html>"}, true, true},
	{"403 HTML page mentioning 404", s3Shape{status: 403, body: "<html><body>Forbidden: see error 404 docs</body></html>"}, false, false},
	{"connection reset", s3Shape{reset: true}, false, false},
}

func TestS3IsNotFound_ClassifiesRealSDKErrors(t *testing.T) {
	for _, tc := range s3ShapeCases {
		t.Run(tc.name, func(t *testing.T) {
			client := newShapeClient(t, tc.shape)
			_, err := client.HeadObject(context.Background(), &s3.HeadObjectInput{
				Bucket: aws.String("b"), Key: aws.String("k"),
			})
			require.Error(t, err)
			assert.Equal(t, tc.headNotFound, s3IsNotFound(err), "HEAD error was: %v", err)
			_, err = client.GetObject(context.Background(), &s3.GetObjectInput{
				Bucket: aws.String("b"), Key: aws.String("k"),
			})
			require.Error(t, err)
			assert.Equal(t, tc.getNotFound, s3IsNotFound(err), "GET error was: %v", err)
		})
	}
	assert.False(t, s3IsNotFound(nil))
	assert.False(t, s3IsNotFound(fmt.Errorf("plain 404 text is not a typed miss")))
}

// existsFn adapts each driver's Exists so one table covers all six.
type existsDriver struct {
	name string
	mk   func(client *s3.Client) func(ctx context.Context) (bool, error)
}

func r7ExistsDrivers() []existsDriver {
	log := zap.NewNop()
	return []existsDriver{
		{"s3compat", func(c *s3.Client) func(context.Context) (bool, error) {
			d := &S3CompatDriver{client: c, bucket: "data", prefix: "personal-files/vaultaire", logger: log}
			return func(ctx context.Context) (bool, error) { return d.Exists(ctx, "c", "a") }
		}},
		{"s3", func(c *s3.Client) func(context.Context) (bool, error) {
			d := &S3Driver{client: c, logger: log}
			return func(ctx context.Context) (bool, error) { return d.Exists(ctx, "c", "a") }
		}},
		{"lyve", func(c *s3.Client) func(context.Context) (bool, error) {
			d := &LyveDriver{client: c, tenantID: "t", region: "us-west-1", logger: log}
			return func(ctx context.Context) (bool, error) { return d.Exists(ctx, "c", "a") }
		}},
		{"idrive", func(c *s3.Client) func(context.Context) (bool, error) {
			d := &IDriveDriver{client: c, bucket: "vaultaire", logger: log}
			return func(ctx context.Context) (bool, error) { return d.Exists(ctx, "c", "a") }
		}},
		{"geyser", func(c *s3.Client) func(context.Context) (bool, error) {
			d := &GeyserDriver{client: c, bucket: "b", tenantID: "t", logger: log}
			return func(ctx context.Context) (bool, error) { return d.Exists(ctx, "c", "a") }
		}},
		{"r2", func(c *s3.Client) func(context.Context) (bool, error) {
			d := &R2Driver{client: c, bucket: R2DefaultBucket, logger: log}
			return func(ctx context.Context) (bool, error) { return d.Exists(ctx, "c", "a") }
		}},
	}
}

// Exists contract (R6): (false, nil) for an absent object; an error for
// anything the backend could not answer — including a 403 whose request id
// happens to contain "404", which the old substring check reported as absent.
func TestExists_MissVsErrorOnEveryS3Driver(t *testing.T) {
	for _, drv := range r7ExistsDrivers() {
		for _, tc := range s3ShapeCases {
			t.Run(drv.name+"/"+tc.name, func(t *testing.T) {
				exists := drv.mk(newShapeClient(t, tc.shape))
				ok, err := exists(context.Background())
				assert.False(t, ok)
				if tc.headNotFound {
					assert.NoError(t, err, "a miss is (false, nil)")
				} else {
					assert.Error(t, err, "not a miss: the error must surface")
				}
			})
		}
		t.Run(drv.name+"/200 present", func(t *testing.T) {
			exists := drv.mk(newShapeClient(t, s3Shape{status: 200}))
			ok, err := exists(context.Background())
			require.NoError(t, err)
			assert.True(t, ok)
		})
	}
}
