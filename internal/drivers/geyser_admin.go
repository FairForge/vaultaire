// internal/drivers/geyser_admin.go
//
// GeyserAdmin provides programmatic access to Geyser's console API.
// Reverse-engineered from browser traffic — monitor for breakage after
// Geyser console updates.
//
// Authentication model: Geyser requires email MFA on login, making fully
// automated auth impossible. Instead, the operator logs in manually once,
// copies the session cookies into Vaultaire config, and this client keeps
// the session alive indefinitely via /api/keepalive.
//
// Required cookies (from browser Application tab after login):
//
//	accessToken — session token UUID
//	userId      — user UUID
//
// API endpoints:
//
//	GET  /api/keepalive               — extends session, call every 30s
//	GET  /api/buckets/{id}            — get bucket status
//	POST /api/buckets/{id}/airgap     — enable airgap on a bucket
package drivers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	geyserConsoleBase    = "https://console.geyserdata.com/api"
	geyserKeepaliveEvery = 30 * time.Second
)

// GeyserAdminClient manages airgap and bucket state via Geyser's console API.
// It is safe for concurrent use.
type GeyserAdminClient struct {
	mu            sync.Mutex
	httpClient    *http.Client
	logger        *zap.Logger
	accessToken   string
	userID        string
	stopKeepalive chan struct{}
}

// GeyserBucketStatus is the operational state of a Geyser bucket.
type GeyserBucketStatus struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BucketName  string `json:"bucketName"`
	Status      string `json:"status"` // "ACTIVE" | "AIRGAPPED"
	Endpoint    string `json:"endpoint"`
	LogicalSize int64  `json:"logicalSize"`
	Size        int    `json:"size"`
}

// geyserEnvelope is the standard Geyser API response wrapper.
type geyserEnvelope struct {
	Body    json.RawMessage `json:"body"`
	Status  string          `json:"status"`
	Headers struct {
		AuthID string `json:"authId"`
	} `json:"headers"`
}

// NewGeyserAdminClient creates a client using existing session cookies.
// Obtain these from the browser Application tab after logging in manually:
//  1. Go to https://console.geyserdata.com and log in
//  2. Open DevTools → Application → Cookies → console.geyserdata.com
//  3. Copy the values of accessToken and userId
//  4. Store them in your config (environment variables or secrets manager)
//
// Call StartKeepalive() after creation to prevent session expiry.
func NewGeyserAdminClient(accessToken, userID string, logger *zap.Logger) *GeyserAdminClient {
	return &GeyserAdminClient{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger:        logger,
		accessToken:   accessToken,
		userID:        userID,
		stopKeepalive: make(chan struct{}),
	}
}

// StartKeepalive pings /api/keepalive every 30 seconds to prevent session
// expiry. Call this once after creating the client. Stop it with StopKeepalive.
func (c *GeyserAdminClient) StartKeepalive(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(geyserKeepaliveEvery)
		defer ticker.Stop()

		// Ping immediately on start to validate the token.
		if err := c.keepalive(ctx); err != nil {
			c.logger.Warn("geyser keepalive failed — token may be expired",
				zap.Error(err))
		} else {
			c.logger.Info("geyser session active")
		}

		for {
			select {
			case <-ticker.C:
				if err := c.keepalive(ctx); err != nil {
					c.logger.Warn("geyser keepalive failed",
						zap.Error(err))
				}
			case <-c.stopKeepalive:
				c.logger.Info("geyser keepalive stopped")
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// StopKeepalive stops the background keepalive goroutine.
func (c *GeyserAdminClient) StopKeepalive() {
	close(c.stopKeepalive)
}

// UpdateToken replaces the session token — call this when the operator
// provides a fresh token after a server restart.
func (c *GeyserAdminClient) UpdateToken(accessToken, userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accessToken = accessToken
	c.userID = userID
	c.logger.Info("geyser session token updated",
		zap.String("token", accessToken[:8]+"..."))
}

// AirgapBucket enables airgap protection on a bucket.
// Once airgapped, the bucket is write-protected and cannot be deleted
// without the airgap password. Store the password securely — losing it
// means permanent loss of access.
func (c *GeyserAdminClient) AirgapBucket(ctx context.Context, bucketID, airgapPassword string) error {
	var env geyserEnvelope
	if err := c.post(ctx,
		fmt.Sprintf("/buckets/%s/airgap", bucketID),
		map[string]string{"password": airgapPassword},
		&env,
	); err != nil {
		return fmt.Errorf("airgap bucket %s: %w", bucketID, err)
	}

	if env.Status != "OK" {
		return fmt.Errorf("airgap failed for bucket %s: status=%s", bucketID, env.Status)
	}

	c.logger.Info("bucket airgapped successfully",
		zap.String("bucketID", bucketID))
	return nil
}

// GetBucketStatus returns the current status of a Geyser bucket.
func (c *GeyserAdminClient) GetBucketStatus(ctx context.Context, bucketID string) (*GeyserBucketStatus, error) {
	var env geyserEnvelope
	if err := c.get(ctx, fmt.Sprintf("/buckets/%s", bucketID), &env); err != nil {
		return nil, fmt.Errorf("get bucket %s: %w", bucketID, err)
	}

	var bucket GeyserBucketStatus
	if err := json.Unmarshal(env.Body, &bucket); err != nil {
		return nil, fmt.Errorf("parse bucket response: %w", err)
	}
	return &bucket, nil
}

// IsAirgapped returns true if the bucket is currently in AIRGAPPED state.
func (c *GeyserAdminClient) IsAirgapped(ctx context.Context, bucketID string) (bool, error) {
	status, err := c.GetBucketStatus(ctx, bucketID)
	if err != nil {
		return false, err
	}
	return status.Status == "AIRGAPPED", nil
}

// keepalive pings the keepalive endpoint to extend the session.
func (c *GeyserAdminClient) keepalive(ctx context.Context) error {
	var env geyserEnvelope
	if err := c.get(ctx, "/keepalive", &env); err != nil {
		return err
	}
	if env.Status != "OK" {
		return fmt.Errorf("keepalive returned status=%s", env.Status)
	}
	return nil
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func (c *GeyserAdminClient) post(ctx context.Context, path string, body interface{}, out interface{}) error {
	return c.doRequest(ctx, http.MethodPost, path, body, out)
}

func (c *GeyserAdminClient) get(ctx context.Context, path string, out interface{}) error {
	return c.doRequest(ctx, http.MethodGet, path, nil, out)
}

func (c *GeyserAdminClient) doRequest(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method,
		geyserConsoleBase+path, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", "https://console.geyserdata.com")
	req.Header.Set("Referer", "https://console.geyserdata.com/")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Inject session cookies — both are required.
	c.mu.Lock()
	accessToken := c.accessToken
	userID := c.userID
	c.mu.Unlock()

	req.AddCookie(&http.Cookie{Name: "accessToken", Value: accessToken})
	req.AddCookie(&http.Cookie{Name: "userId", Value: userID})

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status %d from %s %s: %s",
			resp.StatusCode, method, path, string(raw))
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response from %s %s: %w", method, path, err)
		}
	}
	return nil
}
