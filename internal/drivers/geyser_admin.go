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
// Required env vars:
//
//	GEYSER_ACCESS_TOKEN       — session token from browser cookie
//	GEYSER_USER_ID            — user UUID from browser cookie
//	GEYSER_DATACENTER_ID      — datacenter UUID (from your Geyser account)
//	GEYSER_CUSTOMER_ID        — customer UUID (from your Geyser account)
//	GEYSER_TAPE_COLLECTION_ID — tape collection UUID (from your Geyser account)
//
// API endpoints:
//
//	GET    /api/keepalive           — extends session, call every 30s
//	GET    /api/buckets/{id}        — get bucket status
//	POST   /api/buckets/{id}/airgap — enable airgap on a bucket
//	POST   /api/buckets             — provision a new bucket
//	DELETE /api/buckets/{id}        — delete a bucket
//	GET    /api/invoices            — billing records
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

// GeyserProvisioningConfig holds the account-specific UUIDs required to
// provision new buckets. Load these from environment variables or your
// secrets manager — never hardcode in source.
//
//	cfg := drivers.GeyserProvisioningConfig{
//	    DatacenterID:     os.Getenv("GEYSER_DATACENTER_ID"),
//	    CustomerID:       os.Getenv("GEYSER_CUSTOMER_ID"),
//	    TapeCollectionID: os.Getenv("GEYSER_TAPE_COLLECTION_ID"),
//	}
type GeyserProvisioningConfig struct {
	DatacenterID     string // GEYSER_DATACENTER_ID
	CustomerID       string // GEYSER_CUSTOMER_ID
	TapeCollectionID string // GEYSER_TAPE_COLLECTION_ID
}

// GeyserAdminClient manages airgap and bucket state via Geyser's console API.
// It is safe for concurrent use.
type GeyserAdminClient struct {
	mu            sync.Mutex
	httpClient    *http.Client
	logger        *zap.Logger
	accessToken   string
	userID        string
	provConfig    GeyserProvisioningConfig
	stopKeepalive chan struct{}
}

// GeyserBucketStatus is the operational state of a Geyser bucket.
type GeyserBucketStatus struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	BucketName  string `json:"bucketName"`
	Status      string `json:"status"` // "ACTIVE" | "AIRGAPPED" | "PROVISIONING"
	Endpoint    string `json:"endpoint"`
	LogicalSize int64  `json:"logicalSize"`
	Size        int    `json:"size"`
}

// GeyserTapeCollectionInvoice is the per-collection line item within an invoice.
// It shows actual TB stored, rate, and any optional add-on costs.
type GeyserTapeCollectionInvoice struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	TapeCollectionID string  `json:"tapeCollectionId"`
	DatacenterID     string  `json:"datacenterId"`
	Geo              string  `json:"geo"`
	TBCount          float64 `json:"tbCount"`         // actual TB stored this month
	TBRate           float64 `json:"tbRate"`          // $/TB — currently $1.55
	TBCost           float64 `json:"tbCost"`          // tbCount * tbRate
	Compression      bool    `json:"compression"`     // whether compression was enabled
	CompressionRate  float64 `json:"compressionRate"` // $/TB surcharge if enabled
	CompressionCost  float64 `json:"compressionCost"` // total compression surcharge
	Encryption       bool    `json:"encryption"`
	EncryptionRate   float64 `json:"encryptionRate"`
	EncryptionCost   float64 `json:"encryptionCost"`
	Cost             float64 `json:"cost"` // total for this collection line
}

// GeyserMiscBilling is a catch-all line item, used for the 100TB minimum
// commitment shortfall charge. If you store less than 100TB, Geyser bills
// the difference at the standard rate.
//
// Example: 18TB used → Amount=82, Rate=1.55, Total=127.10 (shortfall charge)
type GeyserMiscBilling struct {
	Feature string  `json:"feature"` // "TAPE"
	Label   string  `json:"label"`   // "Minimum TBs Count Balance"
	Amount  float64 `json:"amount"`  // TB shortfall against 100TB minimum
	Rate    float64 `json:"rate"`    // $1.55/TB
	Total   float64 `json:"total"`   // amount * rate
}

