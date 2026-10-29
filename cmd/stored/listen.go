package main

// stored listen (WP-DX-1): a long poll on the account's event log
// (GET /api/v1/events?after=<cursor>&wait=25), each new event POSTed once,
// in order (the last 1,000 ids forwarded are remembered, so a re-read never
// delivers one twice — RP80-3), to the local endpoint — the body a webhook delivery carries,
// signed X-Webhook-Signature with the listen secret (printed at start, or
// --secret / STORED_LISTEN_SECRET) — and the cursor saved after each one,
// so a restart resumes where it stopped. The first run starts at now: the
// log's history is never replayed. Only a local address is forwarded to.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/webhooksig"
)

type listener struct {
	api, token, forward, cursorFile, secret string
	client                                  *http.Client
	out                                     io.Writer
	wait                                    time.Duration

	// seen / order: the last listenSeenMax ids forwarded (RP80-3), so a
	// re-read of the log never delivers an event twice.
	seen  map[string]struct{}
	order []string
}

// listenSeenMax is how many forwarded ids the listener remembers.
const listenSeenMax = 1000

// forwarded reports whether id was forwarded by this listener.
func (l *listener) forwarded(id string) bool {
	_, ok := l.seen[id]
	return ok
}

// remember records a forwarded id, forgetting the oldest past
// listenSeenMax.
func (l *listener) remember(id string) {
	if l.seen == nil {
		l.seen = make(map[string]struct{}, listenSeenMax)
	}
	if _, ok := l.seen[id]; ok {
		return
	}
	l.seen[id] = struct{}{}
	l.order = append(l.order, id)
	if len(l.order) > listenSeenMax {
		delete(l.seen, l.order[0])
		l.order = l.order[1:]
	}
}

func runListen(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	forward := fs.String("forward-to", "", "the local endpoint: localhost:<port>/<path>")
	home, _ := os.UserHomeDir()
	cursorFile := fs.String("cursor-file", filepath.Join(home, ".stored", "listen.cursor"), "where the cursor is kept")
	secret := fs.String("secret", os.Getenv("STORED_LISTEN_SECRET"), "the signing secret of the forwarded events (default: a new one, printed)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *forward == "" {
		return errors.New("--forward-to is required")
	}
	if _, err := forwardURL(*forward); err != nil {
		return err
	}
	token := os.Getenv("STORED_TOKEN")
	if token == "" {
		return errors.New("STORED_TOKEN is not set (a management token: POST /auth/login)")
	}
	api := strings.TrimRight(os.Getenv("STORED_API"), "/")
	if api == "" {
		api = "https://stored.cloud"
	}
	if *secret == "" {
		var b [24]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		*secret = "whsec_" + hex.EncodeToString(b[:])
		_, _ = fmt.Fprintf(out, "Ready. Your listen signing secret is %s (verify X-Webhook-Signature with it)\n", *secret)
	}
	l := &listener{api: api, token: token, forward: *forward, cursorFile: *cursorFile, secret: *secret,
		client: &http.Client{Timeout: 40 * time.Second}, out: out, wait: 25 * time.Second}
	for ctx.Err() == nil {
		if _, err := l.once(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			_, _ = fmt.Fprintln(out, "poll:", err, "— retrying in 5 s")
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
	return nil
}

// forwardURL is the http:// URL of a local forward target, or an error for
// any other host.
func forwardURL(target string) (string, error) {
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("--forward-to %q: not a URL", target)
	}
	host := u.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("--forward-to %q: only a local address (localhost, 127.0.0.1, ::1)", target)
		}
	}
	return u.String(), nil
}

// once is one poll: the events after the cursor (a long poll), each
// forwarded and the cursor saved after it. → how many were forwarded.
func (l *listener) once(ctx context.Context) (int, error) {
	cursor := l.readCursor()
	if cursor == "" {
		cursor = "now"
	}
	q := url.Values{"after": {cursor}, "wait": {fmt.Sprint(int(l.wait.Seconds()))}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.api+"/api/v1/events?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+l.token)
	resp, err := l.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusBadRequest {
		// The cursor expired (90-day retention) or is another account's.
		_, _ = fmt.Fprintln(l.out, "the saved cursor is unknown: starting again from now")
		return 0, l.saveCursor("")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("GET /api/v1/events: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var page struct {
		Data       []json.RawMessage `json:"data"`
		NextCursor string            `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return 0, fmt.Errorf("decode the events: %w", err)
	}
	target, err := forwardURL(l.forward)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, raw := range page.Data {
		var ev struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			return n, fmt.Errorf("decode an event: %w", err)
		}
		if l.forwarded(ev.ID) {
			// A re-read answered it again: never forwarded twice.
			if err := l.saveCursor(ev.ID); err != nil {
				return n, err
			}
			continue
		}
		if err := l.post(ctx, target, ev.ID, raw); err != nil {
			return n, err // the cursor stays before it: retried next poll
		}
		l.remember(ev.ID)
		_, _ = fmt.Fprintf(l.out, "%s  %s  %s → %s\n", time.Now().Format("15:04:05"), ev.Type, ev.ID, target)
		if err := l.saveCursor(ev.ID); err != nil {
			return n, err
		}
		n++
	}
	if len(page.Data) == 0 && cursor == "now" && page.NextCursor != "" {
		return 0, l.saveCursor(page.NextCursor)
	}
	return n, nil
}

// post forwards one event's body, signed; a non-2xx answer is an error.
func (l *listener) post(ctx context.Context, target, id string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Event-ID", id)
	req.Header.Set("X-Webhook-Signature", webhooksig.Sign(body, l.secret, time.Now()))
	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("forward %s: %w", id, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("forward %s: the local endpoint answered %d", id, resp.StatusCode)
	}
	return nil
}

func (l *listener) readCursor() string {
	b, err := os.ReadFile(l.cursorFile) // #nosec G304 -- the developer's own cursor file
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// saveCursor writes the cursor atomically ("" removes it: start at now).
func (l *listener) saveCursor(id string) error {
	if id == "" {
		err := os.Remove(l.cursorFile)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.cursorFile), 0o700); err != nil {
		return err
	}
	tmp := l.cursorFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.cursorFile)
}
