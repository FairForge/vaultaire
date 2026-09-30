package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/engine"
)

// pixeldrainLeg adapts Pixeldrain's REST API (no S3 surface) to
// engine.Driver for the bench only: PUT /api/file/{name} returns an id, GET
// and DELETE address the id. The container/artifact → id map lives in memory
// for the run, which is all a bench needs (a real driver would persist it).
// Transport settings mirror cmd/pixeldrain-bench (HTTP/1.1, keep-alive).
type pixeldrainLeg struct {
	apiKey string
	http   *http.Client
	mu     sync.Mutex
	ids    map[string]string
}

const pixeldrainAPI = "https://pixeldrain.com/api"

func newPixeldrainLeg(apiKey string) *pixeldrainLeg {
	tr := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ReadBufferSize:      1 << 20,
		WriteBufferSize:     1 << 20,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DisableCompression:  true,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &pixeldrainLeg{
		apiKey: apiKey,
		http:   &http.Client{Transport: tr, Timeout: 30 * time.Minute},
		ids:    map[string]string{},
	}
}

func (p *pixeldrainLeg) Name() string { return "pixeldrain" }

func (p *pixeldrainLeg) key(container, artifact string) string { return container + "/" + artifact }

func (p *pixeldrainLeg) lookup(container, artifact string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.ids[p.key(container, artifact)]
	return id, ok
}

func (p *pixeldrainLeg) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pixeldrainAPI+"/user", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth("", p.apiKey)
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pixeldrain /user status %d", resp.StatusCode)
	}
	return nil
}

func (p *pixeldrainLeg) Put(ctx context.Context, container, artifact string, data io.Reader, opts ...engine.PutOption) error {
	size := engine.ApplyPutOptions(opts...).ContentLength
	if size == 0 {
		size = -1
	}
	name := strings.ReplaceAll(p.key(container, artifact), "/", "_")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, pixeldrainAPI+"/file/"+name, data)
	if err != nil {
		return err
	}
	req.SetBasicAuth("", p.apiKey)
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("pixeldrain put: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pixeldrain put status %d: %s", resp.StatusCode, trunc(fmt.Errorf("%s", body), 120))
	}
	var info struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &info); err != nil || info.ID == "" {
		return fmt.Errorf("pixeldrain put: no id in response")
	}
	p.mu.Lock()
	prev, had := p.ids[p.key(container, artifact)]
	if !had {
		p.ids[p.key(container, artifact)] = info.ID
	}
	p.mu.Unlock()
	if had && prev != info.ID {
		// A hedged duplicate lost the race: keep the winner, drop this copy.
		_ = p.deleteID(context.Background(), info.ID)
	}
	return nil
}

func (p *pixeldrainLeg) deleteID(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, pixeldrainAPI+"/file/"+id, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth("", p.apiKey)
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil
}

func (p *pixeldrainLeg) Get(ctx context.Context, container, artifact string) (io.ReadCloser, error) {
	id, ok := p.lookup(container, artifact)
	if !ok {
		return nil, fmt.Errorf("pixeldrain get: NoSuchKey %s", p.key(container, artifact))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pixeldrainAPI+"/file/"+id, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("", p.apiKey)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pixeldrain get: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("pixeldrain get status %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func (p *pixeldrainLeg) Delete(ctx context.Context, container, artifact string) error {
	id, ok := p.lookup(container, artifact)
	if !ok {
		return nil
	}
	if err := p.deleteID(ctx, id); err != nil {
		return fmt.Errorf("pixeldrain delete: %w", err)
	}
	p.mu.Lock()
	delete(p.ids, p.key(container, artifact))
	p.mu.Unlock()
	return nil
}

func (p *pixeldrainLeg) Exists(ctx context.Context, container, artifact string) (bool, error) {
	_, ok := p.lookup(container, artifact)
	return ok, nil
}

func (p *pixeldrainLeg) List(ctx context.Context, container, prefix string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for k := range p.ids {
		if strings.HasPrefix(k, container+"/"+prefix) {
			out = append(out, strings.TrimPrefix(k, container+"/"))
		}
	}
	sort.Strings(out)
	return out, nil
}
