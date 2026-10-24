package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/FairForge/vaultaire/internal/recovery"
)

// HTTPTransport speaks to Noise realms the way the SDK does
// (rust/sdk/src/request.rs): POST <address>/req carrying a ClientRequest;
// a request that needs forward secrecy goes only over an established
// session (HandshakeOnly first); the others may ride in the handshake; a
// MissingSession or a transient failure opens a new session (up to 5 tries).
type HTTPTransport struct {
	HTTP *http.Client
	// Token returns the realm's auth token (a JWT minted for this realm).
	Token func(ctx context.Context, realm Realm) (string, error)
	Rand  io.Reader // nil = crypto/rand
	// Now is for tests; nil = time.Now.
	Now func() time.Time

	mu       sync.Mutex
	sessions map[recovery.RealmID]*httpSession
}

type httpSession struct {
	id        uint32
	transport *recovery.NoiseTransport
	lifetime  time.Duration
	lastUsed  time.Time
}

// maxResponseBody bounds what a realm may send back.
const maxResponseBody = 8 << 10

func (h *HTTPTransport) rand() io.Reader {
	if h.Rand != nil {
		return h.Rand
	}
	return rand.Reader
}

func (h *HTTPTransport) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// take removes and returns the realm's live session, if any (the SDK holds
// a session per realm and takes it for the duration of a request).
func (h *HTTPTransport) take(id recovery.RealmID) *httpSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[id]
	delete(h.sessions, id)
	if s != nil && h.now().Sub(s.lastUsed) >= s.lifetime {
		return nil
	}
	return s
}

func (h *HTTPTransport) put(id recovery.RealmID, s *httpSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions == nil {
		h.sessions = map[recovery.RealmID]*httpSession{}
	}
	h.sessions[id] = s
}

var errMissingSession = errors.New("recovery: realm lost the session")

// Send implements Transport.
func (h *HTTPTransport) Send(ctx context.Context, realm Realm, req recovery.SecretsRequest) (recovery.SecretsResponse, error) {
	if len(realm.PublicKey) != 32 {
		return recovery.SecretsResponse{}, fmt.Errorf("recovery: realm %s has no 32-byte Noise public key", realm.ID)
	}
	payload, err := req.MarshalCBOR()
	if err != nil {
		return recovery.SecretsResponse{}, err
	}
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		sess := h.take(realm.ID)
		var padded []byte
		switch {
		case sess != nil:
			padded, err = h.transport(ctx, realm, sess, payload)
		case req.NeedsForwardSecrecy():
			if sess, _, err = h.handshake(ctx, realm, nil); err == nil {
				padded, err = h.transport(ctx, realm, sess, payload)
			}
		default:
			sess, padded, err = h.handshake(ctx, realm, payload)
		}
		if err == nil {
			h.put(realm.ID, sess)
			return recovery.UnmarshalPadded(padded)
		}
		lastErr = err
		if errors.Is(err, errMissingSession) || errors.Is(err, ErrTransient) {
			continue
		}
		return recovery.SecretsResponse{}, err
	}
	return recovery.SecretsResponse{}, lastErr
}

func (h *HTTPTransport) handshake(ctx context.Context, realm Realm, payload []byte) (*httpSession, []byte, error) {
	hs, msg, err := recovery.NoiseClientStart(h.rand(), [32]byte(realm.PublicKey), payload)
	if err != nil {
		return nil, nil, err
	}
	var idb [4]byte
	if _, err := io.ReadFull(h.rand(), idb[:]); err != nil {
		return nil, nil, fmt.Errorf("session id: %w", err)
	}
	kind := recovery.KindHandshakeOnly
	if len(payload) > 0 {
		kind = recovery.KindSecretsRequest
	}
	sess := &httpSession{id: binary.BigEndian.Uint32(idb[:])}
	resp, err := h.post(ctx, realm, recovery.ClientRequest{SessionID: sess.id, Kind: kind, Handshake: &msg})
	if err != nil {
		return nil, nil, err
	}
	if resp.Handshake == nil {
		return nil, nil, fmt.Errorf("%w: realm %s answered a handshake with a transport message", ErrMisbehavingRealm, realm.ID)
	}
	t, got, err := hs.Finish(*resp.Handshake)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: realm %s: %w", ErrMisbehavingRealm, realm.ID, err)
	}
	if len(payload) == 0 && len(got) != 0 {
		return nil, nil, fmt.Errorf("%w: realm %s answered an empty handshake with data", ErrMisbehavingRealm, realm.ID)
	}
	sess.transport, sess.lifetime, sess.lastUsed = t, resp.SessionLifetime, h.now()
	return sess, got, nil
}

