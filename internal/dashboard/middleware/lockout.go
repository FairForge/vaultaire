package middleware

import (
	"strings"
	"sync"
	"time"
)

// AccountLockout counts failed sign-ins per account and refuses further
// attempts for a while once a threshold is crossed (pre-launch checklist
// item 2: "lockout/alert on N failures"). It complements the per-IP
// LoginRateLimiter: a distributed guess against ONE account is invisible to
// per-IP limits, and one office IP typing a wrong password must not lock a
// whole team out of every account.
//
// Keys are normalised e-mail addresses. The map is bounded: entries whose
// window has passed are swept on every Nth write, and a runaway map is
// reset outright (the same shape as the waitlist limiter).
type AccountLockout struct {
	mu        sync.Mutex
	entries   map[string]*lockoutEntry
	threshold int
	window    time.Duration
	lockFor   time.Duration
	writes    int
	now       func() time.Time
}

type lockoutEntry struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

const (
	lockoutSweepEvery = 256
	lockoutMaxEntries = 50000
)

// NewAccountLockout locks an account for lockFor after threshold failures
// inside window.
func NewAccountLockout(threshold int, window, lockFor time.Duration) *AccountLockout {
	return &AccountLockout{
		entries:   make(map[string]*lockoutEntry),
		threshold: threshold,
		window:    window,
		lockFor:   lockFor,
		now:       time.Now,
	}
}

func lockoutKey(account string) string {
	return strings.ToLower(strings.TrimSpace(account))
}

// Locked reports whether the account is currently locked and until when.
func (l *AccountLockout) Locked(account string) (bool, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[lockoutKey(account)]
	if !ok {
		return false, time.Time{}
	}
	now := l.now()
	if now.Before(e.lockedUntil) {
		return true, e.lockedUntil
	}
	return false, time.Time{}
}

// Fail records one failed attempt. It returns true when this failure crossed
// the threshold and locked the account (the caller records the lockout).
func (l *AccountLockout) Fail(account string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.writes++
	if l.writes%lockoutSweepEvery == 0 || len(l.entries) > lockoutMaxEntries {
		l.sweepLocked(now)
	}
	k := lockoutKey(account)
	e, ok := l.entries[k]
	if !ok || now.Sub(e.windowStart) > l.window {
		e = &lockoutEntry{windowStart: now}
		l.entries[k] = e
	}
	e.failures++
	if e.failures >= l.threshold && now.After(e.lockedUntil) {
		e.lockedUntil = now.Add(l.lockFor)
		e.failures = 0
		e.windowStart = now
		return true
	}
	return false
}

// Reset clears the account's failure count after a successful sign-in.
func (l *AccountLockout) Reset(account string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, lockoutKey(account))
}

// sweepLocked drops entries that are neither locked nor inside their window.
// Caller holds mu.
func (l *AccountLockout) sweepLocked(now time.Time) {
	if len(l.entries) > lockoutMaxEntries {
		l.entries = make(map[string]*lockoutEntry)
		return
	}
	for k, e := range l.entries {
		if now.After(e.lockedUntil) && now.Sub(e.windowStart) > l.window {
			delete(l.entries, k)
		}
	}
}

// Len is the number of tracked accounts (tests).
func (l *AccountLockout) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
