package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// rawClient speaks WebDAV directly, for what the driver hides: a Depth 1
// PROPFIND timed on its own, exact resource names (no davName mapping) and
// deep paths for the limits probes, and the recursive DELETE of the run
// folder. Scheme and host are the parsed -url; paths are escaped segments.
type rawClient struct {
	base       url.URL
	user, pass string
	hc         *http.Client
}

func newRawClient(rawURL, user, pass string) (*rawClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse -url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("-url must be http(s)://host[:port][/path] without credentials")
	}
	return &rawClient{
		base: url.URL{Scheme: u.Scheme, Host: u.Host, Path: strings.TrimSuffix(u.Path, "/")},
		user: user,
		pass: pass,
		hc: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   512,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 10 * time.Minute,
			ForceAttemptHTTP2:     false,
		}},
	}, nil
}

// pathOf is the escaped path of segs below the URL's own path.
func (r *rawClient) pathOf(segs []string, dir bool) string {
	var b strings.Builder
	b.WriteString(r.base.Path)
	for _, s := range segs {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	if dir || b.Len() == 0 {
		b.WriteByte('/')
	}
	return b.String()
}

// decodedLen is the length of the decoded path (what a server's path-length
// limit counts).
func (r *rawClient) decodedLen(segs []string) int {
	n := len(r.base.Path)
	for _, s := range segs {
		n += 1 + len([]rune(s))
	}
	return n
}

func (r *rawClient) do(ctx context.Context, method string, segs []string, dir bool, body io.Reader, length int64, hdr map[string]string) (*http.Response, error) {
	u := r.base
	u.RawPath = r.pathOf(segs, dir)
	p, err := url.PathUnescape(u.RawPath)
	if err != nil {
		return nil, fmt.Errorf("unescape %s: %w", u.RawPath, err)
	}
	u.Path = p
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", method, err)
	}
	if req.URL.Host != r.base.Host {
		return nil, fmt.Errorf("request host %q is not the configured %q", req.URL.Host, r.base.Host)
	}
	req.ContentLength = length
	req.SetBasicAuth(r.user, r.pass)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, u.RawPath, err)
	}
	return resp, nil
}

// statusBody consumes a response: its status and a short body excerpt.
func statusBody(resp *http.Response) (int, string) {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func ok2xx(code int) bool { return code >= 200 && code < 300 }

// mkcolAll creates every folder of segs, top down (405 = exists).
func (r *rawClient) mkcolAll(ctx context.Context, segs []string) error {
	for i := 1; i <= len(segs); i++ {
		resp, err := r.do(ctx, "MKCOL", segs[:i], true, nil, 0, nil)
		if err != nil {
			return err
		}
		code, body := statusBody(resp)
		if !ok2xx(code) && code != http.StatusMethodNotAllowed {
			return fmt.Errorf("MKCOL %s: %d %s", r.pathOf(segs[:i], true), code, body)
		}
	}
	return nil
}

// put sends body to segs; it returns the status and body excerpt.
func (r *rawClient) put(ctx context.Context, segs []string, body []byte) (int, string, error) {
	var rd io.Reader
	if len(body) > 0 {
		rd = strings.NewReader(string(body))
	}
	resp, err := r.do(ctx, http.MethodPut, segs, false, rd, int64(len(body)), map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return 0, "", err
	}
	code, msg := statusBody(resp)
	return code, msg, nil
}

// get reads segs whole (small objects only).
func (r *rawClient) get(ctx context.Context, segs []string) (int, []byte, error) {
	resp, err := r.do(ctx, http.MethodGet, segs, false, nil, 0, nil)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		code, _ := statusBody(resp)
		return code, nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return resp.StatusCode, b, err
}

// remove DELETEs segs (a folder recursively); a 404 is fine.
func (r *rawClient) remove(ctx context.Context, segs []string, dir bool) error {
	resp, err := r.do(ctx, http.MethodDelete, segs, dir, nil, 0, nil)
	if err != nil {
		return err
	}
	code, body := statusBody(resp)
	if ok2xx(code) || code == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("DELETE %s: %d %s", r.pathOf(segs, dir), code, body)
}

// exists is a Depth 0 PROPFIND: 207 = there, 404 = not.
func (r *rawClient) exists(ctx context.Context, segs []string, dir bool) (bool, error) {
	resp, err := r.do(ctx, "PROPFIND", segs, dir, strings.NewReader(propfindBody), int64(len(propfindBody)),
		map[string]string{"Depth": "0", "Content-Type": "application/xml; charset=utf-8"})
	if err != nil {
		return false, err
	}
	code, body := statusBody(resp)
	switch code {
	case http.StatusMultiStatus:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("PROPFIND %s: %d %s", r.pathOf(segs, dir), code, body)
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>` +
	`<D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/><D:getcontentlength/></D:prop></D:propfind>`

// listDepth1 is one Depth 1 PROPFIND of a folder, streamed: it returns the
// decoded last path segment of every member (the folder itself excluded),
// and how long the request took to answer in full.
func (r *rawClient) listDepth1(ctx context.Context, segs []string) ([]string, time.Duration, error) {
	start := time.Now()
	resp, err := r.do(ctx, "PROPFIND", segs, true, strings.NewReader(propfindBody), int64(len(propfindBody)),
		map[string]string{"Depth": "1", "Content-Type": "application/xml; charset=utf-8"})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMultiStatus {
		code, body := statusBody(resp)
		return nil, 0, fmt.Errorf("PROPFIND %s: %d %s", r.pathOf(segs, true), code, body)
	}
	self := strings.TrimSuffix(r.pathOf(segs, true), "/")
	selfDecoded, _ := url.PathUnescape(self)
	var names []string
	dec := xml.NewDecoder(resp.Body)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("parse multistatus: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Space != "DAV:" || se.Name.Local != "href" {
			continue
		}
		var href string
		if err := dec.DecodeElement(&href, &se); err != nil {
			return nil, 0, fmt.Errorf("parse href: %w", err)
		}
		hu, err := url.Parse(strings.TrimSpace(href))
		if err != nil {
			return nil, 0, fmt.Errorf("parse href %q: %w", href, err)
		}
		p := strings.TrimSuffix(hu.Path, "/")
		if p == selfDecoded || p == self {
			continue
		}
		names = append(names, p[strings.LastIndex(p, "/")+1:])
	}
	return names, time.Since(start), nil
}
