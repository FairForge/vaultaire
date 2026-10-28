// Package node is the data node (WP-EDGE-3, Stage 1a Prompt 47; plan 40.10:
// "unmetered data nodes running this binary stateless"): cmd/vaultaire with
// VAULTAIRE_ROLE=node. It opens NO database, NO engine, runs NO jobs, holds
// NO key of the hierarchy (contract line 4) — only its own token for the
// control plane's edge calls. It serves the data plane's three requests
// itself through internal/dataplane (PutObject and UploadPart: the move or
// the codec; GetObject: the presigned read), and hands every other SIGNED S3
// request — HEAD, DELETE, a listing, Create/Complete/Abort of a multipart
// upload, anything the control plane passes through — to the origin
// untouched (the Host header kept: the client's signature verifies there).
// The web and API paths of the origin do not exist here (404).
//
// A node is NOT the attested data plane of contract line 10: it claims
// nothing about the code it runs to anyone but its operator. A
// tenant-scoped (BYO) node admits one tenant and nothing it reports is
// trusted for integrity or metering — the control plane's rule, not the
// node's.
package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/dataplane"
	"github.com/FairForge/vaultaire/internal/recovery/secret"
)

// RoleEnv names the role; RoleNode is the data node's.
const (
	RoleEnv  = "VAULTAIRE_ROLE"
	RoleNode = "node"
)

// tokenCredential is the systemd credential's name of the node's token.
const tokenCredential = "node_token"

// tokenMinLen: a token shorter than this is a typo, not a secret.
const tokenMinLen = 16

// heartbeatEvery is how often the node tells the control plane it is up
// (the control plane marks a node unseen for 90 s down).
const heartbeatEvery = 30 * time.Second

// ignoredInRole are settings of the origin a node never reads: named in a
// Warn so an operator does not believe they matter.
var ignoredInRole = []string{"STORAGE_MODE", "DB_HOST", "DB_NAME", "DB_USER", "DB_PASSWORD", "DATABASE_URL",
	"ENCRYPTION_MASTER_KEY", "ENCRYPTION_MASTER_KEY_FILE", "EDGE_SERVICE_TOKEN", "JWT_SECRET", "IDRIVE_ACCESS_KEY", "LYVE_ACCESS_KEY"}

// Config is what the node needs.
type Config struct {
	// ControlPlaneURL is the control plane's origin (NODE_CONTROL_PLANE_URL):
	// the edge calls go there.
	ControlPlaneURL string
	// OriginURL is where a pass-through goes (NODE_ORIGIN_URL; default the
	// control plane's URL — the origin answers the S3 API there too).
	OriginURL string
	// Token is the node's token (the registry minted it once): credential
	// node_token, then NODE_TOKEN_FILE, then NODE_TOKEN.
	Token []byte
	// PublicHost is the node's own hostname (NODE_PUBLIC_HOST), for the logs.
	PublicHost string
	// Port is the listen port (PORT, default 8100).
	Port int
}

// ConfigFromEnv reads the node's configuration. err = a setting missing or
// unusable (the caller refuses to boot); ignored = origin settings set here
// that the role never reads.
func ConfigFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, []string, error) {
	var cfg Config
	cfg.ControlPlaneURL = strings.TrimRight(strings.TrimSpace(getenv("NODE_CONTROL_PLANE_URL")), "/")
	if err := checkURL("NODE_CONTROL_PLANE_URL", cfg.ControlPlaneURL); err != nil {
		return cfg, nil, err
	}
	cfg.OriginURL = strings.TrimRight(strings.TrimSpace(getenv("NODE_ORIGIN_URL")), "/")
	if cfg.OriginURL == "" {
		cfg.OriginURL = cfg.ControlPlaneURL
	} else if err := checkURL("NODE_ORIGIN_URL", cfg.OriginURL); err != nil {
		return cfg, nil, err
	}
	v, err := secret.Load(getenv, readFile, tokenCredential, "NODE_TOKEN")
	if err != nil {
		return cfg, nil, err
	}
	if len(v.Bytes) < tokenMinLen {
		return cfg, nil, fmt.Errorf("NODE_TOKEN: not set or shorter than %d bytes — the token the registry minted (credential %s, NODE_TOKEN_FILE or NODE_TOKEN)", tokenMinLen, tokenCredential)
	}
	cfg.Token = v.Bytes
	cfg.PublicHost = strings.TrimSpace(getenv("NODE_PUBLIC_HOST"))
	cfg.Port = 8100
	if p := strings.TrimSpace(getenv("PORT")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return cfg, nil, fmt.Errorf("PORT %q: not a port", p)
		}
		cfg.Port = n
	}
	var ignored []string
	for _, k := range ignoredInRole {
		if getenv(k) != "" {
			ignored = append(ignored, k)
		}
	}
	return cfg, ignored, nil
}

