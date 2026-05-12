package drivers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/FairForge/vaultaire/internal/engine"
	"go.uber.org/zap"
)

type listResult struct {
	XMLName  xml.Name `xml:"ListBucketResult"`
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

// QuotalessDriver uses raw HTTP + SigV4 signing with UNSIGNED-PAYLOAD.
// The AWS SDK v2 S3 client is incompatible with Quotaless's Minio gateway
// (flexible checksums corrupt downloads, streaming payload signing resets connections).
type QuotalessDriver struct {
	httpClient *http.Client
	signer     *v4.Signer
	creds      aws.CredentialsProvider
	endpoint   string
	bucket     string
	rootPath   string
	region     string
	maxRetries int
	logger     *zap.Logger
}

func NewQuotalessDriver(accessKey, secretKey, endpoint string, logger *zap.Logger) (*QuotalessDriver, error) {
	if endpoint == "" {
		endpoint = "https://srv1.quotaless.cloud:8000"
	}

	isStaticEndpoint := strings.Contains(endpoint, "srv") ||
		strings.Contains(endpoint, "nl.") ||
		strings.Contains(endpoint, "us.") ||
		strings.Contains(endpoint, "sg.")

	transport := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      256 * 1024,
		WriteBufferSize:     256 * 1024,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(128),
		},
	}

	if isStaticEndpoint {
		logger.Info("Quotaless using static endpoint",
			zap.String("endpoint", endpoint))
	} else {
		logger.Info("Quotaless using dynamic endpoint",
			zap.String("endpoint", endpoint))
	}

	return &QuotalessDriver{
		httpClient: &http.Client{Transport: transport},
		signer:     v4.NewSigner(),
		creds:      credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		endpoint:   strings.TrimRight(endpoint, "/"),
		bucket:     "data",
		rootPath:   "personal-files",
		region:     "us-east-1",
		maxRetries: 3,
		logger:     logger,
	}, nil
}

func (d *QuotalessDriver) signAndDo(ctx context.Context, req *http.Request) (*http.Response, error) {
	creds, err := d.creds.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("retrieve credentials: %w", err)
	}
	req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	if err := d.signer.SignHTTP(ctx, creds, req, "UNSIGNED-PAYLOAD", "s3", d.region, time.Now()); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}
	return d.httpClient.Do(req)
}

func (d *QuotalessDriver) objectURL(container, artifact string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", d.endpoint, d.bucket, d.rootPath, container, artifact)
}

func (d *QuotalessDriver) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	var (
		buf    []byte
		seeker io.ReadSeeker
		size   int64
	)

	if rs, ok := data.(io.ReadSeeker); ok {
		seeker = rs
		cur, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return fmt.Errorf("seek current: %w", err)
		}
		end, err := rs.Seek(0, io.SeekEnd)
		if err != nil {
			return fmt.Errorf("seek end: %w", err)
		}
		size = end - cur
		if _, err := rs.Seek(cur, io.SeekStart); err != nil {
			return fmt.Errorf("seek start: %w", err)
		}
	} else {
		var err error
		buf, err = io.ReadAll(data)
		if err != nil {
			return fmt.Errorf("buffer data: %w", err)
		}
		size = int64(len(buf))
	}

	objURL := d.objectURL(container, artifact)
	var lastErr error
	for attempt := 0; attempt < d.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			d.logger.Warn("retrying upload",
				zap.Int("attempt", attempt+1),
				zap.String("path", container+"/"+artifact),
				zap.Duration("backoff", backoff),
				zap.Error(lastErr))
			time.Sleep(backoff)
		}

		var reqBody io.Reader
		if buf != nil {
			reqBody = bytes.NewReader(buf)
		} else {
			if _, err := seeker.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("reset reader: %w", err)
			}
			reqBody = seeker
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPut, objURL, reqBody)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.ContentLength = size

		resp, err := d.signAndDo(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("PUT %d", resp.StatusCode)
			continue
		}

		d.logger.Debug("upload successful",
			zap.String("container", container),
			zap.String("artifact", artifact),
			zap.Int("attempts", attempt+1))
		return nil
	}

	return fmt.Errorf("upload failed after %d attempts: %w", d.maxRetries, lastErr)
}

