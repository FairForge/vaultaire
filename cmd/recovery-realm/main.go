// Command recovery-realm is one Juicebox-protocol realm (keystone K5,
// docs/reviews/K5-PROBE.md): it keeps PIN-protected shares for users of the
// tenants it serves, counts wrong guesses durably and erases a record at its
// limit. It is its own binary and is never linked into cmd/vaultaire
// (TestRecoveryRealm_NotLinkedIntoVaultaire): a realm belongs on another
// box, first the attested one of plan 41.4, and a second realm under a
// different operator before anything may call the limit hardware-enforced
// (F-19). Until then: "attempt-limited by our realm".
//
// Configuration (environment):
//
//	RECOVERY_REALM_ID              32 hex characters (required)
//	RECOVERY_REALM_DB              the bbolt file (required)
//	RECOVERY_REALM_LISTEN          default 127.0.0.1:8780
//	RECOVERY_REALM_TLS_CERT/_KEY   both or neither; without them the listen
//	                               address must be loopback
//	RECOVERY_REALM_METRICS_LISTEN  default 127.0.0.1:8781, "off" for none
//	recovery_realm_key             the Noise static secret, 64 hex: systemd
//	                               credential, else RECOVERY_REALM_KEY_FILE,
//	                               else RECOVERY_REALM_KEY (required)
//	recovery_realm_tenants         {"<tenant>": {"<version>": "<hex key>"}},
//	                               same precedence (RECOVERY_REALM_TENANTS…)
//
// A source that is set but unusable is a refused boot; no key is ever
// logged. Enrolling a realm is a key_ceremony entry in the transparency log
// (docs/TRANSPARENCY.md § Recovery realms): `recovery-realm enrolment`, with
// the same environment, prints the entry's body and exits.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/FairForge/vaultaire/internal/recovery"
	"github.com/FairForge/vaultaire/internal/recovery/realm"
	"github.com/FairForge/vaultaire/internal/recovery/secret"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

const (
	credentialKey     = "recovery_realm_key"
	envKey            = "RECOVERY_REALM_KEY"
	credentialTenants = "recovery_realm_tenants"
	envTenants        = "RECOVERY_REALM_TENANTS"
)

type config struct {
	id             recovery.RealmID
	dbPath         string
	listen         string
	metricsListen  string
	tlsCert, tlsKy string
	staticSecret   [32]byte
	keys           keyring
	sources        map[string]secret.Value // for the boot log: the source names only
}