func checkURL(name, v string) error {
	if v == "" {
		return fmt.Errorf("%s: not set — the control plane's origin (https://…)", name)
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return fmt.Errorf("%s: %q is not an origin (https://host[:port])", name, v)
	}
	return nil
}

// The node's own metrics (the data-plane counters of the control plane are
// the control plane's: they count this node's admits under its host label).
var (
	nodeRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vaultaire_node_requests_total",
		Help: "Requests a data node took, by op (put, part, get, proxy) and outcome (edge, pass_through, refused, error).",
	}, []string{"op", "outcome"})
	nodeInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vaultaire_node_in_flight",
		Help: "Requests the data node is serving now.",
	})
)

// Node is a built data node.
type Node struct {
	cfg      Config
	cp       *dataplane.HTTPControlPlane
	host     *dataplane.Host
	proxy    *httputil.ReverseProxy
	router   chi.Router
	reg      *prometheus.Registry
	logger   *zap.Logger
	version  string
	inFlight atomic.Int64
}

// Build makes the node from the environment.
func Build(getenv func(string) string, readFile func(string) ([]byte, error), version string, logger *zap.Logger) (*Node, error) {
	cfg, ignored, err := ConfigFromEnv(getenv, readFile)
	if err != nil {
		return nil, err
	}
	if len(ignored) > 0 {
		logger.Warn("data node: these settings are the origin's and are ignored in this role", zap.Strings("ignored", ignored))
	}
	origin, _ := url.Parse(cfg.OriginURL)
	n := &Node{cfg: cfg, logger: logger, version: version, reg: prometheus.NewRegistry()}
	n.cp = dataplane.NewHTTPControlPlane(cfg.ControlPlaneURL, cfg.Token, nil)
	n.host = &dataplane.Host{CP: n.cp, Client: &http.Client{}, ClientIP: peerIP, Logger: logger}
	// The origin answers what the node does not take: the request as the
	// client sent it — its Host kept, so its signature verifies there.
	n.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(origin)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Warn("data node: the origin did not answer a pass-through", zap.Error(err))
			writeS3Error(w, http.StatusServiceUnavailable, "SlowDown", "Please reduce your request rate: the service could not be reached. Retry.", r.URL.Path)
		},
	}
	n.reg.MustRegister(nodeRequests, nodeInFlight, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	n.router = n.routes()
	return n, nil
}

// Router is the node's HTTP surface.
func (n *Node) Router() chi.Router { return n.router }

// SetBackendTransport replaces the transport the node reaches the
// backends' links with (the tests' fake backend).
func (n *Node) SetBackendTransport(rt http.RoundTripper) { n.host.Client = &http.Client{Transport: rt} }

// routes: PutObject/UploadPart and GetObject on /{bucket}/*, /health,
// /metrics; every other request is a 404, or — a SIGNED S3 request — the
// origin's (handed on untouched).
func (n *Node) routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/health", n.handleHealth)
	r.Get("/metrics", promhttp.HandlerFor(n.reg, promhttp.HandlerOpts{}).ServeHTTP)
	r.Put("/{bucket}/*", n.handlePut)
	r.Get("/{bucket}/*", n.handleGet)
	r.NotFound(n.handleOther)
	r.MethodNotAllowed(n.handleOther)
	return r
}

