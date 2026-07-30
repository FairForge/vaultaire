// internal/drivers/geyser_admin.go
//
// GeyserAdmin provides programmatic access to Geyser's console API
// (console.geyserdata.com). Reverse-engineered from the console's JS bundles
// and live-probed 2026-07-29 — monitor for breakage after console updates.
// Ground truth for endpoints and payloads: internal/drivers/geyser_README.md
// ("Console API map").
//
// Authentication — programmatic login (preferred):
//
//  1. Login(ctx, email, password)   POST /api/login → MFA challenge {hash}.
//     Geyser emails a one-time code (totpUser=false) or expects a TOTP code
//     from the account's authenticator app (totpUser=true).
//  2. VerifyMFA(ctx, hash, code)    PUT /api/login → session {id, user:{id}}.
//     The session id and user id become the accessToken/userId cookies every
//     console call sends. Any httpOnly cookies the server sets are captured
//     by the client's cookie jar and forwarded automatically.
//  3. StartKeepalive(ctx)           GET /api/keepalive every 30s keeps the
//     session (~1h idle expiry) alive indefinitely.
//
// NewGeyserAdminClientWithLogin bundles steps 1-2 when the MFA code is already
// in hand (TOTP accounts). With email MFA the code only arrives after step 1,
// so call Login, wait for the email, then VerifyMFA.
//
// Legacy fallback: NewGeyserAdminClient(accessToken, userID, ...) injects a
// session obtained elsewhere (e.g. browser DevTools). Still works, but the
// login flow above removes the manual step entirely.
//
// API endpoints:
//
//	POST   /api/login                              — password login → MFA challenge
//	PUT    /api/login                              — MFA verify → session
//	GET    /api/keepalive                          — extends session, call every 30s
//	GET    /api/buckets                            — list all buckets
//	GET    /api/buckets/{id}                       — get bucket status
//	POST   /api/buckets                            — provision a new bucket
//	DELETE /api/buckets/{id}                       — delete a bucket
//	POST   /api/buckets/{id}/airgap                — enable airgap (one-way, MFA not required)
//	POST   /api/buckets/{id}/mount                 — initiate un-airgap (triggers email MFA)
//	POST   /api/buckets/{id}/confirmmount          — confirm un-airgap with emailed code
//	POST   /api/buckets/{id}/restoreToCache        — thaw object from tape to staging disk
//	POST   /api/buckets/{id}/restore               — push recalled object to a cloud integration
//	GET    /api/buckets/{id}/cloudIntegrations     — list cloud integrations
//	POST   /api/buckets/{id}/cloudIntegrations     — create cloud integration
//	DELETE /api/buckets/{id}/cloudIntegrations/{i} — delete cloud integration
//	POST   /api/cloudSync                          — server-side ingest from another cloud
//	GET    /api/cloudSync?query=bucketId=={id}     — cloud sync job status (RSQL query)
//	GET    /api/invoices                           — billing records
//	GET    /api/keys                               — list S3 key IDs (secrets not returned)
//	GET    /api/tapeCollections                    — tape collections with per-tape detail
//	GET    /api/sites                              — datacenters (LA, London, São Paulo)
//	GET    /api/events                             — audit log
//
// Response framing is inconsistent: some endpoints wrap payloads in a
// {body, status, headers} envelope (/api/buckets, /api/tapeCollections),
// others return bare JSON (/api/sites, /api/keys). geyserBody handles both.
package drivers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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

// GeyserAdminClient manages airgap, bucket, restore, and integration state via
// Geyser's console API. It is safe for concurrent use.
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
	ID            string `json:"id"`
	Name          string `json:"name"`
	BucketName    string `json:"bucketName"`
	Status        string `json:"status"` // "ACTIVE" | "AIRGAPPED" | "PROVISIONING"
	Endpoint      string `json:"endpoint"`
	LogicalSize   int64  `json:"logicalSize"`
	Size          int    `json:"size"`
	Versioning    string `json:"versioning"`
	ObjectLocking bool   `json:"objectLocking"`
	CORSEnabled   bool   `json:"corsEnabled"`
	S3URL         string `json:"s3Url"`
}

