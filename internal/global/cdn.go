// internal/global/cdn.go
package global

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CDNProvider represents supported CDN providers
type CDNProvider string

const (
	CDNCloudflare CDNProvider = "cloudflare"
	CDNFastly     CDNProvider = "fastly"
	CDNBunnyCDN   CDNProvider = "bunnycdn"
	CDNCloudFront CDNProvider = "cloudfront"
	CDNAkamai     CDNProvider = "akamai"
)

// CDNConfig configures a CDN provider
type CDNConfig struct {
	Provider   CDNProvider
	Enabled    bool
	ZoneID     string            // Cloudflare zone, Fastly service, etc.
	APIKey     string            // API key or token
	APISecret  string            // Secret for signed URLs
	BaseURL    string            // CDN base URL
	OriginURL  string            // Origin server URL
	TTL        time.Duration     // Default cache TTL
	Headers    map[string]string // Custom headers
	PullZoneID string            // BunnyCDN pull zone
	KeyPairID  string            // CloudFront key pair ID
	PrivateKey string            // CloudFront private key
}

// CDNClient provides CDN operations
type CDNClient interface {
	// Cache operations
	PurgeURL(ctx context.Context, url string) error
	PurgePrefix(ctx context.Context, prefix string) error
	PurgeAll(ctx context.Context) error

	// URL generation
	SignedURL(key string, expiry time.Duration) (string, error)
	PublicURL(key string) string

	// Cache control
	SetCacheHeaders(key string, ttl time.Duration, tags []string) map[string]string

	// Health and metrics
	Health(ctx context.Context) error
	Provider() CDNProvider
}

// CDNManager manages multiple CDN providers
type CDNManager struct {
	mu      sync.RWMutex
	primary CDNClient
	clients map[CDNProvider]CDNClient
	config  map[CDNProvider]*CDNConfig
	metrics *CDNMetrics
}

// CDNMetrics tracks CDN performance
type CDNMetrics struct {
	mu          sync.RWMutex
	PurgeCount  map[CDNProvider]int64
	PurgeErrors map[CDNProvider]int64
	SignedURLs  map[CDNProvider]int64
	LastPurge   map[CDNProvider]time.Time
	LastError   map[CDNProvider]error
}

// NewCDNManager creates a new CDN manager
func NewCDNManager() *CDNManager {
	return &CDNManager{
		clients: make(map[CDNProvider]CDNClient),
		config:  make(map[CDNProvider]*CDNConfig),
		metrics: &CDNMetrics{
			PurgeCount:  make(map[CDNProvider]int64),
			PurgeErrors: make(map[CDNProvider]int64),
			SignedURLs:  make(map[CDNProvider]int64),
			LastPurge:   make(map[CDNProvider]time.Time),
			LastError:   make(map[CDNProvider]error),
		},
	}
}

