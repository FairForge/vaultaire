// internal/drivers/geyser_admin.go
//
// GeyserAdmin provides programmatic access to Geyser's console API
// (console.geyserdata.com). Originally reverse-engineered from the console's
// JS bundles (2026-07-29); since 2026-09-19 the console's own OpenAPI 3.1 spec
// (GET /api/v3/api-docs, authenticated) is the ground truth, alongside
// internal/drivers/geyser_README.md ("Console API map").
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

// GeyserTape is a single physical tape usage entry within a collection.
// Field names live-verified 2026-09-19 (barcode 140241L9, serial
// HPE-1925523288, type "lto9"). Collections return an hourly time-series of
// these entries under tapeUsage, so the same tape can appear repeatedly with
// different start/end times.
type GeyserTape struct {
	TapeID            string `json:"tapeId"`
	Barcode           string `json:"barcode"`
	SerialNumber      string `json:"serialNumber"`
	Type              string `json:"type"` // e.g. "lto9"
	AvailableCapacity int64  `json:"availableCapacity"`
	TotalCapacity     int64  `json:"totalCapacity"`
	WriteProtected    bool   `json:"writeProtected"`
	LastAccessed      string `json:"lastAccessed"`
	StartTime         string `json:"startTime"`
	EndTime           string `json:"endTime"`
}

// GeyserDatacenter is a datacenter reference (nested in collections/buckets,
// and the row shape of GET /api/datacenters, which lists only datacenters
// currently open for new provisioning).
type GeyserDatacenter struct {
	ID   string `json:"id"`
	Name string `json:"name"` // e.g. "LA1", "LA2", "LON1", "SP1"
	Geo  string `json:"geo"`
}

// GeyserSustainability is Geyser's per-collection carbon-savings metric.
type GeyserSustainability struct {
	PercentSaved float64 `json:"percentSaved"`
}

// GeyserTapeCollection is a tape collection from GET /api/tapeCollections.
// Size is the provisioned capacity in TB — the number Geyser bills on.
type GeyserTapeCollection struct {
	ID             string               `json:"id"`
	Name           string               `json:"name"`
	Size           int                  `json:"size"` // provisioned TB
	TapeCount      int                  `json:"tapeCount"`
	DualCopy       bool                 `json:"dualCopy"`
	Compression    bool                 `json:"compression"`
	Encryption     bool                 `json:"encryption"`
	CreatedAt      string               `json:"createdAt"`
	Datacenter     GeyserDatacenter     `json:"datacenter"`
	Sustainability GeyserSustainability `json:"sustainability"`
	TapeUsage      []GeyserTape         `json:"tapeUsage"`
}

// GeyserSite is a Geyser site from GET /api/sites. The wire carries only the
// geography string and id (live-confirmed: London UK, Los Angeles US,
// Sao Paulo Brazil).
type GeyserSite struct {
	ID  string `json:"id"`
	Geo string `json:"geo"`
}

// GeyserEvent is an audit-log entry from GET /api/events.
// Field names live-verified 2026-09-19: Name is the event kind ("login",
// "bucketCreated", "tapeCollectionCreated", ...), Result is SUCCESS/FAILURE.
type GeyserEvent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Result      string `json:"result,omitempty"`
	Severity    string `json:"severity,omitempty"`
	Created     string `json:"created"`
}

// GeyserBrowseEntry is one row of GET /api/buckets/{id}/browse. Location is
// the console's per-object storage location ("CACHE" while staged) — a cleaner
// signal than parsing the spectra-storage S3 metadata header.
type GeyserBrowseEntry struct {
	Key          string `json:"key"`
	IsFolder     bool   `json:"isFolder"`
	Size         int64  `json:"size"`
	Location     string `json:"location,omitempty"` // e.g. "CACHE"
	LastModified string `json:"lastModified,omitempty"`
	VersionID    string `json:"versionId,omitempty"`
}

// GeyserBucketSizePoint is one point of the windowed logical-size time-series
// from GET /api/buckets/{id}/size (not a lifetime total).
type GeyserBucketSizePoint struct {
	ID          string `json:"id"`
	BucketID    string `json:"bucketId"`
	StartTime   string `json:"startTime"`
	EndTime     string `json:"endTime"`
	LogicalSize int64  `json:"logicalSize"`
}

