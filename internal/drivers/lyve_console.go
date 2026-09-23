package drivers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// LyveConsoleClient calls Lyve Cloud's account-management ("RS*") actions.
//
// The Lyve console has no separate backend: it POSTs form-encoded actions
// (Action=RS…, Version=2010-05-08) to the IAM host, signed with plain SigV4
// as service "iam", using an ordinary S3 access key. RSCustomerDetails and
// the billing actions answer only for the account ROOT key — a scoped IAM
// user gets "Action not supported". See .private/lyve-console-rs-actions.md.
//
// Its production job is the authenticated liveness probe: a TCP dial says the
// host is up, this says our credentials still work and the account is not
// suspended — the failure class a dead key produces and a dial never sees.
type LyveConsoleClient struct {
	endpoint   string
	region     string
	creds      aws.Credentials
	httpClient *http.Client
	signer     *v4.Signer
	retryDelay time.Duration
}

// LyveConsoleOption configures a LyveConsoleClient.
type LyveConsoleOption func(*LyveConsoleClient)

// ErrLyveConsoleForbidden is returned when a console action is refused with
// 403 on both attempts. Root intermittently gets a single 403 from
// RSCustomerDetails (observed 2026-09-20: two ~5-minute windows after IAM
// mutations), so one 403 is retried before it counts.
var ErrLyveConsoleForbidden = errors.New("lyve console: forbidden")

const (
	// DefaultLyveConsoleEndpoint is the global IAM host every console action
	// is POSTed to, regardless of bucket region.
	DefaultLyveConsoleEndpoint = "https://iam.global.lyve.seagate.com/"
	// DefaultLyveConsoleRetryDelay is the pause before the single 403 retry.
	DefaultLyveConsoleRetryDelay = 10 * time.Second
	lyveConsoleAPIVersion        = "2010-05-08"
)

// WithLyveConsoleEndpoint overrides the IAM host (tests).
func WithLyveConsoleEndpoint(endpoint string) LyveConsoleOption {
	return func(c *LyveConsoleClient) { c.endpoint = endpoint }
}

// WithLyveConsoleHTTPClient overrides the HTTP client (tests).
func WithLyveConsoleHTTPClient(hc *http.Client) LyveConsoleOption {
	return func(c *LyveConsoleClient) { c.httpClient = hc }
}

// WithLyveConsoleRetryDelay sets the pause before the single 403 retry.
func WithLyveConsoleRetryDelay(d time.Duration) LyveConsoleOption {
	return func(c *LyveConsoleClient) { c.retryDelay = d }
}

// NewLyveConsoleClient builds a client for the given key pair. For
// RSCustomerDetails the key must be the account root key.
func NewLyveConsoleClient(accessKey, secretKey string, opts ...LyveConsoleOption) *LyveConsoleClient {
	c := &LyveConsoleClient{
		endpoint:   DefaultLyveConsoleEndpoint,
		region:     "us-east-1", // any region is accepted; the signature just has to be well-formed
		creds:      aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey},
		httpClient: TunedHTTPClient(WithHTTP1Only()),
		signer:     v4.NewSigner(),
		retryDelay: DefaultLyveConsoleRetryDelay,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// CustomerDetails calls RSCustomerDetails for the given customer id (ours is
// "v01") and returns nil when the account answers 200. A single 403 is
// retried once after retryDelay; any other non-2xx fails immediately.
func (c *LyveConsoleClient) CustomerDetails(ctx context.Context, customerName string) error {
	form := url.Values{}
	form.Set("Action", "RSCustomerDetails")
	form.Set("CustomerName", customerName)
	form.Set("Version", lyveConsoleAPIVersion)
	return c.do(ctx, "RSCustomerDetails", form)
}

func (c *LyveConsoleClient) do(ctx context.Context, action string, form url.Values) error {
	status, err := c.post(ctx, form)
	if err != nil {
		return fmt.Errorf("lyve console %s: %w", action, err)
	}
	if status == http.StatusForbidden {
		// One propagation-window retry, then it is a real failure.
		if c.retryDelay > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("lyve console %s: retry after 403: %w", action, ctx.Err())
			case <-time.After(c.retryDelay):
			}
		}
		status, err = c.post(ctx, form)
		if err != nil {
			return fmt.Errorf("lyve console %s (retry): %w", action, err)
		}
		if status == http.StatusForbidden {
			return fmt.Errorf("lyve console %s: 403 on 2 attempts: %w", action, ErrLyveConsoleForbidden)
		}
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("lyve console %s: unexpected status %d", action, status)
	}
	return nil
}

// post signs and sends one form POST, returning the HTTP status.
func (c *LyveConsoleClient) post(ctx context.Context, form url.Values) (int, error) {
	payload := form.Encode()
	sum := sha256.Sum256([]byte(payload))
	payloadHash := hex.EncodeToString(sum[:])

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := c.signer.SignHTTP(ctx, c.creds, req, payloadHash, "iam", c.region, time.Now()); err != nil {
		return 0, fmt.Errorf("sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}