// RegisterProvider registers a CDN provider
func (m *CDNManager) RegisterProvider(config *CDNConfig) error {
	if !config.Enabled {
		return nil
	}

	var client CDNClient
	var err error

	switch config.Provider {
	case CDNCloudflare:
		client, err = NewCloudflareClient(config)
	case CDNFastly:
		client, err = NewFastlyClient(config)
	case CDNBunnyCDN:
		client, err = NewBunnyCDNClient(config)
	case CDNCloudFront:
		client, err = NewCloudFrontClient(config)
	default:
		return fmt.Errorf("unsupported CDN provider: %s", config.Provider)
	}

	if err != nil {
		return fmt.Errorf("failed to create %s client: %w", config.Provider, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.clients[config.Provider] = client
	m.config[config.Provider] = config

	if m.primary == nil {
		m.primary = client
	}

	return nil
}

// SetPrimary sets the primary CDN provider
func (m *CDNManager) SetPrimary(provider CDNProvider) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	client, ok := m.clients[provider]
	if !ok {
		return fmt.Errorf("provider %s not registered", provider)
	}

	m.primary = client
	return nil
}

// Primary returns the primary CDN client
func (m *CDNManager) Primary() CDNClient {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.primary
}

// Client returns a specific CDN client
func (m *CDNManager) Client(provider CDNProvider) (CDNClient, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	client, ok := m.clients[provider]
	return client, ok
}

// PurgeURL purges a URL from all CDNs
func (m *CDNManager) PurgeURL(ctx context.Context, url string) error {
	m.mu.RLock()
	clients := make([]CDNClient, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.mu.RUnlock()

	var errs []error
	for _, client := range clients {
		if err := client.PurgeURL(ctx, url); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", client.Provider(), err))
			m.recordError(client.Provider(), err)
		} else {
			m.recordPurge(client.Provider())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("purge errors: %v", errs)
	}
	return nil
}

// PurgePrefix purges all URLs with prefix from all CDNs
func (m *CDNManager) PurgePrefix(ctx context.Context, prefix string) error {
	m.mu.RLock()
	clients := make([]CDNClient, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.mu.RUnlock()

	var errs []error
	for _, client := range clients {
		if err := client.PurgePrefix(ctx, prefix); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", client.Provider(), err))
			m.recordError(client.Provider(), err)
		} else {
			m.recordPurge(client.Provider())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("purge errors: %v", errs)
	}
	return nil
}

// SignedURL generates a signed URL using the primary CDN
func (m *CDNManager) SignedURL(key string, expiry time.Duration) (string, error) {
	m.mu.RLock()
	primary := m.primary
	m.mu.RUnlock()

	if primary == nil {
		return "", fmt.Errorf("no CDN provider configured")
	}

	url, err := primary.SignedURL(key, expiry)
	if err == nil {
		m.recordSignedURL(primary.Provider())
	}
	return url, err
}

// PublicURL returns a public CDN URL
func (m *CDNManager) PublicURL(key string) string {
	m.mu.RLock()
	primary := m.primary
	m.mu.RUnlock()

	if primary == nil {
		return key
	}

	return primary.PublicURL(key)
}

func (m *CDNManager) recordPurge(provider CDNProvider) {
	m.metrics.mu.Lock()
	defer m.metrics.mu.Unlock()
	m.metrics.PurgeCount[provider]++
	m.metrics.LastPurge[provider] = time.Now()
}

func (m *CDNManager) recordError(provider CDNProvider, err error) {
	m.metrics.mu.Lock()
	defer m.metrics.mu.Unlock()
	m.metrics.PurgeErrors[provider]++
	m.metrics.LastError[provider] = err
}

func (m *CDNManager) recordSignedURL(provider CDNProvider) {
	m.metrics.mu.Lock()
	defer m.metrics.mu.Unlock()
	m.metrics.SignedURLs[provider]++
}

// GetMetrics returns CDN metrics
func (m *CDNManager) GetMetrics() map[CDNProvider]map[string]interface{} {
	m.metrics.mu.RLock()
	defer m.metrics.mu.RUnlock()

	result := make(map[CDNProvider]map[string]interface{})
	for provider := range m.clients {
		result[provider] = map[string]interface{}{
			"purge_count":  m.metrics.PurgeCount[provider],
			"purge_errors": m.metrics.PurgeErrors[provider],
			"signed_urls":  m.metrics.SignedURLs[provider],
			"last_purge":   m.metrics.LastPurge[provider],
		}
		if err := m.metrics.LastError[provider]; err != nil {
			result[provider]["last_error"] = err.Error()
		}
	}
	return result
}

// CloudflareClient implements CDN operations for Cloudflare
type CloudflareClient struct {
	config     *CDNConfig
	httpClient *http.Client
}

// NewCloudflareClient creates a Cloudflare CDN client
func NewCloudflareClient(config *CDNConfig) (*CloudflareClient, error) {
	if config.ZoneID == "" {
		return nil, fmt.Errorf("zone ID required for Cloudflare")
	}
	if config.APIKey == "" {
		return nil, fmt.Errorf("API key required for Cloudflare")
	}

	return &CloudflareClient{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

func (c *CloudflareClient) Provider() CDNProvider {
	return CDNCloudflare
}

func (c *CloudflareClient) PurgeURL(ctx context.Context, purgeURL string) error {
	return c.purge(ctx, map[string]interface{}{
		"files": []string{purgeURL},
	})
}

func (c *CloudflareClient) PurgePrefix(ctx context.Context, prefix string) error {
	return c.purge(ctx, map[string]interface{}{
		"prefixes": []string{prefix},
	})
}

func (c *CloudflareClient) PurgeAll(ctx context.Context) error {
	return c.purge(ctx, map[string]interface{}{
		"purge_everything": true,
	})
}

func (c *CloudflareClient) purge(ctx context.Context, payload map[string]interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal purge request: %w", err)
	}

	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/purge_cache", c.config.ZoneID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("purge request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("purge failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (c *CloudflareClient) SignedURL(key string, expiry time.Duration) (string, error) {
	if c.config.APISecret == "" {
		return "", fmt.Errorf("API secret required for signed URLs")
	}

	expires := time.Now().Add(expiry).Unix()
	message := fmt.Sprintf("%s%d", key, expires)

	mac := hmac.New(sha256.New, []byte(c.config.APISecret))
	mac.Write([]byte(message))
	signature := base64.URLEncoding.EncodeToString(mac.Sum(nil))

	return fmt.Sprintf("%s/%s?expires=%d&signature=%s",
		strings.TrimSuffix(c.config.BaseURL, "/"),
		strings.TrimPrefix(key, "/"),
		expires,
		signature,
	), nil
}

func (c *CloudflareClient) PublicURL(key string) string {
	return fmt.Sprintf("%s/%s",
		strings.TrimSuffix(c.config.BaseURL, "/"),
		strings.TrimPrefix(key, "/"),
	)
}

func (c *CloudflareClient) SetCacheHeaders(key string, ttl time.Duration, tags []string) map[string]string {
	headers := map[string]string{
		"Cache-Control": fmt.Sprintf("public, max-age=%d", int(ttl.Seconds())),
	}
	if len(tags) > 0 {
		headers["Cache-Tag"] = strings.Join(tags, ",")
	}
	return headers
}

func (c *CloudflareClient) Health(ctx context.Context) error {
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s", c.config.ZoneID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed with status %d", resp.StatusCode)
	}

	return nil
}

// FastlyClient implements CDN operations for Fastly
type FastlyClient struct {
	config     *CDNConfig
	httpClient *http.Client
}

// NewFastlyClient creates a Fastly CDN client
func NewFastlyClient(config *CDNConfig) (*FastlyClient, error) {
	if config.ZoneID == "" {
		return nil, fmt.Errorf("service ID required for Fastly")
	}
	if config.APIKey == "" {
		return nil, fmt.Errorf("API key required for Fastly")
	}

	return &FastlyClient{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

func (c *FastlyClient) Provider() CDNProvider {
	return CDNFastly
}

func (c *FastlyClient) PurgeURL(ctx context.Context, purgeURL string) error {
	req, err := http.NewRequestWithContext(ctx, "PURGE", purgeURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Fastly-Key", c.config.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("purge request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("purge failed with status %d", resp.StatusCode)
	}

	return nil
}

func (c *FastlyClient) PurgePrefix(ctx context.Context, prefix string) error {
	url := fmt.Sprintf("https://api.fastly.com/service/%s/purge/%s",
		c.config.ZoneID, strings.TrimPrefix(prefix, "/"))

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Fastly-Key", c.config.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("purge request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("purge failed with status %d", resp.StatusCode)
	}

	return nil
}

func (c *FastlyClient) PurgeAll(ctx context.Context) error {
	url := fmt.Sprintf("https://api.fastly.com/service/%s/purge_all", c.config.ZoneID)

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Fastly-Key", c.config.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("purge request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("purge failed with status %d", resp.StatusCode)
	}

	return nil
}

func (c *FastlyClient) SignedURL(key string, expiry time.Duration) (string, error) {
	if c.config.APISecret == "" {
		return "", fmt.Errorf("API secret required for signed URLs")
	}

	expires := time.Now().Add(expiry).Unix()
	path := "/" + strings.TrimPrefix(key, "/")
	message := fmt.Sprintf("%s%d", path, expires)

	mac := hmac.New(sha256.New, []byte(c.config.APISecret))
	mac.Write([]byte(message))
	signature := base64.URLEncoding.EncodeToString(mac.Sum(nil))

	return fmt.Sprintf("%s%s?expires=%d&signature=%s",
		strings.TrimSuffix(c.config.BaseURL, "/"),
		path,
		expires,
		signature,
	), nil
}

func (c *FastlyClient) PublicURL(key string) string {
	return fmt.Sprintf("%s/%s",
		strings.TrimSuffix(c.config.BaseURL, "/"),
		strings.TrimPrefix(key, "/"),
	)
}

func (c *FastlyClient) SetCacheHeaders(key string, ttl time.Duration, tags []string) map[string]string {
	headers := map[string]string{
		"Cache-Control":     fmt.Sprintf("public, max-age=%d", int(ttl.Seconds())),
		"Surrogate-Control": fmt.Sprintf("max-age=%d", int(ttl.Seconds())),
	}
	if len(tags) > 0 {
		headers["Surrogate-Key"] = strings.Join(tags, " ")
	}
	return headers
}

func (c *FastlyClient) Health(ctx context.Context) error {
	url := fmt.Sprintf("https://api.fastly.com/service/%s/details", c.config.ZoneID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Fastly-Key", c.config.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed with status %d", resp.StatusCode)
	}

	return nil
}

// BunnyCDNClient implements CDN operations for BunnyCDN
type BunnyCDNClient struct {
	config     *CDNConfig
	httpClient *http.Client
}

// NewBunnyCDNClient creates a BunnyCDN client
func NewBunnyCDNClient(config *CDNConfig) (*BunnyCDNClient, error) {
	if config.PullZoneID == "" {
		return nil, fmt.Errorf("pull zone ID required for BunnyCDN")
	}
	if config.APIKey == "" {
		return nil, fmt.Errorf("API key required for BunnyCDN")
	}

	return &BunnyCDNClient{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

func (c *BunnyCDNClient) Provider() CDNProvider {
	return CDNBunnyCDN
}

func (c *BunnyCDNClient) PurgeURL(ctx context.Context, purgeURL string) error {
	apiURL := fmt.Sprintf("https://api.bunny.net/purge?url=%s", url.QueryEscape(purgeURL))

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("AccessKey", c.config.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("purge request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("purge failed with status %d", resp.StatusCode)
	}

	return nil
}

func (c *BunnyCDNClient) PurgePrefix(ctx context.Context, prefix string) error {
	// BunnyCDN purges by URL pattern
	purgeURL := fmt.Sprintf("%s/%s*",
		strings.TrimSuffix(c.config.BaseURL, "/"),
		strings.TrimPrefix(prefix, "/"))
	return c.PurgeURL(ctx, purgeURL)
}

func (c *BunnyCDNClient) PurgeAll(ctx context.Context) error {
	apiURL := fmt.Sprintf("https://api.bunny.net/pullzone/%s/purgeCache", c.config.PullZoneID)

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("AccessKey", c.config.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("purge request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("purge failed with status %d", resp.StatusCode)
	}

	return nil
}

func (c *BunnyCDNClient) SignedURL(key string, expiry time.Duration) (string, error) {
	if c.config.APISecret == "" {
		return "", fmt.Errorf("API secret required for signed URLs")
	}

	expires := time.Now().Add(expiry).Unix()
	path := "/" + strings.TrimPrefix(key, "/")

	// BunnyCDN token authentication
	hashableBase := fmt.Sprintf("%s%s%d", c.config.APISecret, path, expires)
	hash := sha256.Sum256([]byte(hashableBase))
	token := base64.URLEncoding.EncodeToString(hash[:])
	token = strings.ReplaceAll(token, "=", "")

	return fmt.Sprintf("%s%s?token=%s&expires=%d",
		strings.TrimSuffix(c.config.BaseURL, "/"),
		path,
		token,
		expires,
	), nil
}

func (c *BunnyCDNClient) PublicURL(key string) string {
	return fmt.Sprintf("%s/%s",
		strings.TrimSuffix(c.config.BaseURL, "/"),
		strings.TrimPrefix(key, "/"),
	)
}

func (c *BunnyCDNClient) SetCacheHeaders(key string, ttl time.Duration, tags []string) map[string]string {
	return map[string]string{
		"Cache-Control": fmt.Sprintf("public, max-age=%d", int(ttl.Seconds())),
	}
}

func (c *BunnyCDNClient) Health(ctx context.Context) error {
	apiURL := fmt.Sprintf("https://api.bunny.net/pullzone/%s", c.config.PullZoneID)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return err
	}

	req.Header.Set("AccessKey", c.config.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed with status %d", resp.StatusCode)
	}

	return nil
}

// CloudFrontClient implements CDN operations for AWS CloudFront
type CloudFrontClient struct {
	config     *CDNConfig
	httpClient *http.Client
}

// NewCloudFrontClient creates a CloudFront CDN client
func NewCloudFrontClient(config *CDNConfig) (*CloudFrontClient, error) {
	if config.ZoneID == "" {
		return nil, fmt.Errorf("distribution ID required for CloudFront")
	}

	return &CloudFrontClient{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

func (c *CloudFrontClient) Provider() CDNProvider {
	return CDNCloudFront
}

func (c *CloudFrontClient) PurgeURL(ctx context.Context, purgeURL string) error {
	// CloudFront invalidation requires AWS SDK
	// This is a simplified placeholder - real implementation would use AWS SDK
	return fmt.Errorf("CloudFront purge requires AWS SDK integration")
}

func (c *CloudFrontClient) PurgePrefix(ctx context.Context, prefix string) error {
	return fmt.Errorf("CloudFront purge requires AWS SDK integration")
}

func (c *CloudFrontClient) PurgeAll(ctx context.Context) error {
	return fmt.Errorf("CloudFront purge requires AWS SDK integration")
}

func (c *CloudFrontClient) SignedURL(key string, expiry time.Duration) (string, error) {
	// CloudFront signed URLs require RSA signing
	// This is a placeholder - real implementation would use crypto/rsa
	if c.config.KeyPairID == "" || c.config.PrivateKey == "" {
		return "", fmt.Errorf("key pair ID and private key required for CloudFront signed URLs")
	}
	return "", fmt.Errorf("CloudFront signed URLs require RSA implementation")
}

func (c *CloudFrontClient) PublicURL(key string) string {
	return fmt.Sprintf("%s/%s",
		strings.TrimSuffix(c.config.BaseURL, "/"),
		strings.TrimPrefix(key, "/"),
	)
}

func (c *CloudFrontClient) SetCacheHeaders(key string, ttl time.Duration, tags []string) map[string]string {
	return map[string]string{
		"Cache-Control": fmt.Sprintf("public, max-age=%d", int(ttl.Seconds())),
	}
}

func (c *CloudFrontClient) Health(ctx context.Context) error {
	// Would require AWS SDK
	return nil
}

// CachePolicy defines caching behavior
type CachePolicy struct {
	Name               string
	DefaultTTL         time.Duration
	MaxTTL             time.Duration
	MinTTL             time.Duration
	QueryStringCaching string // "none", "all", "whitelist"
	QueryStringKeys    []string
	HeaderCaching      string // "none", "all", "whitelist"
	HeaderKeys         []string
	CookieCaching      string // "none", "all", "whitelist"
	CookieKeys         []string
	CompressionEnabled bool
}

// DefaultCachePolicies returns common cache policies
func DefaultCachePolicies() map[string]*CachePolicy {
	return map[string]*CachePolicy{
		"static": {
			Name:               "static",
			DefaultTTL:         24 * time.Hour,
			MaxTTL:             7 * 24 * time.Hour,
			MinTTL:             1 * time.Hour,
			QueryStringCaching: "none",
			HeaderCaching:      "none",
			CookieCaching:      "none",
			CompressionEnabled: true,
		},
		"dynamic": {
			Name:               "dynamic",
			DefaultTTL:         0,
			MaxTTL:             1 * time.Hour,
			MinTTL:             0,
			QueryStringCaching: "all",
			HeaderCaching:      "whitelist",
			HeaderKeys:         []string{"Authorization", "Accept-Encoding"},
			CookieCaching:      "none",
			CompressionEnabled: true,
		},
		"api": {
			Name:               "api",
			DefaultTTL:         0,
			MaxTTL:             0,
			MinTTL:             0,
			QueryStringCaching: "all",
			HeaderCaching:      "all",
			CookieCaching:      "all",
			CompressionEnabled: true,
		},
	}
}
