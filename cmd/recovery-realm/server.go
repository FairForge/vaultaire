package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FairForge/vaultaire/internal/recovery"
	"github.com/FairForge/vaultaire/internal/recovery/realm"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// The realm's HTTP face (docs/reviews/K5-PROBE.md § The wire): POST /req
// with a ClientRequest; the tenant token is inside it; every SecretsRequest
// travels under Noise NK to this realm's static key; one needing forward
// secrecy only over an established session. GET / names the realm and its
// public key (the key a client pins and the transparency log records).

const (
	// maxEnvelope bounds a ClientRequest: a SecretsRequest of at most
	// BODY_SIZE_LIMIT plus the token (≤ 1000) and the Noise framing.
	maxEnvelope     = recovery.MaxRequestBody + maxTokenLength + 256
	sessionLifetime = 5 * time.Minute
	maxSessions     = 100_000
	// maxSessionsPerSubject bounds one user's share of the table (RP52-2):
	// the SDK keeps one session per realm, so a few cover retries.
	maxSessionsPerSubject = 8
)

var ops = []string{"handshake", "register1", "register2", "recover1", "recover2", "recover3", "delete", "unknown"}

var outcomes = []string{
	"ok", "not_registered", "no_guesses", "version_mismatch", "bad_unlock_key_tag",
	"invalid_auth", "decoding_error", "session_error", "missing_session", "payload_too_large", "unavailable",
}

type metrics struct {
	requests  *prometheus.CounterVec
	destroyed prometheus.Counter
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "recovery_realm_requests_total",
			Help: "Requests by operation and outcome (op unknown = refused before the request could be read).",
		}, []string{"op", "outcome"}),
		destroyed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "recovery_realm_records_destroyed_total",
			Help: "Records that ran out of guesses and were erased.",
		}),
	}
	reg.MustRegister(m.requests, m.destroyed)
	for _, op := range ops {
		for _, o := range outcomes {
			m.requests.WithLabelValues(op, o)
		}
	}
	return m
}

func opLabel(o recovery.Op) string { return strings.ToLower(o.String()) }

var statusLabel = map[recovery.Status]string{
	recovery.StatusOK: "ok", recovery.StatusNotRegistered: "not_registered", recovery.StatusNoGuesses: "no_guesses",
	recovery.StatusVersionMismatch: "version_mismatch", recovery.StatusBadUnlockKeyTag: "bad_unlock_key_tag",
}

type sessionKey struct {
	tenant, sub string
	id          uint32
}

// session is one client's Noise transport. mu serialises its requests (the
// nonces are sequential); lastUsed is atomic so the session table's lock is
// never taken while a session's is held the other way round.
type session struct {
	mu        sync.Mutex
	transport *recovery.NoiseTransport
	lastUsed  atomic.Int64 // unix nanoseconds
}

func (s *session) idle(now time.Time) bool {
	return now.UnixNano()-s.lastUsed.Load() >= int64(sessionLifetime)
}

type server struct {
	id           recovery.RealmID
	staticSecret [32]byte
	staticPublic [32]byte
	realm        *realm.Realm
	auth         *authenticator
	metrics      *metrics
	logger       *zap.Logger
	rand         io.Reader
	now          func() time.Time

	mu       sync.Mutex
	sessions map[sessionKey]*session
	// bySubject indexes sessions by (tenant, subject) for the per-subject
	// bound (RP52-2); kept in step with sessions by putSession/deleteLocked.
	bySubject map[subjectKey]map[uint32]struct{}
}

type subjectKey struct{ tenant, sub string }

// deleteLocked removes one session and its index entry; s.mu is held.
func (s *server) deleteLocked(key sessionKey) {
	delete(s.sessions, key)
	sk := subjectKey{key.tenant, key.sub}
	if ids := s.bySubject[sk]; ids != nil {
		delete(ids, key.id)
		if len(ids) == 0 {
			delete(s.bySubject, sk)
		}
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /req", s.handleReq)
	mux.HandleFunc("GET /{$}", s.handleIdentity)
	return mux
}

func (s *server) handleIdentity(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"realm_id":   s.id.String(),
		"public_key": hex.EncodeToString(s.staticPublic[:]),
	})
}

