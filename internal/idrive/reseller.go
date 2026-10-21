// Package idrive is the iDrive e2 Reseller API client (WP-CAP-1 B; the full
// endpoint reference is .private/IDRIVE_RESELLER_API.md). Only the two
// read-only calls the backend_capacity job needs are built — Users and
// UsageStats; the provisioning calls (create_user, enable_user_region,
// create_access_key) come with the reseller integration proper.
//
// Auth is one static header, `token: <key>` — god-mode for the whole
// reseller fleet, so the key is read like the master key (the systemd
// credential idrive_reseller_api_key, then IDRIVE_RESELLER_API_KEY_FILE,
// then the environment; a source that is set but unusable is an error the
// caller turns into a refused boot) and never logged.
package idrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// ResellerBaseURL is the live API (§ 1 of the reference).
const ResellerBaseURL = "https://api.idrivee2.com/api/reseller/v1"

// CredentialResellerKey is the systemd credential name of the reseller API
// key ($CREDENTIALS_DIRECTORY/idrive_reseller_api_key).
const CredentialResellerKey = "idrive_reseller_api_key"

// ErrResellerKeySource: the key is set somewhere but cannot be used.
var ErrResellerKeySource = errors.New("iDrive reseller key source unusable")

// LoadResellerKey resolves IDRIVE_RESELLER_API_KEY by the master key's
// precedence (WP-R8-1 A5): the credential, then _FILE, then the variable.
// "" with no error = not set (the vendor view stays off, a note on the job).
func LoadResellerKey(getenv func(string) string, readFile func(string) ([]byte, error)) (string, error) {
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		raw, err := readFile(filepath.Join(dir, CredentialResellerKey))
		switch {
		case err == nil:
			return strings.TrimSpace(string(raw)), nil
		case errors.Is(err, fs.ErrNotExist):
			// The unit carries other credentials, or not this one: next source.
		default:
			return "", fmt.Errorf("%w: credential %s cannot be read", ErrResellerKeySource, CredentialResellerKey)
		}
	}
	if path := getenv("IDRIVE_RESELLER_API_KEY_FILE"); path != "" {
		raw, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("%w: IDRIVE_RESELLER_API_KEY_FILE cannot be read", ErrResellerKeySource)
		}
		if key := strings.TrimSpace(string(raw)); key != "" {
			return key, nil
		}
		return "", fmt.Errorf("%w: IDRIVE_RESELLER_API_KEY_FILE names an empty file", ErrResellerKeySource)
	}
	return strings.TrimSpace(getenv("IDRIVE_RESELLER_API_KEY")), nil
}

// ResellerClient asks the Reseller API. Every request is bounded (15 s).
type ResellerClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewResellerClient builds one; baseURL "" = the live API.
func NewResellerClient(baseURL, apiKey string) *ResellerClient {
	if baseURL == "" {
		baseURL = ResellerBaseURL
	}
	return &ResellerClient{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey,
		http: &http.Client{Timeout: 15 * time.Second}}
}

// User is one sub-account as GET /users lists it. StorageUsedGB is the
// console's unit (decimal GB) — the caller converts and sanity-checks it.
type User struct {
	Email         string  `json:"email"`
	StorageUsedGB float64 `json:"storage_used"`
	Enabled       bool    `json:"enabled"`
}

// UsageDay is one day of POST /usage_stats: bytes (§ 3.1 of the reference).
type UsageDay struct {
	Date      string `json:"date"`
	Uploads   int64  `json:"uploads"`
	Downloads int64  `json:"downloads"`
	DiskUsed  int64  `json:"disk_used"`
}

// Users lists the reseller's sub-accounts (optionally one, by e-mail).
func (c *ResellerClient) Users(ctx context.Context, email string) ([]User, error) {
	u := c.baseURL + "/users"
	if email != "" {
		u += "?email=" + url.QueryEscape(email)
	}
	var out []User
	if err := c.call(ctx, http.MethodGet, u, nil, &out); err != nil {
		return nil, fmt.Errorf("reseller users: %w", err)
	}
	return out, nil
}

// UsageStats is the user's daily usage over [from, to] (dates in UTC).
func (c *ResellerClient) UsageStats(ctx context.Context, email string, from, to time.Time) ([]UsageDay, error) {
	body := map[string]string{"email": email,
		"date_from": from.UTC().Format("2006-01-02"), "date_to": to.UTC().Format("2006-01-02")}
	var out []UsageDay
	if err := c.call(ctx, http.MethodPost, c.baseURL+"/usage_stats", body, &out); err != nil {
		return nil, fmt.Errorf("reseller usage_stats for %s: %w", email, err)
	}
	return out, nil
}

// call sends one request and decodes the answer's list into out, whether it
// arrives bare or wrapped (`data`, `users`, `stats` — the console wraps some
// answers in `data`, the doc shows others bare; both shapes are accepted).
func (c *ResellerClient) call(ctx context.Context, method, url string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("token", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, apiErrorText(raw))
	}
	return decodeList(raw, out)
}

// apiErrorText pulls the error message out of the API's error envelope
// (never the whole body: it could be large).
func apiErrorText(raw []byte) string {
	var env struct {
		Error struct {
			Type, Code, Message string
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
		return env.Error.Type + "/" + env.Error.Code + ": " + env.Error.Message
	}
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return strings.TrimSpace(string(raw))
}

// decodeList decodes a JSON list into out, unwrapping a {data|users|stats:
// [...]} envelope when one is there.
func decodeList(raw []byte, out any) error {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		return json.Unmarshal(raw, out)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode answer: %w", err)
	}
	for _, key := range []string{"data", "users", "stats", "usage_stats"} {
		if inner, ok := env[key]; ok && strings.HasPrefix(strings.TrimSpace(string(inner)), "[") {
			return json.Unmarshal(inner, out)
		}
	}
	return fmt.Errorf("decode answer: no list found (keys: %s)", strings.Join(mapKeys(env), ", "))
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