// GeyserTapeCollectionInvoice is the per-collection line item within an invoice.
type GeyserTapeCollectionInvoice struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	TapeCollectionID string  `json:"tapeCollectionId"`
	DatacenterID     string  `json:"datacenterId"`
	Geo              string  `json:"geo"`
	TBCount          float64 `json:"tbCount"`
	TBRate           float64 `json:"tbRate"`
	TBCost           float64 `json:"tbCost"`
	Compression      bool    `json:"compression"`
	CompressionRate  float64 `json:"compressionRate"`
	CompressionCost  float64 `json:"compressionCost"`
	Encryption       bool    `json:"encryption"`
	EncryptionRate   float64 `json:"encryptionRate"`
	EncryptionCost   float64 `json:"encryptionCost"`
	Cost             float64 `json:"cost"`
}

// GeyserMiscBilling is the minimum commitment shortfall charge.
// Geyser bills a minimum TB count at $1.55/TB.
// If you store less than the minimum, Amount = shortfall TB, Total = shortfall cost.
type GeyserMiscBilling struct {
	Feature string  `json:"feature"` // "TAPE"
	Label   string  `json:"label"`   // "Minimum TBs Count Balance"
	Amount  float64 `json:"amount"`  // TB shortfall
	Rate    float64 `json:"rate"`    // $1.55/TB
	Total   float64 `json:"total"`   // amount * rate
}

// GeyserInvoice represents a single billing record from GET /api/invoices.
//
// Month is 0-indexed (0=January, 11=December).
// IsInvoice=false means it is a pending estimate, not yet finalised.
// Total = tape collection charges + minimum commitment shortfall.
type GeyserInvoice struct {
	ID                     string                        `json:"id"`
	CreatedAt              string                        `json:"createdAt"`
	Month                  int                           `json:"month"`
	Year                   int                           `json:"year"`
	IsInvoice              bool                          `json:"isInvoice"`
	Subtotal               float64                       `json:"subtotal"`
	Total                  float64                       `json:"total"`
	Discount               float64                       `json:"discount"`
	TapeCollectionInvoices []GeyserTapeCollectionInvoice `json:"tapeCollectionInvoices"`
	MiscBilling            []GeyserMiscBilling           `json:"miscBilling"`
}

// GeyserKeyInfo is a single S3 keypair entry from GET /api/keys.
// SecretAccessKey is always null after initial creation — Geyser does not
// return secrets after the creation response.
type GeyserKeyInfo struct {
	ID              string  `json:"id"`
	Inactive        bool    `json:"inactive"`
	Initialized     bool    `json:"initialized"`
	SecretAccessKey *string `json:"secretAccessKey"` // always null
	UserARN         *string `json:"userARN"`
}

// MFAChallenge is the response to POST /api/login. The account is not yet
// authenticated at this point — pass Hash together with the MFA code to
// VerifyMFA to obtain a session.
type MFAChallenge struct {
	Hash         string `json:"hash"`
	ID           string `json:"id"`           // challenge id — NOT the session token
	ResponseType string `json:"responseType"` // "MFA"
	TOTPUser     bool   `json:"totpUser"`     // true = code from TOTP app, false = emailed code
}

// geyserSession is the response to a successful PUT /api/login (MFA verify).
type geyserSession struct {
	ID   string `json:"id"` // becomes the accessToken cookie
	User struct {
		ID string `json:"id"` // becomes the userId cookie
	} `json:"user"`
}

// GeyserCloudIntegration is a configured external S3 destination on a bucket
// (target of RestoreToCloud). Further fields will be added as live responses
// reveal them — no integration has been created on our account yet.
type GeyserCloudIntegration struct {
	ID                   string `json:"id"`
	CloudIntegrationType string `json:"cloudIntegrationType"` // AWS | WASABI | ORACLE | GEYSER
	Region               string `json:"region,omitempty"`
	Bucket               string `json:"bucket,omitempty"`
}