// reply writes an envelope answer with its HTTP status.
func (s *server) reply(w http.ResponseWriter, status int, resp recovery.ClientResponse, op, outcome string) {
	s.metrics.requests.WithLabelValues(op, outcome).Inc()
	body, err := resp.MarshalCBOR()
	if err != nil {
		s.logger.Error("recovery realm: encode answer", zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *server) fail(w http.ResponseWriter, st recovery.ClientStatus, op string) {
	code, outcome := http.StatusBadRequest, "decoding_error"
	switch st {
	case recovery.ClientInvalidAuth:
		code, outcome = http.StatusUnauthorized, "invalid_auth"
	case recovery.ClientPayloadTooLarge:
		code, outcome = http.StatusRequestEntityTooLarge, "payload_too_large"
	case recovery.ClientSessionError:
		outcome = "session_error"
	case recovery.ClientMissingSession:
		code, outcome = http.StatusOK, "missing_session"
	case recovery.ClientUnavailable:
		code, outcome = http.StatusServiceUnavailable, "unavailable"
	}
	s.reply(w, code, recovery.ClientResponse{Status: st}, op, outcome)
}

func (s *server) handleReq(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEnvelope+1))
	if err != nil {
		s.fail(w, recovery.ClientDecodingError, "unknown")
		return
	}
	if len(body) > maxEnvelope {
		s.fail(w, recovery.ClientPayloadTooLarge, "unknown")
		return
	}
	req, err := recovery.UnmarshalClientRequest(body)
	if err != nil {
		s.fail(w, recovery.ClientDecodingError, "unknown")
		return
	}
	who, err := s.auth.validate(req.AuthToken)
	if err != nil || req.Realm != s.id {
		s.logger.Debug("recovery realm: refused token", zap.Error(err)) // never the token
		s.fail(w, recovery.ClientInvalidAuth, "unknown")
		return
	}
	key := sessionKey{tenant: who.Tenant, sub: who.Subject, id: req.SessionID}
	if req.Handshake != nil {
		s.handshake(r.Context(), w, who, key, req)
		return
	}
	s.transport(r.Context(), w, who, key, req.Transport)
}

// handle runs one SecretsRequest and pads its answer; ok=false means the
// realm could not answer (the store failed): the caller says Unavailable.
func (s *server) handle(ctx context.Context, who realm.Auth, sreq recovery.SecretsRequest) ([]byte, string, bool) {
	resp, err := s.realm.Handle(ctx, who, sreq)
	if err != nil {
		if !errors.Is(err, realm.ErrBadRequest) {
			s.logger.Error("recovery realm: request failed", zap.String("op", sreq.Op.String()), zap.Error(err))
		}
		return nil, "unavailable", false
	}
	padded, err := resp.MarshalPadded()
	if err != nil {
		s.logger.Error("recovery realm: pad answer", zap.Error(err))
		return nil, "unavailable", false
	}
	return padded, statusLabel[resp.Status], true
}