// GeyserInvoice represents a single billing record from GET /api/invoices.
//
// Key fields:
//   - Month is 0-indexed (0=January, 11=December)
//   - IsInvoice=false means it is a pending estimate, not yet finalised
//   - Subtotal = tape collection charges only
//   - Total = subtotal + misc (minimum commitment shortfall)
//
// The $155/month minimum reflects the 100TB commitment at $1.55/TB.
// MiscBilling carries the shortfall charge when actual usage is below 100TB.
type GeyserInvoice struct {
	ID                     string                        `json:"id"`
	CreatedAt              string                        `json:"createdAt"`
	Month                  int                           `json:"month"` // 0-indexed
	Year                   int                           `json:"year"`
	IsInvoice              bool                          `json:"isInvoice"` // false = estimate
	Subtotal               float64                       `json:"subtotal"`
	Total                  float64                       `json:"total"`
	Discount               float64                       `json:"discount"`
	TapeCollectionInvoices []GeyserTapeCollectionInvoice `json:"tapeCollectionInvoices"`
	MiscBilling            []GeyserMiscBilling           `json:"miscBilling"`
}

// geyserEnvelope is the standard Geyser API response wrapper.
type geyserEnvelope struct {
	Body    json.RawMessage `json:"body"`
	Status  string          `json:"status"`
	Headers struct {
		AuthID string `json:"authId"`
	} `json:"headers"`
}

// createBucketRequest is the payload for POST /api/buckets.
type createBucketRequest struct {
	Name             string `json:"name"`
	TapeCollectionID string `json:"tapeCollectionId"`
	Size             int    `json:"size"`
	DatacenterID     string `json:"datacenterId"`
	CustomerID       string `json:"customerId"`
}

// createBucketResponse is what Geyser returns immediately after POST /api/buckets.
// Status will be "PROVISIONING" — poll GetBucketStatus until "ACTIVE".
type createBucketResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// NewGeyserAdminClient creates a client using existing session cookies and
// account-specific provisioning config.
//
// Obtain accessToken and userID from the browser Application tab after login:
//  1. Go to https://console.geyserdata.com and log in
//  2. Open DevTools → Application → Cookies → console.geyserdata.com
//  3. Copy the values of accessToken and userId
//  4. Store them as GEYSER_ACCESS_TOKEN and GEYSER_USER_ID env vars
//
// Call StartKeepalive() after creation to prevent session expiry.
func NewGeyserAdminClient(accessToken, userID string, cfg GeyserProvisioningConfig, logger *zap.Logger) *GeyserAdminClient {
	return &GeyserAdminClient{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger:        logger,
		accessToken:   accessToken,
		userID:        userID,
		provConfig:    cfg,
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

// ── Bucket operations ─────────────────────────────────────────────────────────

// CreateBucket provisions a new Geyser tape bucket and blocks until it reaches
// ACTIVE status (up to 2 minutes). Returns the fully-populated bucket status.
//
// Why we poll: Geyser tape provisioning is asynchronous. The POST returns
// immediately with status "PROVISIONING"; the bucket becomes usable only
// after the tape system completes allocation (~10–60 seconds in practice).
//
// Name rules: Geyser requires alphanumeric only — no hyphens, underscores,
// or dots. This method strips all non-alphanumeric characters before sending.
// Example: "tenant-abc_123" → "tenantabc123"
func (c *GeyserAdminClient) CreateBucket(ctx context.Context, name string) (*GeyserBucketStatus, error) {
	safe := sanitizeBucketName(name)
	if safe == "" {
		return nil, fmt.Errorf("create bucket: name %q contains no alphanumeric characters", name)
	}

	if c.provConfig.TapeCollectionID == "" || c.provConfig.DatacenterID == "" || c.provConfig.CustomerID == "" {
		return nil, fmt.Errorf("create bucket: GeyserProvisioningConfig is incomplete — check GEYSER_DATACENTER_ID, GEYSER_CUSTOMER_ID, GEYSER_TAPE_COLLECTION_ID env vars")
	}

	payload := createBucketRequest{
		Name:             safe,
		TapeCollectionID: c.provConfig.TapeCollectionID,
		Size:             1,
		DatacenterID:     c.provConfig.DatacenterID,
		CustomerID:       c.provConfig.CustomerID,
	}

	var env geyserEnvelope
	if err := c.post(ctx, "/buckets", payload, &env); err != nil {
		return nil, fmt.Errorf("create bucket %q: %w", safe, err)
	}

	var created createBucketResponse
	if err := json.Unmarshal(env.Body, &created); err != nil {
		return nil, fmt.Errorf("parse create bucket response: %w", err)
	}

	c.logger.Info("bucket provisioning started",
		zap.String("bucketID", created.ID),
		zap.String("name", safe))

	return c.waitForActive(ctx, created.ID, 2*time.Minute, 5*time.Second)
}

// waitForActive polls GetBucketStatus until the bucket reaches ACTIVE status
// or the deadline expires. Private helper for CreateBucket.
func (c *GeyserAdminClient) waitForActive(ctx context.Context, bucketID string, maxWait, interval time.Duration) (*GeyserBucketStatus, error) {
	deadline := time.Now().Add(maxWait)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled while waiting for bucket %s to become active", bucketID)
		case <-ticker.C:
			status, err := c.GetBucketStatus(ctx, bucketID)
			if err != nil {
				// Transient error during provisioning — log and keep polling.
				c.logger.Warn("poll bucket status error",
					zap.String("bucketID", bucketID),
					zap.Error(err))
				continue
			}

			c.logger.Debug("bucket provisioning status",
				zap.String("bucketID", bucketID),
				zap.String("status", status.Status))

			if status.Status == "ACTIVE" {
				c.logger.Info("bucket is active",
					zap.String("bucketID", bucketID),
					zap.String("bucketName", status.BucketName))
				return status, nil
			}

			if time.Now().After(deadline) {
				return nil, fmt.Errorf("bucket %s did not become active within %s (last status: %s)",
					bucketID, maxWait, status.Status)
			}
		}
	}
}