// CreateCloudIntegrationRequest is the payload for creating a cloud
// integration. The field set is provisional until the first live create —
// only cloudIntegrationType is confirmed from the JS bundles. Endpoint probes
// the open question of whether the AWS type accepts custom endpoints (which
// would let us target iDrive or Lyve).
type CreateCloudIntegrationRequest struct {
	CloudIntegrationType string `json:"cloudIntegrationType"` // AWS | WASABI | ORACLE | GEYSER
	Region               string `json:"region,omitempty"`
	Bucket               string `json:"bucket,omitempty"`
	AccessKey            string `json:"accessKey,omitempty"`
	SecretKey            string `json:"secretKey,omitempty"`
	Endpoint             string `json:"endpoint,omitempty"`
}

// CloudSyncSource identifies the external bucket Geyser pulls from during a
// cloud sync (server-side ingest — the bytes never transit our infrastructure).
// Endpoint is not in the observed console schema; the cloudsync probe sends it
// to learn whether the ingest leg can target a custom S3 endpoint (Lyve).
type CloudSyncSource struct {
	Type      string `json:"type"` // AWS | WASABI | ORACLE
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	Endpoint  string `json:"endpoint,omitempty"`
}

// CreateCloudSyncRequest is the payload for POST /api/cloudSync.
// Action defaults to "SYNC" when empty.
type CreateCloudSyncRequest struct {
	Source   CloudSyncSource `json:"source"`
	Action   string          `json:"action"`
	BucketID string          `json:"bucketId"`
}

// CloudSyncJob is a single cloud sync job from GET /api/cloudSync.
type CloudSyncJob struct {
	ID        string `json:"id"`
	BucketID  string `json:"bucketId"`
	Status    string `json:"status"` // PENDING | QUEUED | INPROGRESS | COMPLETED
	CreatedAt string `json:"createdAt"`
}

// GeyserTape is a single physical tape within a collection.
//
// Field tags are inferred from console UI values (barcode 140241L9, serial
// HPE-1925523288, LTO-9, 17.5 TB) — verify against a live capture and adjust
// if the wire names differ.
type GeyserTape struct {
	Barcode        string `json:"barcode"`
	Serial         string `json:"serial"`
	Type           string `json:"type"` // e.g. "LTO-9"
	AvailableBytes int64  `json:"available"`
	TotalBytes     int64  `json:"total"`
	WriteProtected bool   `json:"writeProtected"`
}

// GeyserTapeCollection is a tape collection from GET /api/tapeCollections.
type GeyserTapeCollection struct {
	ID    string       `json:"id"`
	Name  string       `json:"name"`
	Tapes []GeyserTape `json:"tapes"`
}