func (d *QuotalessDriver) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	objURL := d.objectURL(container, artifact)

	var lastErr error
	for attempt := 0; attempt < d.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			d.logger.Warn("retrying get",
				zap.Int("attempt", attempt+1),
				zap.String("path", container+"/"+artifact),
				zap.Duration("backoff", backoff),
				zap.Error(lastErr))
			time.Sleep(backoff)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, objURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		resp, err := d.signAndDo(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("object not found: %s/%s", container, artifact)
		}

		if resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("GET %d", resp.StatusCode)
			continue
		}

		return resp.Body, nil
	}

	return nil, fmt.Errorf("get failed after %d attempts: %w", d.maxRetries, lastErr)
}

func (d *QuotalessDriver) Delete(ctx context.Context, container, artifact string) error {
	objURL := d.objectURL(container, artifact)

	var lastErr error
	for attempt := 0; attempt < d.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			d.logger.Warn("retrying delete",
				zap.Int("attempt", attempt+1),
				zap.String("path", container+"/"+artifact),
				zap.Duration("backoff", backoff),
				zap.Error(lastErr))
			time.Sleep(backoff)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, objURL, nil)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}

		resp, err := d.signAndDo(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
			lastErr = fmt.Errorf("DELETE %d", resp.StatusCode)
			continue
		}

		return nil
	}

	return fmt.Errorf("delete failed after %d attempts: %w", d.maxRetries, lastErr)
}

func (d *QuotalessDriver) List(ctx context.Context, container string, prefix string) ([]string, error) {
	fullPrefix := fmt.Sprintf("%s/%s/", d.rootPath, container)
	if prefix != "" {
		fullPrefix += prefix
	}

	u, err := url.Parse(fmt.Sprintf("%s/%s", d.endpoint, d.bucket))
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	q := u.Query()
	q.Set("list-type", "2")
	q.Set("prefix", fullPrefix)
	q.Set("max-keys", "1000")
	u.RawQuery = q.Encode()

	var lastErr error
	for attempt := 0; attempt < d.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			d.logger.Warn("retrying list",
				zap.Int("attempt", attempt+1),
				zap.String("prefix", fullPrefix),
				zap.Duration("backoff", backoff),
				zap.Error(lastErr))
			time.Sleep(backoff)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		resp, err := d.signAndDo(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("LIST %d", resp.StatusCode)
			continue
		}

		var result listResult
		if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("parse list response: %w", err)
			continue
		}
		_ = resp.Body.Close()

		cleaned := make([]string, 0, len(result.Contents))
		for _, entry := range result.Contents {
			rel := strings.TrimPrefix(entry.Key, fullPrefix)
			if rel != "" {
				cleaned = append(cleaned, rel)
			}
		}

		return cleaned, nil
	}

	return nil, fmt.Errorf("list failed after %d attempts: %w", d.maxRetries, lastErr)
}

func (d *QuotalessDriver) Exists(ctx context.Context, container, artifact string) (bool, error) {
	objURL := d.objectURL(container, artifact)

	var lastErr error
	for attempt := 0; attempt < d.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt*attempt) * time.Second
			d.logger.Warn("retrying exists check",
				zap.Int("attempt", attempt+1),
				zap.String("path", container+"/"+artifact),
				zap.Duration("backoff", backoff),
				zap.Error(lastErr))
			time.Sleep(backoff)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodHead, objURL, nil)
		if err != nil {
			return false, fmt.Errorf("create request: %w", err)
		}

		resp, err := d.signAndDo(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return true, nil
		}
		if resp.StatusCode == http.StatusNotFound {
			return false, nil
		}

		lastErr = fmt.Errorf("HEAD %d", resp.StatusCode)
	}

	return false, fmt.Errorf("exists check failed after %d attempts: %w", d.maxRetries, lastErr)
}

func (d *QuotalessDriver) HealthCheck(ctx context.Context) error {
	u, err := url.Parse(d.endpoint)
	if err != nil {
		return fmt.Errorf("parse endpoint: %w", err)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return fmt.Errorf("quotaless health check: %w", err)
	}
	return conn.Close()
}

func (d *QuotalessDriver) GetMetrics() map[string]interface{} {
	return map[string]interface{}{
		"endpoint":    d.endpoint,
		"bucket":      d.bucket,
		"max_retries": d.maxRetries,
		"root_path":   d.rootPath,
	}
}

func (d *QuotalessDriver) Name() string {
	return "quotaless"
}