// reservedPrefix are the origin's web and API paths: never served or
// forwarded by a node, signed or not.
func reservedPath(p string) bool {
	for _, pre := range []string{"/api/", "/dashboard", "/translog/", "/admin", "/auth/", "/webhook/", "/cdn/", "/legal/", "/docs"} {
		if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// signedS3 is the origin's predicate (internal/api signedS3Request): an
// AWS-scheme Authorization or a presigned query.
func signedS3(r *http.Request) bool {
	if auth.IsAWSAuthorization(r.Header.Get("Authorization")) {
		return true
	}
	q := r.URL.Query()
	return q.Get("X-Amz-Algorithm") != "" || q.Has("X-Amz-Signature") || q.Has("X-Amz-Credential") || (q.Has("AWSAccessKeyId") && q.Has("Signature"))
}

func (n *Node) handleOther(w http.ResponseWriter, r *http.Request) {
	if reservedPath(r.URL.Path) || !signedS3(r) {
		http.NotFound(w, r)
		return
	}
	nodeRequests.WithLabelValues("proxy", "pass_through").Inc()
	n.proxy.ServeHTTP(w, r)
}

func (n *Node) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"status":"healthy","role":"node","version":%q}`+"\n", n.version)
}

func (n *Node) begin() func() {
	n.inFlight.Add(1)
	nodeInFlight.Inc()
	return func() {
		n.inFlight.Add(-1)
		nodeInFlight.Dec()
	}
}

func (n *Node) handlePut(w http.ResponseWriter, r *http.Request) {
	defer n.begin()()
	op := dataplane.WriteOp(r)
	if reservedPath(r.URL.Path) || !signedS3(r) || (len(r.URL.Query()) > 0 && op != dataplane.OpPart && !onlyXID(r)) {
		n.handleOther(w, r)
		return
	}
	res, err := n.host.Write(r.Context(), r, r.Body, r.ContentLength)
	switch {
	case err != nil:
		// The control plane did not answer the admit: the body is untouched,
		// the origin takes the request (the Worker's rule).
		n.logger.Warn("data node: admit failed — the origin takes the request", zap.Error(err))
		nodeRequests.WithLabelValues(op, "error").Inc()
		n.proxy.ServeHTTP(w, r)
	case res.PassThrough:
		nodeRequests.WithLabelValues(op, "pass_through").Inc()
		n.proxy.ServeHTTP(w, r)
	default:
		outcome := "edge"
		if res.Admit.Refusal != nil {
			outcome = "refused"
		}
		nodeRequests.WithLabelValues(op, outcome).Inc()
		dataplane.WriteRelay(w, res.Relay)
	}
}

func (n *Node) handleGet(w http.ResponseWriter, r *http.Request) {
	defer n.begin()()
	if reservedPath(r.URL.Path) || !signedS3(r) || r.URL.Query().Has("X-Amz-Signature") || (len(r.URL.Query()) > 0 && !onlyXID(r)) {
		n.handleOther(w, r)
		return
	}
	res, err := n.host.Read(r.Context(), r)
	switch {
	case err != nil:
		n.logger.Warn("data node: admit failed — the origin takes the request", zap.Error(err))
		nodeRequests.WithLabelValues(dataplane.OpGet, "error").Inc()
		n.proxy.ServeHTTP(w, r)
	case res.PassThrough:
		nodeRequests.WithLabelValues(dataplane.OpGet, "pass_through").Inc()
		n.proxy.ServeHTTP(w, r)
	case res.Relay != nil:
		nodeRequests.WithLabelValues(dataplane.OpGet, "refused").Inc()
		dataplane.WriteRelay(w, res.Relay)
	default:
		nodeRequests.WithLabelValues(dataplane.OpGet, "edge").Inc()
		defer func() { _ = res.Body.Close() }()
		for k, v := range res.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(res.Status)
		_, _ = io.Copy(w, res.Body)
	}
}

// onlyXID: aws-sdk-go-v2 names the operation in the query (x-id) — not a
// sub-resource.
func onlyXID(r *http.Request) bool {
	for k := range r.URL.Query() {
		if k != "x-id" {
			return false
		}
	}
	return true
}

// peerIP is the node's client: its direct peer (a node is reached directly,
// not behind the origin's HAProxy).
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeS3Error(w http.ResponseWriter, status int, code, msg, path string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Retry-After", "30")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n<Error><Code>%s</Code><Message>%s</Message><Resource>%s</Resource></Error>", code, msg, path)
}

// heartbeat tells the control plane the node is up, every 30 s until ctx
// ends.
func (n *Node) heartbeat(ctx context.Context) {
	beat := func() {
		hb := dataplane.Heartbeat{Version: n.version, BuildSHA: n.version, Load: float64(runtime.NumGoroutine()), InFlight: n.inFlight.Load()}
		if err := n.cp.Heartbeat(ctx, hb); err != nil {
			n.logger.Warn("data node: heartbeat not taken", zap.Error(err))
		}
	}
	beat()
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beat()
		}
	}
}

// Serve runs the node until ctx ends, then drains for up to 30 s.
func (n *Node) Serve(ctx context.Context) error {
	srv := &http.Server{Addr: fmt.Sprintf(":%d", n.cfg.Port), Handler: n.router, ReadHeaderTimeout: 30 * time.Second}
	hctx, stop := context.WithCancel(ctx)
	defer stop()
	go n.heartbeat(hctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	n.logger.Info("data node: serving", zap.Int("port", n.cfg.Port), zap.String("public_host", n.cfg.PublicHost),
		zap.String("control_plane", n.cfg.ControlPlaneURL), zap.String("version", n.version))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Main is the node role's main: build from the environment, serve until
// SIGINT/SIGTERM. A configuration that does not build refuses the boot.
func Main(version string) {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()
	n, err := Build(os.Getenv, os.ReadFile, version, logger)
	if err != nil {
		logger.Fatal("data node: refusing to start", zap.Error(err))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := n.Serve(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal("data node: stopped", zap.Error(err))
	}
	logger.Info("data node: stopped")
}