// GeyserSite is a Geyser datacenter from GET /api/sites.
// Live-confirmed sites: Los Angeles US, London UK, São Paulo, Brazil.
type GeyserSite struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GeyserEvent is an audit-log entry from GET /api/events (logins, actions).
type GeyserEvent struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Message   string `json:"message,omitempty"`
	UserID    string `json:"userId,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// geyserEnvelope is the response wrapper some console endpoints use.
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

// NewGeyserAdminClient creates a client from an existing session (accessToken
// and userID obtained via the login flow elsewhere, or legacy manual copy from
// browser DevTools). Prefer NewGeyserAdminClientWithLogin, which performs the
// login programmatically.
//
// Call StartKeepalive() after creation to prevent session expiry.
func NewGeyserAdminClient(accessToken, userID string, cfg GeyserProvisioningConfig, logger *zap.Logger) *GeyserAdminClient {
	jar, err := cookiejar.New(nil)
	if err != nil {
		// cookiejar.New with nil options cannot fail today; degrade to the
		// manual-cookie fallback in doRaw if it ever does.
		jar = nil
	}
	c := &GeyserAdminClient{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Jar:     jar,
		},
		logger:        logger,
		accessToken:   accessToken,
		userID:        userID,
		provConfig:    cfg,
		stopKeepalive: make(chan struct{}),
	}
	if accessToken != "" || userID != "" {
		c.seedSessionCookies(accessToken, userID)
	}
	return c
}

// NewGeyserAdminClientWithLogin creates a client and authenticates it in one
// shot: Login (password) followed by VerifyMFA (code). This works when the MFA
// code is already in hand — i.e. TOTP accounts, where the code is generated
// locally. For email-MFA accounts the code only arrives after Login fires, so
// use NewGeyserAdminClient("", "", ...) and drive Login/VerifyMFA separately.
//
// Call StartKeepalive() on the returned client to prevent session expiry.
func NewGeyserAdminClientWithLogin(ctx context.Context, email, password, mfaCode string, cfg GeyserProvisioningConfig, logger *zap.Logger) (*GeyserAdminClient, error) {
	c := NewGeyserAdminClient("", "", cfg, logger)
	challenge, err := c.Login(ctx, email, password)
	if err != nil {
		return nil, err
	}
	if err := c.VerifyMFA(ctx, challenge.Hash, mfaCode); err != nil {
		return nil, err
	}
	return c, nil
}

// Login starts a console session: POST /api/login with the account password.
// On success Geyser issues an MFA challenge and sends a one-time code to the
// account email (or expects a TOTP code if the account has TOTP enabled —
// check the returned TOTPUser flag). Complete authentication with VerifyMFA.
func (c *GeyserAdminClient) Login(ctx context.Context, email, password string) (*MFAChallenge, error) {
	raw, err := c.doRaw(ctx, http.MethodPost, "/login", map[string]string{
		"emailAddress": email,
		"password":     password,
	})
	if err != nil {
		return nil, fmt.Errorf("geyser login: %w", err)
	}
	body, err := geyserBody(raw)
	if err != nil {
		return nil, fmt.Errorf("geyser login: %w", err)
	}

	var challenge MFAChallenge
	if err := json.Unmarshal(body, &challenge); err != nil {
		return nil, fmt.Errorf("parse login challenge: %w", err)
	}
	if challenge.Hash == "" {
		return nil, fmt.Errorf("geyser login: response contained no challenge hash")
	}

	c.logger.Info("geyser login challenge issued",
		zap.String("responseType", challenge.ResponseType),
		zap.Bool("totpUser", challenge.TOTPUser))
	return &challenge, nil
}

// VerifyMFA completes the login: PUT /api/login with the challenge hash from
// Login and the MFA code. On success the session id and user id are stored and
// sent as cookies on every subsequent request; httpOnly cookies set by the
// server land in the client's cookie jar and are forwarded automatically.
func (c *GeyserAdminClient) VerifyMFA(ctx context.Context, hash, code string) error {
	raw, err := c.doRaw(ctx, http.MethodPut, "/login", map[string]string{
		"hash":  hash,
		"token": code,
	})
	if err != nil {
		return fmt.Errorf("geyser verify MFA: %w", err)
	}
	body, err := geyserBody(raw)
	if err != nil {
		return fmt.Errorf("geyser verify MFA: %w", err)
	}

	var sess geyserSession
	if err := json.Unmarshal(body, &sess); err != nil {
		return fmt.Errorf("parse MFA session response: %w", err)
	}
	if sess.ID == "" {
		return fmt.Errorf("geyser verify MFA: response contained no session id")
	}

	c.mu.Lock()
	c.accessToken = sess.ID
	c.userID = sess.User.ID
	c.mu.Unlock()
	c.seedSessionCookies(sess.ID, sess.User.ID)

	c.logger.Info("geyser console session established",
		zap.String("userID", sess.User.ID))
	return nil
}

// SessionCookies returns the current console session pair (accessToken,
// userId) — what probe tooling needs to issue raw requests against console
// endpoints the typed surface doesn't cover yet.
func (c *GeyserAdminClient) SessionCookies() (accessToken, userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accessToken, c.userID
}

// seedSessionCookies writes the session cookies into the jar so they are sent
// on every request (and overwrite any stale jar entries from a prior session).
func (c *GeyserAdminClient) seedSessionCookies(accessToken, userID string) {
	if c.httpClient.Jar == nil {
		return
	}
	u, err := url.Parse("https://console.geyserdata.com/")
	if err != nil {
		return
	}
	c.httpClient.Jar.SetCookies(u, []*http.Cookie{
		{Name: "accessToken", Value: accessToken, Path: "/"}, // #nosec G124 — outgoing request cookie to Geyser API, not served to users
		{Name: "userId", Value: userID, Path: "/"},           // #nosec G124 — outgoing request cookie to Geyser API, not served to users
	})
}

// StartKeepalive pings /api/keepalive every 30 seconds to prevent session
// expiry. Call this once after creating the client. Stop it with StopKeepalive.
func (c *GeyserAdminClient) StartKeepalive(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(geyserKeepaliveEvery)
		defer ticker.Stop()

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
	c.accessToken = accessToken
	c.userID = userID
	c.mu.Unlock()
	c.seedSessionCookies(accessToken, userID)

	preview := accessToken
	if len(preview) > 8 {
		preview = preview[:8] + "..."
	}
	c.logger.Info("geyser session token updated",
		zap.String("token", preview))
}

// ── Bucket operations ─────────────────────────────────────────────────────────

// CreateBucket provisions a new Geyser tape bucket and blocks until it reaches
// ACTIVE status (up to 2 minutes). Returns the fully-populated bucket status.
//
// Name rules: Geyser requires alphanumeric only — no hyphens, underscores,
// or dots. This method strips all non-alphanumeric characters before sending.
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

	var created createBucketResponse
	if err := c.doJSON(ctx, http.MethodPost, "/buckets", payload, &created); err != nil {
		return nil, fmt.Errorf("create bucket %q: %w", safe, err)
	}

	c.logger.Info("bucket provisioning started",
		zap.String("bucketID", created.ID),
		zap.String("name", safe))

	return c.waitForActive(ctx, created.ID, 2*time.Minute, 5*time.Second)
}

// waitForActive polls GetBucketStatus until ACTIVE or deadline. Private helper.
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
// There is no undo. The bucket must not be airgapped.
func (c *GeyserAdminClient) DeleteBucket(ctx context.Context, bucketID string) error {
	if err := c.doJSON(ctx, http.MethodDelete, fmt.Sprintf("/buckets/%s", bucketID), nil, nil); err != nil {
		return fmt.Errorf("delete bucket %s: %w", bucketID, err)
	}
	c.logger.Info("bucket deleted", zap.String("bucketID", bucketID))
	return nil
}

// GetBucketStatus returns the current status of a Geyser bucket.
//
// bucketID is the console-internal UUID, NOT the S3 bucket name (e.g. S3 name
// stored3lib-632df558-... → console ID 632df558-...).
func (c *GeyserAdminClient) GetBucketStatus(ctx context.Context, bucketID string) (*GeyserBucketStatus, error) {
	var bucket GeyserBucketStatus
	if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/buckets/%s", bucketID), nil, &bucket); err != nil {
		return nil, fmt.Errorf("get bucket %s: %w", bucketID, err)
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
//
// Once airgapped, the bucket is write-protected and CANNOT be unlocked via
// API — removal requires manual action through the Geyser console UI at
// console.geyserdata.com, which triggers an email MFA challenge.
//
// This is intentional: airgap is a one-way compliance commitment providing
// WORM (Write Once Read Many) guarantees. Store the password securely —
// losing it means permanent loss of write access.
func (c *GeyserAdminClient) AirgapBucket(ctx context.Context, bucketID, airgapPassword string) error {
	if err := c.doJSON(ctx,
		http.MethodPost,
		fmt.Sprintf("/buckets/%s/airgap", bucketID),
		map[string]string{"password": airgapPassword},
		nil,
	); err != nil {
		return fmt.Errorf("airgap bucket %s: %w", bucketID, err)
	}

	c.logger.Info("bucket airgapped successfully",
		zap.String("bucketID", bucketID))
	return nil
}

// InitiateMount begins the un-airgap process for a bucket.
// Geyser sends a one-time verification code to the account email.
// Pass that code to ConfirmMount to complete the operation.
//
// Un-airgapping requires human involvement by design — the email MFA step
// cannot be bypassed programmatically. This is a Geyser security requirement.
func (c *GeyserAdminClient) InitiateMount(ctx context.Context, bucketID, airgapPassword string) error {
	if err := c.doJSON(ctx,
		http.MethodPost,
		fmt.Sprintf("/buckets/%s/mount", bucketID),
		map[string]string{"password": airgapPassword},
		nil,
	); err != nil {
		return fmt.Errorf("initiate mount bucket %s: %w", bucketID, err)
	}

	c.logger.Info("mount initiated — check email for verification code",
		zap.String("bucketID", bucketID))
	return nil
}

// ConfirmMount completes the un-airgap process using the code emailed after
// InitiateMount. On success the bucket transitions from AIRGAPPED to ACTIVE.
func (c *GeyserAdminClient) ConfirmMount(ctx context.Context, bucketID, emailCode string) (*GeyserBucketStatus, error) {
	var bucket GeyserBucketStatus
	if err := c.doJSON(ctx,
		http.MethodPost,
		fmt.Sprintf("/buckets/%s/confirmmount", bucketID),
		map[string]string{"code": emailCode},
		&bucket,
	); err != nil {
		return nil, fmt.Errorf("confirm mount bucket %s: %w", bucketID, err)
	}

	c.logger.Info("bucket successfully un-airgapped",
		zap.String("bucketID", bucketID),
		zap.String("status", bucket.Status))
	return &bucket, nil
}

// ── Restore operations ────────────────────────────────────────────────────────

// RestoreToCache thaws an archived object from tape onto Vail's staging disk —
// the console-API equivalent of S3 RestoreObject. versionID may be empty for
// the current version (it is then omitted from the payload).
func (c *GeyserAdminClient) RestoreToCache(ctx context.Context, bucketID, path, versionID string) error {
	payload := map[string]string{"path": path}
	if versionID != "" {
		payload["versionId"] = versionID
	}
	if err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("/buckets/%s/restoreToCache", bucketID), payload, nil); err != nil {
		return fmt.Errorf("restore to cache %s/%s: %w", bucketID, path, err)
	}
	c.logger.Info("restore to cache submitted",
		zap.String("bucketID", bucketID),
		zap.String("path", path))
	return nil
}

// RestoreToCloud pushes a recalled object directly into a configured cloud
// integration (see ListCloudIntegrations) — the bytes go straight from Geyser
// to the destination, bypassing our bandwidth entirely. versionID may be
// empty for the current version.
func (c *GeyserAdminClient) RestoreToCloud(ctx context.Context, bucketID, path, integrationID, versionID string) error {
	payload := map[string]string{
		"path":          path,
		"integrationId": integrationID,
	}
	if versionID != "" {
		payload["versionId"] = versionID
	}
	if err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("/buckets/%s/restore", bucketID), payload, nil); err != nil {
		return fmt.Errorf("restore to cloud %s/%s (integration %s): %w", bucketID, path, integrationID, err)
	}
	c.logger.Info("restore to cloud integration submitted",
		zap.String("bucketID", bucketID),
		zap.String("path", path),
		zap.String("integrationID", integrationID))
	return nil
}

// ── Cloud integrations ────────────────────────────────────────────────────────

// ListCloudIntegrations returns the cloud integrations configured on a bucket.
// Returns an empty slice when none are configured (live-observed bare []).
func (c *GeyserAdminClient) ListCloudIntegrations(ctx context.Context, bucketID string) ([]GeyserCloudIntegration, error) {
	var integrations []GeyserCloudIntegration
	if err := c.doJSON(ctx, http.MethodGet,
		fmt.Sprintf("/buckets/%s/cloudIntegrations", bucketID), nil, &integrations); err != nil {
		return nil, fmt.Errorf("list cloud integrations for bucket %s: %w", bucketID, err)
	}
	return integrations, nil
}

// CreateCloudIntegration configures a new cloud integration on a bucket.
func (c *GeyserAdminClient) CreateCloudIntegration(ctx context.Context, bucketID string, req CreateCloudIntegrationRequest) (*GeyserCloudIntegration, error) {
	var integration GeyserCloudIntegration
	if err := c.doJSON(ctx, http.MethodPost,
		fmt.Sprintf("/buckets/%s/cloudIntegrations", bucketID), req, &integration); err != nil {
		return nil, fmt.Errorf("create cloud integration on bucket %s: %w", bucketID, err)
	}
	c.logger.Info("cloud integration created",
		zap.String("bucketID", bucketID),
		zap.String("type", req.CloudIntegrationType),
		zap.String("integrationID", integration.ID))
	return &integration, nil
}

// DeleteCloudIntegration removes a cloud integration from a bucket.
func (c *GeyserAdminClient) DeleteCloudIntegration(ctx context.Context, bucketID, integrationID string) error {
	if err := c.doJSON(ctx, http.MethodDelete,
		fmt.Sprintf("/buckets/%s/cloudIntegrations/%s", bucketID, integrationID), nil, nil); err != nil {
		return fmt.Errorf("delete cloud integration %s on bucket %s: %w", integrationID, bucketID, err)
	}
	c.logger.Info("cloud integration deleted",
		zap.String("bucketID", bucketID),
		zap.String("integrationID", integrationID))
	return nil
}

// ── cloudSync ─────────────────────────────────────────────────────────────────

// CreateCloudSync starts a server-side ingest job: Geyser pulls directly from
// the source bucket into the target Geyser bucket — no bytes transit our
// infrastructure. Action defaults to "SYNC" when empty.
func (c *GeyserAdminClient) CreateCloudSync(ctx context.Context, req CreateCloudSyncRequest) error {
	if req.Action == "" {
		req.Action = "SYNC"
	}
	if err := c.doJSON(ctx, http.MethodPost, "/cloudSync", req, nil); err != nil {
		return fmt.Errorf("create cloud sync into bucket %s: %w", req.BucketID, err)
	}
	c.logger.Info("cloud sync submitted",
		zap.String("bucketID", req.BucketID),
		zap.String("sourceType", req.Source.Type),
		zap.String("sourceBucket", req.Source.Bucket))
	return nil
}

// GetCloudSyncStatus returns the cloud sync jobs for a bucket via an RSQL
// query. Job Status is one of PENDING | QUEUED | INPROGRESS | COMPLETED.
//
// Note: live-observed to return 500 when the account has no integrations
// configured — treat errors as "possibly none yet", not fatal.
func (c *GeyserAdminClient) GetCloudSyncStatus(ctx context.Context, bucketID string) ([]CloudSyncJob, error) {
	query := url.Values{"query": {"bucketId==" + bucketID}}
	var jobs []CloudSyncJob
	if err := c.doJSON(ctx, http.MethodGet, "/cloudSync?"+query.Encode(), nil, &jobs); err != nil {
		return nil, fmt.Errorf("get cloud sync status for bucket %s: %w", bucketID, err)
	}
	return jobs, nil
}

// ── Billing / account info ────────────────────────────────────────────────────

// GetInvoices returns all billing records for the FairForge account.
// Use this to cross-check Geyser's usage billing against internal quota
// tracking in PostgreSQL.
func (c *GeyserAdminClient) GetInvoices(ctx context.Context) ([]GeyserInvoice, error) {
	var invoices []GeyserInvoice
	if err := c.doJSON(ctx, http.MethodGet, "/invoices", nil, &invoices); err != nil {
		return nil, fmt.Errorf("get invoices: %w", err)
	}
	return invoices, nil
}

// GetKeys returns all S3 key IDs associated with the account.
// Secrets are never returned — they are only visible at creation time.
func (c *GeyserAdminClient) GetKeys(ctx context.Context) ([]GeyserKeyInfo, error) {
	var keys []GeyserKeyInfo
	if err := c.doJSON(ctx, http.MethodGet, "/keys", nil, &keys); err != nil {
		return nil, fmt.Errorf("get keys: %w", err)
	}
	return keys, nil
}

// GetTapeCollections returns the account's tape collections with per-tape
// detail (barcode, serial, capacity, write protection).
func (c *GeyserAdminClient) GetTapeCollections(ctx context.Context) ([]GeyserTapeCollection, error) {
	var collections []GeyserTapeCollection
	if err := c.doJSON(ctx, http.MethodGet, "/tapeCollections", nil, &collections); err != nil {
		return nil, fmt.Errorf("get tape collections: %w", err)
	}
	return collections, nil
}

// GetSites returns Geyser's datacenters (live-confirmed: Los Angeles US,
// London UK, São Paulo Brazil).
func (c *GeyserAdminClient) GetSites(ctx context.Context) ([]GeyserSite, error) {
	var sites []GeyserSite
	if err := c.doJSON(ctx, http.MethodGet, "/sites", nil, &sites); err != nil {
		return nil, fmt.Errorf("get sites: %w", err)
	}
	return sites, nil
}

// GetEvents returns the account audit log (logins, actions, timestamps).
func (c *GeyserAdminClient) GetEvents(ctx context.Context) ([]GeyserEvent, error) {
	var events []GeyserEvent
	if err := c.doJSON(ctx, http.MethodGet, "/events", nil, &events); err != nil {
		return nil, fmt.Errorf("get events: %w", err)
	}
	return events, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func (c *GeyserAdminClient) keepalive(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodGet, "/keepalive", nil, nil)
}

// doJSON performs a request and decodes the response payload — unwrapping the
// {body, status, headers} envelope when present — into out (which may be nil
// for calls whose response body is irrelevant).
func (c *GeyserAdminClient) doJSON(ctx context.Context, method, path string, payload, out interface{}) error {
	raw, err := c.doRaw(ctx, method, path, payload)
	if err != nil {
		return err
	}
	body, err := geyserBody(raw)
	if err != nil {
		return err
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response from %s %s: %w", method, path, err)
	}
	return nil
}

// doRaw performs an authenticated console request and returns the raw response
// body. Any 2xx status is a success (login returns 201). Session cookies come
// from the jar; when the jar lacks them (jar unavailable, or direct token
// injection before any request), the stored accessToken/userId are attached
// manually as a fallback.
func (c *GeyserAdminClient) doRaw(ctx context.Context, method, path string, payload interface{}) ([]byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method,
		geyserConsoleBase+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", "https://console.geyserdata.com")
	req.Header.Set("Referer", "https://console.geyserdata.com/")
	req.Header.Set("X-Source", "UI")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	jarHas := map[string]bool{}
	if c.httpClient.Jar != nil {
		for _, ck := range c.httpClient.Jar.Cookies(req.URL) {
			jarHas[ck.Name] = true
		}
	}
	c.mu.Lock()
	accessToken := c.accessToken
	userID := c.userID
	c.mu.Unlock()
	if accessToken != "" && !jarHas["accessToken"] {
		req.AddCookie(&http.Cookie{Name: "accessToken", Value: accessToken}) // #nosec G124 — outgoing request cookie to Geyser API, not served to users
	}
	if userID != "" && !jarHas["userId"] {
		req.AddCookie(&http.Cookie{Name: "userId", Value: userID}) // #nosec G124 — outgoing request cookie to Geyser API, not served to users
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response from %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected status %d from %s %s: %s",
			resp.StatusCode, method, path, string(raw))
	}
	return raw, nil
}

// geyserBody unwraps a console API response. Endpoints are inconsistent: some
// wrap payloads in a {body, status, headers} envelope (/api/buckets,
// /api/tapeCollections), others return bare JSON (/api/sites, /api/keys).
// A non-"OK" envelope status is surfaced as an error. Bare responses (or
// bodies with a coincidental non-envelope "status" field, like bucket status
// objects) are returned as-is.
func geyserBody(raw []byte) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var probe struct {
		Body    json.RawMessage `json:"body"`
		Status  *string         `json:"status"`
		Headers json.RawMessage `json:"headers"`
	}
	// Envelope iff a "body" key is present, or "status" and "headers" appear
	// together (status-only responses like airgap/keepalive acks).
	if err := json.Unmarshal(raw, &probe); err == nil &&
		(probe.Body != nil || (probe.Status != nil && probe.Headers != nil)) {
		if probe.Status != nil && *probe.Status != "OK" {
			return nil, fmt.Errorf("geyser API returned status %q", *probe.Status)
		}
		return probe.Body, nil
	}
	return raw, nil
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