// DeleteBucket permanently deletes a Geyser bucket by its UUID.
// There is no undo. Verify the bucket is not airgapped before calling —
// airgapped buckets cannot be deleted without first removing the airgap.
func (c *GeyserAdminClient) DeleteBucket(ctx context.Context, bucketID string) error {
	if err := c.deleteReq(ctx, fmt.Sprintf("/buckets/%s", bucketID)); err != nil {
		return fmt.Errorf("delete bucket %s: %w", bucketID, err)
	}
	c.logger.Info("bucket deleted", zap.String("bucketID", bucketID))
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

// ── Billing ───────────────────────────────────────────────────────────────────

// GetInvoices returns all billing records for the FairForge account.
// Use this to cross-check Geyser's usage billing against internal quota
// tracking in PostgreSQL.
//
// The response body is a paginated list; this returns all records from the
// first page (Geyser defaults to pageSize=10, which covers our history).
func (c *GeyserAdminClient) GetInvoices(ctx context.Context) ([]GeyserInvoice, error) {
	var env geyserEnvelope
	if err := c.get(ctx, "/invoices", &env); err != nil {
		return nil, fmt.Errorf("get invoices: %w", err)
	}

	var invoices []GeyserInvoice
	if err := json.Unmarshal(env.Body, &invoices); err != nil {
		return nil, fmt.Errorf("parse invoices response: %w", err)
	}
	return invoices, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

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

func (c *GeyserAdminClient) post(ctx context.Context, path string, body interface{}, out interface{}) error {
	return c.doRequest(ctx, http.MethodPost, path, body, out)
}

func (c *GeyserAdminClient) get(ctx context.Context, path string, out interface{}) error {
	return c.doRequest(ctx, http.MethodGet, path, nil, out)
}

func (c *GeyserAdminClient) deleteReq(ctx context.Context, path string) error {
	return c.doRequest(ctx, http.MethodDelete, path, nil, nil)
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

	// Inject session cookies — both are required by Geyser's auth layer.
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
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			// Log but don't override the primary error — body close failures
			// are non-fatal for the caller.
			_ = cerr
		}
	}()

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response from %s %s: %w", method, path, err)
		}
	}
	return nil
}

// sanitizeBucketName strips every character that is not a letter or digit.
// Geyser rejects names with hyphens, underscores, dots, or any other symbol.
//
// Example: "tenant-abc_123" → "tenantabc123"
func sanitizeBucketName(name string) string {
	result := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			result = append(result, ch)
		}
	}
	return string(result)
}
