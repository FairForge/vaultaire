package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// graph is the tool's Microsoft Graph client: app-only, at most maxInFlight
// requests at once, Retry-After honoured. Its own (not the driver's): the
// migration needs the copy action, the monitor and the per-drive walks the
// driver never makes.
type graph struct {
	base    string // https://graph.microsoft.com/v1.0
	token   func(ctx context.Context) (string, error)
	http    *http.Client // Graph calls
	monitor *http.Client // the copy monitor: pre-authenticated, never redirected
	sem     chan struct{}
	wait    func(ctx context.Context, d time.Duration) error

	requests  atomic.Int64
	throttled atomic.Int64
}

const (
	maxInFlight   = 4
	maxAttempts   = 6
	maxRetryAfter = 2 * time.Minute
)

func newGraph(base string, token func(ctx context.Context) (string, error)) *graph {
	return &graph{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 5 * time.Minute},
		monitor: &http.Client{Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		sem:  make(chan struct{}, maxInFlight),
		wait: sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// graphError is a Graph answer >= 400 after the retries.
type graphError struct {
	Status int
	Code   string
	Body   string
	Method string
	URL    string
}

func (e *graphError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d %s %s", e.Method, e.URL, e.Status, e.Code, e.Body)
}

func isStatus(err error, status int) bool {
	var ge *graphError
	return errors.As(err, &ge) && ge.Status == status
}

// do sends one Graph request (path relative to base, or an absolute URL)
// and returns the response of a 2xx/3xx; 429, 503, other 5xx and transport
// errors are retried with Retry-After or backoff; any other >= 400 is a
// *graphError. The caller closes the body.
func (g *graph) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*http.Response, error) {
	u := path
	if !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
		u = g.base + path
	}
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-g.sem }()

	var lastErr error
	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		tok, err := g.token(ctx)
		if err != nil {
			return nil, fmt.Errorf("token: %w", err)
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, fmt.Errorf("request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("User-Agent", "ISV|FairForge|permafrost-migrate-drive/1.0")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if body != nil && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
		g.requests.Add(1)
		resp, err := g.http.Do(req)
		wait := backoff
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("%s %s: %w", method, redact(u), err)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
				g.throttled.Add(1)
			}
			if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 {
				wait = time.Duration(ra) * time.Second
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			lastErr = &graphError{Status: resp.StatusCode, Body: string(b), Method: method, URL: redact(u)}
		case resp.StatusCode >= 400:
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			var ge struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(b, &ge)
			return nil, &graphError{Status: resp.StatusCode, Code: ge.Error.Code, Body: ge.Error.Message, Method: method, URL: redact(u)}
		default:
			return resp, nil
		}
		if attempt == maxAttempts {
			break
		}
		if wait > maxRetryAfter {
			wait = maxRetryAfter
		}
		if err := g.wait(ctx, wait); err != nil {
			return nil, err
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	return nil, lastErr
}

// redact keeps a URL's path, never its query (a pre-authenticated URL's
// token lives there).
func redact(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "<url>"
	}
	if p.Host != "graph.microsoft.com" {
		return p.Scheme + "://" + p.Host + "/…"
	}
	return p.Path
}

func (g *graph) getJSON(ctx context.Context, path string, out any) error {
	resp, err := g.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", redact(path), err)
	}
	return nil
}

// drive is one of the user's drives.
type drive struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DriveType string `json:"driveType"`
	Quota     struct {
		Total     int64  `json:"total"`
		Used      int64  `json:"used"`
		Remaining int64  `json:"remaining"`
		Deleted   int64  `json:"deleted"`
		State     string `json:"state"`
	} `json:"quota"`
}

func (g *graph) drives(ctx context.Context, upn string) ([]drive, error) {
	var out struct {
		Value []drive `json:"value"`
	}
	err := g.getJSON(ctx, "/users/"+url.PathEscape(upn)+"/drives?$select=id,name,driveType,quota", &out)
	return out.Value, err
}

// item is a driveItem with the facets the walk needs.
type item struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	File *struct {
		Hashes struct {
			QuickXorHash string `json:"quickXorHash"`
		} `json:"hashes"`
	} `json:"file"`
}