func (h *HTTPTransport) transport(ctx context.Context, realm Realm, sess *httpSession, payload []byte) ([]byte, error) {
	ct, err := sess.transport.Encrypt(payload)
	if err != nil {
		return nil, err
	}
	resp, err := h.post(ctx, realm, recovery.ClientRequest{SessionID: sess.id, Kind: recovery.KindSecretsRequest, Transport: ct})
	if err != nil {
		return nil, err
	}
	if resp.Handshake != nil {
		return nil, fmt.Errorf("%w: realm %s answered a transport message with a handshake", ErrMisbehavingRealm, realm.ID)
	}
	pt, err := sess.transport.Decrypt(resp.Transport)
	if err != nil {
		return nil, fmt.Errorf("%w: realm %s: %w", ErrMisbehavingRealm, realm.ID, err)
	}
	sess.lastUsed = h.now()
	return pt, nil
}

// post sends the envelope and maps the envelope's outcome to an error.
func (h *HTTPTransport) post(ctx context.Context, realm Realm, req recovery.ClientRequest) (recovery.ClientResponse, error) {
	token, err := h.Token(ctx, realm)
	if err != nil {
		return recovery.ClientResponse{}, fmt.Errorf("%w: token for realm %s: %w", ErrInvalidAuth, realm.ID, err)
	}
	req.Realm, req.AuthToken = realm.ID, token
	body, err := req.MarshalCBOR()
	if err != nil {
		return recovery.ClientResponse{}, err
	}
	url := strings.TrimSuffix(realm.Address, "/") + "/req"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return recovery.ClientResponse{}, fmt.Errorf("realm %s request: %w", realm.ID, err)
	}
	hreq.Header.Set("Content-Type", "application/octet-stream")
	hreq.Header.Set("X-Juicebox-Version", "0.3.7")
	client := h.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	hresp, err := client.Do(hreq)
	if err != nil {
		return recovery.ClientResponse{}, fmt.Errorf("%w: realm %s: %w", ErrTransient, realm.ID, err)
	}
	defer func() { _ = hresp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, maxResponseBody))
	if err != nil {
		return recovery.ClientResponse{}, fmt.Errorf("%w: realm %s: %w", ErrTransient, realm.ID, err)
	}
	resp, derr := recovery.UnmarshalClientResponse(raw)
	if derr != nil { // a body that is not an envelope: the status speaks (request.rs:45)
		switch hresp.StatusCode {
		case http.StatusUnauthorized:
			return resp, fmt.Errorf("%w: realm %s", ErrInvalidAuth, realm.ID)
		case http.StatusOK:
			return resp, fmt.Errorf("%w: realm %s: %w", ErrMisbehavingRealm, realm.ID, derr)
		default:
			return resp, fmt.Errorf("%w: realm %s: HTTP %d", ErrTransient, realm.ID, hresp.StatusCode)
		}
	}
	switch resp.Status {
	case recovery.ClientOK:
		return resp, nil
	case recovery.ClientInvalidAuth:
		return resp, fmt.Errorf("%w: realm %s", ErrInvalidAuth, realm.ID)
	case recovery.ClientMissingSession:
		return resp, errMissingSession
	case recovery.ClientUnavailable, recovery.ClientRateLimitExceeded:
		return resp, fmt.Errorf("%w: realm %s: %v", ErrTransient, realm.ID, resp.Status)
	default:
		return resp, fmt.Errorf("%w: realm %s: %v", ErrMisbehavingRealm, realm.ID, resp.Status)
	}
}
