package api

// The edge service token (WP-EDGE-1 Part A, Stage 1a Prompt 44): what the
// Worker on s3.stored.cloud presents on every admit and commit call. Read
// like the master key (crypto.LoadMasterKeys' precedence, through
// internal/recovery/secret): the systemd credential
// $CREDENTIALS_DIRECTORY/edge_service_token, then EDGE_SERVICE_TOKEN_FILE,
// then the variable. A source that is set but unusable is a fatal boot;
// not set = the edge routes are not mounted and the edge_ingest flag does
// nothing (one Info line). The value is never logged.
//
// EDGE_TRUST_ANY_PEER=true (labs only, a Warn line) lifts the rule that
// the call must come from a Cloudflare peer (clientip.PeerIsCloudflare):
// wrangler dev talks to a local origin from 127.0.0.1.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/clientip"
	"github.com/FairForge/vaultaire/internal/dataplane"
	"github.com/FairForge/vaultaire/internal/recovery/secret"
)

// edgeTokenCredential is the systemd credential's name.
const edgeTokenCredential = "edge_service_token"

// edgeTokenHeader carries the token on the Worker's calls.
const edgeTokenHeader = "X-Vaultaire-Edge-Token"

// edgeTokenMinLen: a token shorter than this is a typo, not a secret.
const edgeTokenMinLen = 16

// edgeConfig is what the boot resolved.
type edgeConfig struct {
	token        []byte
	trustAnyPeer bool
	// minBytes: a write whose decoded length is below it is the origin's
	// (EDGE_MIN_BYTES, default 4 MiB; 0 = off): two round trips fewer for
	// a small body, and its bytes do not matter for SLC's bandwidth.
	minBytes int64
	// codecHitWait bounds how long a codec commit waits for a hit whose
	// row is another writer's YOUNG claim to flip ready (RP68-2; 0 =
	// edgeCodecHitWaitDefault). Tests lower it.
	codecHitWait time.Duration
}

// edgeMinBytesDefault is EDGE_MIN_BYTES' default.
const edgeMinBytesDefault = 4 << 20

// edgeMinBytesFromEnv reads EDGE_MIN_BYTES: a size (`4MiB`, `262144`) or
// `off`; a rejected value is logged at Warn and the default kept.
func edgeMinBytesFromEnv(getenv func(string) string, logger *zap.Logger) int64 {
	raw := strings.TrimSpace(getenv("EDGE_MIN_BYTES"))
	switch strings.ToLower(raw) {
	case "":
		return edgeMinBytesDefault
	case "off", "0":
		return 0
	}
	if n, err := parseByteSize(raw); err == nil && n > 0 {
		return n
	}
	logger.Warn("EDGE_MIN_BYTES rejected; default kept", zap.String("value", raw))
	return edgeMinBytesDefault
}

// loadEdgeConfig resolves the token and the lab switch. err = a source set
// but unusable (the caller refuses to boot); a zero token = not configured.
func loadEdgeConfig(getenv func(string) string, readFile func(string) ([]byte, error)) (edgeConfig, string, []string, error) {
	v, err := secret.Load(getenv, readFile, edgeTokenCredential, "EDGE_SERVICE_TOKEN")
	if err != nil {
		return edgeConfig{}, "", nil, err
	}
	cfg := edgeConfig{minBytes: edgeMinBytesFromEnv(getenv, zap.NewNop())}
	if len(v.Bytes) > 0 {
		if len(v.Bytes) < edgeTokenMinLen {
			return edgeConfig{}, "", nil, errEdgeTokenTooShort
		}
		cfg.token = v.Bytes
	}
	switch strings.ToLower(strings.TrimSpace(getenv("EDGE_TRUST_ANY_PEER"))) {
	case "true", "1":
		cfg.trustAnyPeer = true
	}
	return cfg, v.Source, v.Ignored, nil
}

type edgeTokenErr string

func (e edgeTokenErr) Error() string { return string(e) }

const errEdgeTokenTooShort = edgeTokenErr("EDGE_SERVICE_TOKEN: shorter than 16 bytes — not a token (openssl rand -hex 32)")

// edgeConfigOrFatal loads the token and stops the process when a source is
// set but unusable (the master key's rule). One Info line names the
// source, never the value.
func edgeConfigOrFatal(logger *zap.Logger, getenv func(string) string, readFile func(string) ([]byte, error)) edgeConfig {
	cfg, source, ignored, err := loadEdgeConfig(getenv, readFile)
	if err != nil {
		logger.Fatal("edge service token: refusing to start", zap.Error(err))
		return edgeConfig{}
	}
	if len(cfg.token) == 0 {
		logger.Info("edge service token: not set — /api/v1/edge/admit and /commit are not mounted and the edge_ingest flag does nothing")
		return cfg
	}
	cfg.minBytes = edgeMinBytesFromEnv(getenv, logger)
	logger.Info("edge service token: "+source, zap.String("source", source), zap.Int64("min_bytes", cfg.minBytes))
	if len(ignored) > 0 {
		logger.Warn("edge service token: lower-precedence sources are also set and ignored — remove them", zap.Strings("ignored", ignored))
	}
	if cfg.trustAnyPeer {
		logger.Warn("EDGE_TRUST_ANY_PEER is set: the edge routes accept the service token from ANY peer, not only a Cloudflare edge (labs only)")
	}
	return cfg
}

// edgeCaller is the auth of an edge call (admit, index, commit, heartbeat):
// the Worker — the service token, compared in constant time, from a
// Cloudflare peer (unless the lab switch is on) — or a registered data node
// (WP-EDGE-3) — its token, looked up by sha256 in data_plane_hosts (the
// 15 s cache), from the IP its row names (clientip.TrustedPeer, the direct
// peer HAProxy saw; never a header the node writes). ok false = the route
// does not exist to this caller (the handlers' 404 — never a 401 that says
// it does). health is the host's registry health ("up" for the Worker).
func (s *Server) edgeCaller(r *http.Request) (h dataplane.HostInfo, health string, ok bool) {
	if len(s.edge.token) == 0 {
		return h, "", false
	}
	got := r.Header.Get(edgeTokenHeader)
	if got == "" {
		return h, "", false
	}
	if subtle.ConstantTimeCompare([]byte(got), s.edge.token) == 1 {
		if !s.edge.trustAnyPeer && !clientip.PeerIsCloudflare(r) {
			return h, "", false
		}
		return dataplane.HostInfo{Name: edgeHost, Kind: dataplane.KindEdge}, "up", true
	}
	if s.db == nil {
		return h, "", false
	}
	sum := sha256.Sum256([]byte(got))
	row, found, err := s.dataPlaneHosts().byTokenHash(r.Context(), hex.EncodeToString(sum[:]))
	if err != nil {
		s.log().Warn("edge call: the host registry could not be read", zap.Error(err))
		return h, "", false
	}
	if !found || row.Kind != dataplane.KindNode || row.Address == "" {
		return h, "", false
	}
	peer := net.ParseIP(clientip.TrustedPeer(r))
	if peer == nil || !peer.Equal(net.ParseIP(row.Address)) {
		return h, "", false
	}
	health = row.Health
	if health == "up" && (row.LastSeen == nil || time.Since(*row.LastSeen) > dataPlaneHostStaleAfter) {
		// Up in the registry but no heartbeat for 90 s: down at the gate
		// now, whatever the next sweep writes.
		health = "down"
	}
	return dataplane.HostInfo{Name: row.Host, Kind: row.Kind, TenantID: row.TenantID}, health, true
}