func loadConfig(getenv func(string) string, readFile func(string) ([]byte, error)) (config, error) {
	var c config
	var err error
	if c.id, err = recovery.ParseRealmID(getenv("RECOVERY_REALM_ID")); err != nil {
		return c, fmt.Errorf("RECOVERY_REALM_ID: %w", err)
	}
	if c.dbPath = getenv("RECOVERY_REALM_DB"); c.dbPath == "" {
		return c, errors.New("RECOVERY_REALM_DB is required")
	}
	c.listen = orDefault(getenv("RECOVERY_REALM_LISTEN"), "127.0.0.1:8780")
	c.metricsListen = orDefault(getenv("RECOVERY_REALM_METRICS_LISTEN"), "127.0.0.1:8781")
	c.tlsCert, c.tlsKy = getenv("RECOVERY_REALM_TLS_CERT"), getenv("RECOVERY_REALM_TLS_KEY")
	if (c.tlsCert == "") != (c.tlsKy == "") {
		return c, errors.New("RECOVERY_REALM_TLS_CERT and RECOVERY_REALM_TLS_KEY go together")
	}
	if c.tlsCert == "" && !loopback(c.listen) {
		return c, fmt.Errorf("RECOVERY_REALM_LISTEN %s is not loopback and no TLS is configured", c.listen)
	}

	c.sources = map[string]secret.Value{}
	keyV, err := secret.Load(getenv, readFile, credentialKey, envKey)
	if err != nil {
		return c, fmt.Errorf("realm key: %w", err)
	}
	if keyV.Bytes == nil {
		return c, fmt.Errorf("realm key: none set (credential %s, %s_FILE or %s)", credentialKey, envKey, envKey)
	}
	k, err := hex.DecodeString(string(keyV.Bytes))
	if err != nil || len(k) != 32 {
		return c, fmt.Errorf("realm key (from the %s): %w: not 64 hex characters", keyV.Source, secret.ErrSource)
	}
	copy(c.staticSecret[:], k)
	clear(k)
	c.sources[credentialKey] = secret.Value{Source: keyV.Source, Ignored: keyV.Ignored}

	tenV, err := secret.Load(getenv, readFile, credentialTenants, envTenants)
	if err != nil {
		return c, fmt.Errorf("realm tenants: %w", err)
	}
	if tenV.Bytes == nil {
		return c, fmt.Errorf("realm tenants: none set (credential %s, %s_FILE or %s)", credentialTenants, envTenants, envTenants)
	}
	if c.keys, err = parseKeyring(tenV.Bytes); err != nil {
		return c, fmt.Errorf("realm tenants (from the %s): %w: %w", tenV.Source, secret.ErrSource, err)
	}
	c.sources[credentialTenants] = secret.Value{Source: tenV.Source, Ignored: tenV.Ignored}
	return c, nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newServer wires a realm over store; reg receives the metrics.
func newServer(c config, store realm.Store, reg prometheus.Registerer, logger *zap.Logger) (*server, error) {
	pub, err := recovery.NoisePublicKey(c.staticSecret)
	if err != nil {
		return nil, fmt.Errorf("realm key: %w", err)
	}
	m := newMetrics(reg)
	return &server{
		id: c.id, staticSecret: c.staticSecret, staticPublic: pub,
		realm:   &realm.Realm{Store: store, OnDestroy: m.destroyed.Inc},
		auth:    &authenticator{realmHex: c.id.String(), keys: c.keys, now: time.Now},
		metrics: m, logger: logger, rand: rand.Reader, now: time.Now,
		sessions: map[sessionKey]*session{},
	}, nil
}

// enrolment is the body of the key_ceremony entry that records this realm
// (POST /api/v1/admin/translog/ceremony, plus the admin's mfa_code). The
// fingerprint is sha256 of the realm's Noise public key (the log's field
// holds a hash); the statement carries the key itself and the realm id, so
// a reader matches them to the configuration a client pins.
func enrolment(c config) (map[string]any, error) {
	pub, err := recovery.NoisePublicKey(c.staticSecret)
	if err != nil {
		return nil, err
	}
	fp := sha256.Sum256(pub[:])
	return map[string]any{
		"ceremony":        "recovery_realm",
		"key_fingerprint": "sha256:" + hex.EncodeToString(fp[:]),
		"participants":    1,
		"statement": fmt.Sprintf("Recovery realm %s enrolled with Noise public key %s. "+
			"Juicebox protocol, attempt-limited by this realm. Not hardware-enforced until a second realm "+
			"under different control exists (F-19).", c.id, hex.EncodeToString(pub[:])),
	}, nil
}

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, "logger:", err)
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "enrolment" {
		c, err := loadConfig(os.Getenv, os.ReadFile)
		if err != nil {
			logger.Fatal("recovery realm: enrolment", zap.Error(err))
		}
		body, err := enrolment(c)
		if err != nil {
			logger.Fatal("recovery realm: enrolment", zap.Error(err))
		}
		_ = json.NewEncoder(os.Stdout).Encode(body)
		return
	}
	if err := run(logger); err != nil {
		logger.Fatal("recovery realm: refusing to start", zap.Error(err))
	}
}

func run(logger *zap.Logger) error {
	c, err := loadConfig(os.Getenv, os.ReadFile)
	if err != nil {
		return err
	}
	store, err := realm.OpenBoltStore(c.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	reg := prometheus.NewRegistry()
	srv, err := newServer(c, store, reg, logger)
	if err != nil {
		return err
	}
	tenants := make([]string, 0, len(c.keys))
	for t := range c.keys {
		tenants = append(tenants, t)
	}
	logger.Info("recovery realm",
		zap.String("realm_id", c.id.String()),
		zap.String("public_key", hex.EncodeToString(srv.staticPublic[:])), // public: clients pin it, the log records it
		zap.String("key_source", c.sources[credentialKey].Source),
		zap.String("tenants_source", c.sources[credentialTenants].Source),
		zap.Strings("tenants", tenants),
		zap.String("listen", c.listen), zap.Bool("tls", c.tlsCert != ""))
	for name, v := range c.sources {
		if len(v.Ignored) > 0 {
			logger.Warn("recovery realm: lower-precedence sources are also set and ignored — remove them",
				zap.String("secret", name), zap.Strings("ignored", v.Ignored))
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go srv.sweepSessions(ctx)

	httpSrv := &http.Server{Addr: c.listen, Handler: srv.routes(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	errc := make(chan error, 2)
	go func() {
		if c.tlsCert != "" {
			errc <- httpSrv.ListenAndServeTLS(c.tlsCert, c.tlsKy)
			return
		}
		errc <- httpSrv.ListenAndServe()
	}()
	var metricsSrv *http.Server
	if !strings.EqualFold(c.metricsListen, "off") {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		metricsSrv = &http.Server{Addr: c.metricsListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- metricsSrv.ListenAndServe() }()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdown)
	}
	return nil
}