// GeyserFeaturePricing is a per-TB price triple (base storage plus the
// compression and encryption add-ons).
type GeyserFeaturePricing struct {
	TB          float64 `json:"tb"`
	Compression float64 `json:"compression"`
	Encryption  float64 `json:"encryption"`
}

// GeyserDatacenterPricing is one row of GET /api/datacenterpricing: the
// customer list price and our reseller wholesale cost for a datacenter.
type GeyserDatacenterPricing struct {
	ID           string               `json:"id"`
	DatacenterID string               `json:"datacenterId"`
	ListPrice    GeyserFeaturePricing `json:"datacenterCustomerListPrice"`
	ResellerCost GeyserFeaturePricing `json:"datacenterResellerCost"`
}

// GeyserEstimateBucketParams is one bucket line in an estimate request.
// DualCopy is a string ("true"/"false") on the wire, matching the console UI.
type GeyserEstimateBucketParams struct {
	DualCopy     string  `json:"dualCopy"`
	Size         float64 `json:"size"` // TB; fractional values accepted
	DatacenterID string  `json:"datacenterId"`
	Compression  bool    `json:"compression"`
	Encryption   bool    `json:"encryption"`
	S3Enabled    bool    `json:"s3Enabled"`
}

// GeyserEstimateRequest is the payload for POST /api/estimates (the reseller
// quote engine). Discount is a percentage; negative values are surcharges.
type GeyserEstimateRequest struct {
	BucketParamsList []GeyserEstimateBucketParams `json:"bucketParamsList"`
	Discount         float64                      `json:"discount"`
	NewCustomer      bool                         `json:"newCustomer"`
	SendEmail        bool                         `json:"sendEmail"`
	EmailMe          bool                         `json:"emailMe"`
}

// GeyserEstimate is the response to POST /api/estimates. ResellerMargin is our
// cut in percent; Geyser's wholesale take is Total × (1 − ResellerMargin/100).
type GeyserEstimate struct {
	Subtotal       float64             `json:"subtotal"`
	Total          float64             `json:"total"`
	Discount       float64             `json:"discount"`
	ResellerMargin float64             `json:"resellerMargin"`
	MiscBilling    []GeyserMiscBilling `json:"miscBilling"`
}

// GeyserTapeCollectionRequest is the payload for POST /api/tapeCollections and
// PUT /api/tapeCollections/{id} (resize — both grow and shrink are accepted,
// live-verified 2026-09-19). All fields are required by the console API.
type GeyserTapeCollectionRequest struct {
	Name         string `json:"name"`
	Size         int    `json:"size"` // provisioned TB
	DatacenterID string `json:"datacenterId"`
	CustomerID   string `json:"customerId"`
	DualCopy     bool   `json:"dualCopy"`
	Compression  bool   `json:"compression"`
	Encryption   bool   `json:"encryption"`
	Color        string `json:"color"`
	Icon         string `json:"icon"`
}

// geyserEnvelope is the response wrapper some console endpoints use.
type geyserEnvelope struct {
	Body    json.RawMessage `json:"body"`
	Status  string          `json:"status"`
	Headers struct {
		AuthID string `json:"authId"`
	} `json:"headers"`
}

