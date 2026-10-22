package btcpay

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

// Config is the provider's four settings. All four or the provider is not
// registered (no route, no checkout) — LoadConfig names what is missing.
type Config struct {
	URL, StoreID, APIKey, WebhookSecret string
}

// Systemd credential names ($CREDENTIALS_DIRECTORY/<name>) of the two
// secrets: read before <VAR>_FILE, before <VAR> — the master key's order
// (WP-R8-1 A5), as IDRIVE_RESELLER_API_KEY.
const (
	CredentialAPIKey        = "btcpay_api_key"
	CredentialWebhookSecret = "btcpay_webhook_secret"
)

// ErrSecretSource: a secret is set somewhere but cannot be used (an
// unreadable credential or file, an empty file) — the caller refuses to
// boot, never "BTCPay off".
var ErrSecretSource = errors.New("BTCPay secret source unusable")

// ErrInsecureURL: BTCPAY_URL is plain http to a host that is not loopback —
// the store key would cross the network in clear (RP30-6, the Blomp
// lesson). The caller logs an Error and does not register the provider.
var ErrInsecureURL = errors.New("BTCPAY_URL is plain http to a host that is not loopback — https required")

// LoadConfig reads BTCPAY_URL, BTCPAY_STORE_ID, BTCPAY_API_KEY and
// BTCPAY_WEBHOOK_SECRET (the two secrets by credential → _FILE → env).
// missing lists the names of the settings that are not set, in that order;
// complete = none missing and BTCPAY_URL is an absolute https URL (plain
// http only to a loopback host — the lab; else ErrInsecureURL). Values are
// never in an error.
func LoadConfig(getenv func(string) string, readFile func(string) ([]byte, error)) (cfg Config, missing []string, err error) {
	cfg.URL = strings.TrimRight(strings.TrimSpace(getenv("BTCPAY_URL")), "/")
	cfg.StoreID = strings.TrimSpace(getenv("BTCPAY_STORE_ID"))
	if cfg.APIKey, err = loadSecret(getenv, readFile, "BTCPAY_API_KEY", CredentialAPIKey); err != nil {
		return cfg, nil, err
	}
	if cfg.WebhookSecret, err = loadSecret(getenv, readFile, "BTCPAY_WEBHOOK_SECRET", CredentialWebhookSecret); err != nil {
		return cfg, nil, err
	}
	for _, kv := range []struct{ name, v string }{
		{"BTCPAY_URL", cfg.URL}, {"BTCPAY_STORE_ID", cfg.StoreID},
		{"BTCPAY_API_KEY", cfg.APIKey}, {"BTCPAY_WEBHOOK_SECRET", cfg.WebhookSecret},
	} {
		if kv.v == "" {
			missing = append(missing, kv.name)
		}
	}
	if cfg.URL != "" {
		u, perr := url.Parse(cfg.URL)
		if perr != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return cfg, missing, fmt.Errorf("BTCPAY_URL must be an absolute http(s) URL")
		}
		if u.Scheme == "http" && !loopback(u.Hostname()) {
			return cfg, missing, ErrInsecureURL
		}
	}
	return cfg, missing, nil
}

// loopback reports localhost or a loopback IP (127.0.0.0/8, ::1).
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loadSecret(getenv func(string) string, readFile func(string) ([]byte, error), name, credential string) (string, error) {
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		raw, err := readFile(filepath.Join(dir, credential))
		switch {
		case err == nil:
			return strings.TrimSpace(string(raw)), nil
		case errors.Is(err, fs.ErrNotExist):
			// The unit carries other credentials, or not this one: next source.
		default:
			return "", fmt.Errorf("%w: credential %s cannot be read", ErrSecretSource, credential)
		}
	}
	if path := getenv(name + "_FILE"); path != "" {
		raw, err := readFile(path)
		if err != nil {
			return "", fmt.Errorf("%w: %s_FILE cannot be read", ErrSecretSource, name)
		}
		if v := strings.TrimSpace(string(raw)); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("%w: %s_FILE names an empty file", ErrSecretSource, name)
	}
	return strings.TrimSpace(getenv(name)), nil
}
