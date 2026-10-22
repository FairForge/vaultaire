// Package btcpay is the BTCPay Server provider (WP-PAY-4, docs/PAYMENTS.md
// § 3): the Greenfield API client, the webhook verifier, and — PR 3 — the
// payments.Provider and the invoice state table. Every protocol fact here
// was checked against a real BTCPay on regtest (the lab spike, Stage 1a
// Prompt 24; captures in testdata/, the facts in docs/PAYMENTS.md § 3.2–3.3).
//
// The store is watch-only (an xpub): no server holds a spending key, ever.
// The API key is scoped to one store (cancreateinvoice, canviewinvoices,
// canmodifyinvoices) and is never logged; neither is a full body.
package btcpay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxResponseBytes caps what is read of a Greenfield answer.
const maxResponseBytes = 1 << 20

// Client calls one store's Greenfield API.
type Client struct {
	baseURL, storeID, apiKey string
	http, fresh              *http.Client
}

// NewClient builds a client for storeID at baseURL (e.g.
// https://pay.stored.cloud) with a store-scoped API key.
func NewClient(baseURL, storeID, apiKey string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), storeID: storeID, apiKey: apiKey,
		http: newHTTPClient(false), fresh: newHTTPClient(true)}
}

// APIError is a non-2xx Greenfield answer: the status and BTCPay's error
// code/message (never the request's body).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("btcpay: HTTP %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("btcpay: HTTP %d", e.Status)
}

// IsUnknownInvoice reports an invoice fetch answered "no such invoice".
// BTCPay 2.4 has no 404 for it: an invoice it does not know has no store,
// so even an unrestricted key gets 403 missing-permission
// (btcpay.store.canviewinvoices) — the lab, docs/PAYMENTS.md § 3.2. A key
// that really lost the permission answers the same; the readiness check
// (GET …/invoices?take=1) catches that one and makes the provider Unready.
// A 404 is accepted too. A 401 (a wrong key) is not "unknown".
func IsUnknownInvoice(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusNotFound || ae.Status == http.StatusForbidden && ae.Code == "missing-permission"
}

// IsPermanent reports a Greenfield refusal no retry changes: a 4xx answer
// (a wrong key, a permission the key lacks, a 422 transition) — never a
// 408/429, a 5xx or no answer at all. The webhook answers it 200 + counted
// instead of a 5xx that would stall the store's webhook queue (RP30-4).
func IsPermanent(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusRequestTimeout && ae.Status != http.StatusTooManyRequests
}

// storePath is /api/v1/stores/{store}/… with every segment escaped.
func (c *Client) storePath(parts ...string) string {
	p := "/api/v1/stores/" + url.PathEscape(c.storeID)
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// do sends one request. A GET that fails to get any answer (a dial, a
// reset, a timeout — never an HTTP status) is retried once on a fresh
// connection; nothing else is retried (a POST could create a second
// invoice).
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("btcpay %s %s: encode: %w", method, path, err)
		}
	}
	resp, err := c.send(ctx, c.http, method, path, body)
	if err != nil && method == http.MethodGet && ctx.Err() == nil {
		resp, err = c.send(ctx, c.fresh, method, path, body)
	}
	if err != nil {
		return fmt.Errorf("btcpay %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("btcpay %s %s: read: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &APIError{Status: resp.StatusCode}
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil {
			ae.Code, ae.Message = e.Code, e.Message
		}
		return fmt.Errorf("btcpay %s %s: %w", method, path, ae)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("btcpay %s %s: decode: %w", method, path, err)
	}
	return nil
}

func (c *Client) send(ctx context.Context, hc *http.Client, method, path string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return hc.Do(req)
}