// createBucketRequest is the payload for POST /api/buckets. The console
// OpenAPI spec marks versioning, icon and color required; live-verified
// values 2026-09-19. Versioning stays SUSPENDED (prod guidance — Geyser
// leaves delete markers on every delete when enabled); Object Lock requires
// versioning ENABLED at creation.
type createBucketRequest struct {
	Name             string `json:"name"`
	TapeCollectionID string `json:"tapeCollectionId"`
	Size             int    `json:"size"`
	DatacenterID     string `json:"datacenterId"`
	CustomerID       string `json:"customerId"`
	Versioning       string `json:"versioning"`
	ObjectLocking    bool   `json:"objectLocking"`
	Icon             string `json:"icon"`
	Color            string `json:"color"`
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
		Versioning:       "SUSPENDED",
		Icon:             "hard-drive-icon-outline",
		Color:            "#3146FF",
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

			// "CREATED" is the terminal status on the live wire (2026-09-19);
			// "ACTIVE" is kept in case older spheres still report it.
			if status.Status == "ACTIVE" || status.Status == "CREATED" {
				c.logger.Info("bucket is provisioned",
					zap.String("bucketID", bucketID),
					zap.String("status", status.Status),
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
	if err := c.doList(ctx, http.MethodGet, "/invoices", nil, &invoices); err != nil {
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
	if err := c.doList(ctx, http.MethodGet, "/tapeCollections", nil, &collections); err != nil {
		return nil, fmt.Errorf("get tape collections: %w", err)
	}
	return collections, nil
}

// GetSites returns Geyser's datacenters (live-confirmed: Los Angeles US,
// London UK, São Paulo Brazil).
func (c *GeyserAdminClient) GetSites(ctx context.Context) ([]GeyserSite, error) {
	var sites []GeyserSite
	if err := c.doList(ctx, http.MethodGet, "/sites", nil, &sites); err != nil {
		return nil, fmt.Errorf("get sites: %w", err)
	}
	return sites, nil
}

// GetEvents returns the account audit log (logins, actions, timestamps).
func (c *GeyserAdminClient) GetEvents(ctx context.Context) ([]GeyserEvent, error) {
	var events []GeyserEvent
	if err := c.doList(ctx, http.MethodGet, "/events", nil, &events); err != nil {
		return nil, fmt.Errorf("get events: %w", err)
	}
	return events, nil
}

// ── Console OpenAPI additions (live-verified 2026-09-19) ─────────────────────

// GetBucketAccess returns the S3 keypairs valid for a bucket
// (GET /api/buckets/{id}/access). An empty result for a reachable bucket has
// been observed to mean the bucket's site is unreachable, not "no keys".
func (c *GeyserAdminClient) GetBucketAccess(ctx context.Context, bucketID string) ([]GeyserKeyInfo, error) {
	var keys []GeyserKeyInfo
	if err := c.doList(ctx, http.MethodGet, fmt.Sprintf("/buckets/%s/access", bucketID), nil, &keys); err != nil {
		return nil, fmt.Errorf("get bucket access %s: %w", bucketID, err)
	}
	return keys, nil
}

// BrowseBucket lists a bucket's contents under prefix via the console
// (GET /api/buckets/{id}/browse). Unlike S3 ListObjectsV2 it carries each
// object's storage Location ("CACHE" while staged) — the per-object
// tape-vs-staged signal the dashboard wants.
func (c *GeyserAdminClient) BrowseBucket(ctx context.Context, bucketID, prefix string) ([]GeyserBrowseEntry, error) {
	var resp struct {
		Contents []GeyserBrowseEntry `json:"contents"`
	}
	path := fmt.Sprintf("/buckets/%s/browse?prefix=%s", bucketID, url.QueryEscape(prefix))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("browse bucket %s prefix %q: %w", bucketID, prefix, err)
	}
	return resp.Contents, nil
}

// PresignUpload mints a presigned PUT URL for path via the console
// (POST /api/buckets/{id}/upload) — a data path that needs no S3 credentials.
func (c *GeyserAdminClient) PresignUpload(ctx context.Context, bucketID, path string) (string, error) {
	return c.presign(ctx, bucketID, "upload", path)
}

// PresignDownload mints a presigned GET URL for path via the console
// (POST /api/buckets/{id}/download).
func (c *GeyserAdminClient) PresignDownload(ctx context.Context, bucketID, path string) (string, error) {
	return c.presign(ctx, bucketID, "download", path)
}

func (c *GeyserAdminClient) presign(ctx context.Context, bucketID, direction, path string) (string, error) {
	payload := struct {
		Path string `json:"path"`
	}{Path: path}
	var resp struct {
		URL string `json:"url"`
	}
	if err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/buckets/%s/%s", bucketID, direction), payload, &resp); err != nil {
		return "", fmt.Errorf("presign %s %s in bucket %s: %w", direction, path, bucketID, err)
	}
	return resp.URL, nil
}

// GetBucketSizeHistory returns the windowed logical-size time-series for a
// bucket (GET /api/buckets/{id}/size). Points cover a window, not lifetime.
func (c *GeyserAdminClient) GetBucketSizeHistory(ctx context.Context, bucketID string) ([]GeyserBucketSizePoint, error) {
	var points []GeyserBucketSizePoint
	if err := c.doList(ctx, http.MethodGet, fmt.Sprintf("/buckets/%s/size", bucketID), nil, &points); err != nil {
		return nil, fmt.Errorf("get bucket size history %s: %w", bucketID, err)
	}
	return points, nil
}

// GetDatacenters returns the datacenters currently open for new provisioning
// (GET /api/datacenters). Note: a datacenter hosting existing collections can
// be absent here once closed to new buckets (LA1 disappeared when LA2 opened).
func (c *GeyserAdminClient) GetDatacenters(ctx context.Context) ([]GeyserDatacenter, error) {
	var dcs []GeyserDatacenter
	if err := c.doList(ctx, http.MethodGet, "/datacenters", nil, &dcs); err != nil {
		return nil, fmt.Errorf("get datacenters: %w", err)
	}
	return dcs, nil
}

// GetDatacenterPricing returns the per-datacenter price table — customer list
// price and our reseller wholesale cost (GET /api/datacenterpricing).
func (c *GeyserAdminClient) GetDatacenterPricing(ctx context.Context) ([]GeyserDatacenterPricing, error) {
	var rows []GeyserDatacenterPricing
	if err := c.doList(ctx, http.MethodGet, "/datacenterpricing", nil, &rows); err != nil {
		return nil, fmt.Errorf("get datacenter pricing: %w", err)
	}
	return rows, nil
}

// Estimate runs Geyser's reseller quote engine (POST /api/estimates). It is a
// pure calculation — nothing is provisioned. Set SendEmail/EmailMe to false
// (the zero value) unless the quote should actually be emailed.
func (c *GeyserAdminClient) Estimate(ctx context.Context, req GeyserEstimateRequest) (*GeyserEstimate, error) {
	var est GeyserEstimate
	if err := c.doJSON(ctx, http.MethodPost, "/estimates", req, &est); err != nil {
		return nil, fmt.Errorf("estimate: %w", err)
	}
	return &est, nil
}

// CreateTapeCollection provisions a new tape collection
// (POST /api/tapeCollections). Billing is on provisioned TBs, so Size is a
// billing commitment — within the account minimum, extra collections are $0
// marginal.
func (c *GeyserAdminClient) CreateTapeCollection(ctx context.Context, req GeyserTapeCollectionRequest) (*GeyserTapeCollection, error) {
	var col GeyserTapeCollection
	if err := c.doJSON(ctx, http.MethodPost, "/tapeCollections", req, &col); err != nil {
		return nil, fmt.Errorf("create tape collection %q: %w", req.Name, err)
	}
	c.logger.Info("tape collection created",
		zap.String("id", col.ID),
		zap.String("name", col.Name),
		zap.Int("sizeTB", col.Size))
	return &col, nil
}

// UpdateTapeCollection updates a collection in place
// (PUT /api/tapeCollections/{id}). Resizing works in both directions
// (live-verified 1→2→1 TB, 2026-09-19), making provisioned capacity — and
// therefore the bill — adjustable month to month.
func (c *GeyserAdminClient) UpdateTapeCollection(ctx context.Context, id string, req GeyserTapeCollectionRequest) (*GeyserTapeCollection, error) {
	var col GeyserTapeCollection
	if err := c.doJSON(ctx, http.MethodPut, fmt.Sprintf("/tapeCollections/%s", id), req, &col); err != nil {
		return nil, fmt.Errorf("update tape collection %s: %w", id, err)
	}
	c.logger.Info("tape collection updated",
		zap.String("id", id),
		zap.Int("sizeTB", col.Size))
	return &col, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func (c *GeyserAdminClient) keepalive(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodGet, "/keepalive", nil, nil)
}

// doList performs a request against a list endpoint and decodes the rows into
// out (a pointer to a slice). It tolerates every wrapper the console mixes:
// the {body, status, headers} envelope, Spring's {content, page} pagination
// (the live shape of /buckets, /tapeCollections, /sites, /events, /invoices,
// /datacenters, /datacenterpricing as of 2026-09-19), both nested, or a bare
// JSON array (/keys).
func (c *GeyserAdminClient) doList(ctx context.Context, method, path string, payload, out interface{}) error {
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
	var paged struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(body, &paged); err == nil && paged.Content != nil {
		body = paged.Content
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode list response from %s %s: %w", method, path, err)
	}
	return nil
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
