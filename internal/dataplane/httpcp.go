package dataplane

// The node's control plane: the three edge calls over HTTPS (WP-EDGE-3).
// Each call carries the node's token in X-Vaultaire-Edge-Token (the
// registry's token_hash is its sha256; the control plane accepts it only
// from the node's registered address). Admit and Index have 10 s each; the
// commit runs on its own clock — detached from the client's request, 60 s
// — because a client gone is not a cancel (RP64-1's rule).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TokenHeader carries a host's token on its control-plane calls.
const TokenHeader = "X-Vaultaire-Edge-Token"

const (
	callTimeout = 10 * time.Second
	callBodyMax = 4 << 20
)

// HTTPControlPlane implements ControlPlane over HTTPS.
type HTTPControlPlane struct {
	base   string
	token  []byte
	client *http.Client
}

// NewHTTPControlPlane asks the control plane at base (its origin URL)
// with token; client nil = a default client (each call has its own
// deadline).
func NewHTTPControlPlane(base string, token []byte, client *http.Client) *HTTPControlPlane {
	if client == nil {
		client = &http.Client{}
	}
	return &HTTPControlPlane{base: strings.TrimRight(base, "/"), token: token, client: client}
}

// Admit is POST /api/v1/edge/admit.
func (c *HTTPControlPlane) Admit(ctx context.Context, req AdmitRequest) (AdmitResponse, error) {
	var resp AdmitResponse
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return resp, c.call(cctx, "/api/v1/edge/admit", req, &resp)
}

// Index is POST /api/v1/edge/index.
func (c *HTTPControlPlane) Index(ctx context.Context, req IndexRequest) (IndexResponse, error) {
	var resp IndexResponse
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return resp, c.call(cctx, "/api/v1/edge/index", req, &resp)
}

// Commit is POST /api/v1/edge/commit, on its own clock.
func (c *HTTPControlPlane) Commit(ctx context.Context, req CommitRequest) (CommitResponse, error) {
	var resp CommitResponse
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitBound)
	defer cancel()
	return resp, c.call(cctx, "/api/v1/edge/commit", req, &resp)
}

// Mirror is POST /api/v1/edge/mirror (RP78-2).
func (c *HTTPControlPlane) Mirror(ctx context.Context, req MirrorSettle) (MirrorSettleResponse, error) {
	var resp MirrorSettleResponse
	return resp, c.call(ctx, "/api/v1/edge/mirror", req, &resp)
}

// Heartbeat is POST /api/v1/edge/heartbeat: the node says it is alive.
func (c *HTTPControlPlane) Heartbeat(ctx context.Context, hb Heartbeat) error {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var ack map[string]any
	return c.call(cctx, "/api/v1/edge/heartbeat", hb, &ack)
}

// Heartbeat is what a node reports every 30 s.
type Heartbeat struct {
	Version  string  `json:"version"`
	BuildSHA string  `json:"build_sha"`
	Load     float64 `json:"load"`
	InFlight int64   `json:"in_flight"`
}

func (c *HTTPControlPlane) call(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TokenHeader, string(c.token))
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, callBodyMax))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return &StatusError{Status: resp.StatusCode, Msg: e.Error}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: the answer is not JSON: %w", path, err)
	}
	return nil
}
