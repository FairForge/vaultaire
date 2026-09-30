package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const mfaPendingTTL = 5 * time.Minute

// MFAPending holds the intermediate state between password validation and
// TOTP verification during login.
type MFAPending struct {
	UserID   string
	TenantID string
	Email    string
	Role     string
	Expires  time.Time
}

// MFAPendingStore is a short-lived in-memory store for pending 2FA challenges.
type MFAPendingStore struct {
	mu      sync.Mutex
	entries map[string]*MFAPending
}

// NewMFAPendingStore creates a new pending MFA store.
func NewMFAPendingStore() *MFAPendingStore {
	return &MFAPendingStore{entries: make(map[string]*MFAPending)}
}

// mfaPendingSweepAt: Create sweeps expired entries whenever the store holds
// this many, so an attacker replaying the first factor (or a wave of users
// who never finish the second step) cannot grow it without bound (R5-15d —
// entries used to be evicted only when their own token was presented).
const mfaPendingSweepAt = 256

// Create stores a pending MFA challenge and returns a random token.
func (s *MFAPendingStore) Create(p MFAPending) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate mfa pending token: %w", err)
	}
	token := hex.EncodeToString(b)

	p.Expires = time.Now().Add(mfaPendingTTL)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= mfaPendingSweepAt {
		s.sweepLocked(time.Now())
	}
	s.entries[token] = &p
	return token, nil
}

// Sweep drops expired challenges. Create calls it inline; exported for tests.
func (s *MFAPendingStore) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(time.Now())
}

func (s *MFAPendingStore) sweepLocked(now time.Time) {
	for k, p := range s.entries {
		if now.After(p.Expires) {
			delete(s.entries, k)
		}
	}
}

// Len is the number of live entries (tests).
func (s *MFAPendingStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Get retrieves and deletes a pending MFA challenge (single use).
func (s *MFAPendingStore) Get(token string) *MFAPending {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.entries[token]
	if !ok || time.Now().After(p.Expires) {
		delete(s.entries, token)
		return nil
	}
	delete(s.entries, token)
	return p
}

// Peek retrieves a pending MFA challenge without consuming it.
func (s *MFAPendingStore) Peek(token string) *MFAPending {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.entries[token]
	if !ok || time.Now().After(p.Expires) {
		delete(s.entries, token)
		return nil
	}
	return p
}