func (s *server) handshake(ctx context.Context, w http.ResponseWriter, who realm.Auth, key sessionKey, req recovery.ClientRequest) {
	hs, payload, err := recovery.NoiseServerStart(s.rand, s.staticSecret, s.staticPublic, *req.Handshake)
	if err != nil {
		s.fail(w, recovery.ClientSessionError, "handshake")
		return
	}
	op, outcome := "handshake", "ok"
	var answer []byte
	if req.Kind == recovery.KindHandshakeOnly {
		if len(payload) != 0 {
			s.fail(w, recovery.ClientDecodingError, "handshake")
			return
		}
	} else {
		sreq, err := recovery.UnmarshalSecretsRequest(payload)
		if err != nil {
			s.fail(w, recovery.ClientDecodingError, "unknown")
			return
		}
		op = opLabel(sreq.Op)
		if sreq.NeedsForwardSecrecy() { // never under the static key alone (requests.rs:147)
			s.fail(w, recovery.ClientDecodingError, op)
			return
		}
		var ok bool
		if answer, outcome, ok = s.handle(ctx, who, sreq); !ok {
			s.fail(w, recovery.ClientUnavailable, op)
			return
		}
	}
	t, resp, err := hs.Finish(answer)
	if err != nil {
		s.fail(w, recovery.ClientSessionError, op)
		return
	}
	sess := &session{transport: t}
	sess.lastUsed.Store(s.now().UnixNano())
	lifetime := sessionLifetime
	if !s.putSession(key, sess) {
		if req.Kind == recovery.KindHandshakeOnly {
			s.fail(w, recovery.ClientUnavailable, op)
			return
		}
		// The table is full but the request it carried is already applied
		// (RP54-2: a Delete done and answered Unavailable): answer it and keep
		// no session — lifetime 0, so the client opens a new one next time.
		lifetime = 0
	}
	s.reply(w, http.StatusOK, recovery.ClientResponse{Handshake: &resp, SessionLifetime: lifetime}, op, outcome)
}

func (s *server) transport(ctx context.Context, w http.ResponseWriter, who realm.Auth, key sessionKey, ct []byte) {
	sess := s.getSession(key)
	if sess == nil {
		s.fail(w, recovery.ClientMissingSession, "unknown")
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	pt, err := sess.transport.Decrypt(ct)
	if err != nil {
		s.dropSession(key)
		s.fail(w, recovery.ClientSessionError, "unknown")
		return
	}
	sreq, err := recovery.UnmarshalSecretsRequest(pt)
	if err != nil {
		s.dropSession(key)
		s.fail(w, recovery.ClientDecodingError, "unknown")
		return
	}
	op := opLabel(sreq.Op)
	answer, outcome, ok := s.handle(ctx, who, sreq)
	if !ok {
		s.dropSession(key) // the client opens a new one for its retry
		s.fail(w, recovery.ClientUnavailable, op)
		return
	}
	out, err := sess.transport.Encrypt(answer)
	if err != nil {
		s.dropSession(key)
		s.fail(w, recovery.ClientSessionError, op)
		return
	}
	sess.lastUsed.Store(s.now().UnixNano())
	s.reply(w, http.StatusOK, recovery.ClientResponse{Transport: out}, op, outcome)
}

func (s *server) getSession(key sessionKey) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[key]
	if sess == nil {
		return nil
	}
	if sess.idle(s.now()) {
		s.deleteLocked(key)
		return nil
	}
	return sess
}

func (s *server) putSession(key sessionKey, sess *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sk := subjectKey{key.tenant, key.sub}
	if _, exists := s.sessions[key]; !exists {
		// A subject at its bound gives up its least recently used session:
		// one user can crowd out only themselves (RP52-2).
		if ids := s.bySubject[sk]; len(ids) >= maxSessionsPerSubject {
			var oldest sessionKey
			var at int64
			for id := range ids {
				k := sessionKey{tenant: key.tenant, sub: key.sub, id: id}
				sess := s.sessions[k]
				if sess == nil { // an index entry with no session: drop it first
					oldest = k
					break
				}
				if t := sess.lastUsed.Load(); at == 0 || t < at {
					oldest, at = k, t
				}
			}
			s.deleteLocked(oldest)
		}
		if len(s.sessions) >= maxSessions {
			return false
		}
	}
	s.sessions[key] = sess
	if s.bySubject == nil {
		s.bySubject = map[subjectKey]map[uint32]struct{}{}
	}
	if s.bySubject[sk] == nil {
		s.bySubject[sk] = map[uint32]struct{}{}
	}
	s.bySubject[sk][key.id] = struct{}{}
	return true
}

func (s *server) dropSession(key sessionKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteLocked(key)
}

// sweepSessions drops idle sessions until ctx ends.
func (s *server) sweepSessions(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			now := s.now()
			for k, sess := range s.sessions {
				if sess.idle(now) {
					s.deleteLocked(k)
				}
			}
			s.mu.Unlock()
		}
	}
}