const itemSelect = "$select=id,name,size,folder,file"

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// itemAt is the item at path under the drive's root; nil when absent.
func (g *graph) itemAt(ctx context.Context, driveID, path string) (*item, error) {
	var it item
	err := g.getJSON(ctx, "/drives/"+driveID+"/root:/"+escapePath(path)+"?"+itemSelect, &it)
	if isStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func (g *graph) root(ctx context.Context, driveID string) (*item, error) {
	var it item
	if err := g.getJSON(ctx, "/drives/"+driveID+"/root?$select=id,name", &it); err != nil {
		return nil, err
	}
	return &it, nil
}

// children is every child of the folder, all pages.
func (g *graph) children(ctx context.Context, driveID, itemID string) ([]item, error) {
	var all []item
	next := "/drives/" + driveID + "/items/" + itemID + "/children?" + itemSelect + "&$top=999"
	for next != "" {
		var page struct {
			Value    []item `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := g.getJSON(ctx, next, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Value...)
		next = page.NextLink
	}
	return all, nil
}

// errExists is a copy whose destination name is taken.
var errExists = errors.New("the destination already exists")

// copyItem starts Graph's copy of the item into the destination folder
// (another drive of the tenant allowed), conflict = fail; the monitor URL.
func (g *graph) copyItem(ctx context.Context, srcDrive, itemID, dstDrive, dstParentID, name string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"parentReference": map[string]string{"driveId": dstDrive, "id": dstParentID},
		"name":            name,
	})
	resp, err := g.do(ctx, http.MethodPost,
		"/drives/"+srcDrive+"/items/"+itemID+"/copy?@microsoft.graph.conflictBehavior=fail", body, nil)
	if isStatus(err, http.StatusConflict) {
		return "", errExists
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("copy %s: HTTP %d, not 202", name, resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("copy %s: no monitor URL", name)
	}
	return loc, nil
}

// copyStatus is the monitor's answer.
type copyStatus struct {
	Status             string  `json:"status"`
	PercentageComplete float64 `json:"percentageComplete"`
	ResourceID         string  `json:"resourceId"`
	Error              *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// pollCopy waits for the copy behind the monitor URL to finish (the monitor
// is pre-authenticated: no Authorization header; a 303 is the finished
// copy). progress is called on every change of percentage.
func (g *graph) pollCopy(ctx context.Context, monitor string, every time.Duration, progress func(copyStatus)) error {
	last := -1.0
	failures := 0
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, monitor, nil)
		if err != nil {
			return fmt.Errorf("monitor request: %w", err)
		}
		resp, err := g.monitor.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if failures++; failures > 10 {
				return fmt.Errorf("monitor: %w", err)
			}
		} else {
			var st copyStatus
			_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&st)
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusSeeOther:
				return nil
			case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
				if failures++; failures > 10 {
					return fmt.Errorf("monitor: HTTP %d", resp.StatusCode)
				}
			case resp.StatusCode >= 400:
				return fmt.Errorf("monitor: HTTP %d", resp.StatusCode)
			default:
				failures = 0
				if st.PercentageComplete != last && progress != nil {
					progress(st)
					last = st.PercentageComplete
				}
				switch st.Status {
				case "completed":
					return nil
				case "failed", "cancelled", "deletePending":
					msg := st.Status
					if st.Error != nil {
						msg += ": " + st.Error.Code + " " + st.Error.Message
					}
					return fmt.Errorf("copy %s", msg)
				}
			}
		}
		if err := g.wait(ctx, every); err != nil {
			return err
		}
	}
}

// putSmall writes a small file at path (≤ 4 MiB; a simple upload) and
// returns the item Graph answers with — its hashes included.
func (g *graph) putSmall(ctx context.Context, driveID, path string, body []byte, conflict string) (*item, error) {
	resp, err := g.do(ctx, http.MethodPut,
		"/drives/"+driveID+"/root:/"+escapePath(path)+":/content?@microsoft.graph.conflictBehavior="+conflict,
		body, map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var it item
	if err := json.NewDecoder(resp.Body).Decode(&it); err != nil {
		return nil, fmt.Errorf("decode put %s: %w", path, err)
	}
	return &it, nil
}

// content streams the item's bytes (Graph answers a 302 to the download
// host; the Authorization header is not carried across hosts).
func (g *graph) content(ctx context.Context, driveID, itemID string) (io.ReadCloser, error) {
	resp, err := g.do(ctx, http.MethodGet, "/drives/"+driveID+"/items/"+itemID+"/content", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("content: HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// permanentDelete removes the item with no recycle-bin entry (D-F1-5);
// a 404 is already gone.
func (g *graph) permanentDelete(ctx context.Context, driveID, itemID string) error {
	resp, err := g.do(ctx, http.MethodPost, "/drives/"+driveID+"/items/"+itemID+"/permanentDelete", nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}
