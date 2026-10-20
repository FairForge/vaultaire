package payments

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
)

// The readiness log (WP-PAY-2's ReadyLog, persisted by WP-PAY-3 — RP21-1,
// migration 098 payment_provider_readiness): each provider's last ready /
// unready observation and the reason it was last unready (kept across a
// ready observation). In memory as before, and — with a
// database — written through on a state change and once an hour, read back
// by UnreadyWithin, so a restart no longer forgets an outage and the lapse
// job's 24 h window is a real 24 h. Pending (Part 0) is never written: a
// provider whose verification has not completed is neither up nor down.

// ReadyLog remembers when each provider was last seen unready.
type ReadyLog struct {
	db     *sql.DB
	logger *zap.Logger

	mu          sync.Mutex
	lastUnready map[string]time.Time
	lastState   map[string]Readiness
	lastWritten map[string]time.Time
}

// NewReadyLog returns an in-memory log.
func NewReadyLog() *ReadyLog { return NewReadyLogDB(nil, nil) }

// NewReadyLogDB returns a log persisted in db (nil = memory only).
func NewReadyLogDB(db *sql.DB, logger *zap.Logger) *ReadyLog {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ReadyLog{db: db, logger: logger, lastUnready: map[string]time.Time{},
		lastState: map[string]Readiness{}, lastWritten: map[string]time.Time{}}
}

// readinessWriteEvery: a state that did not change is written this often.
const readinessWriteEvery = time.Hour

// Record notes one observation: only Unready is an outage; Ready and
// Pending (no verdict yet) leave the outage log alone. With a database,
// Ready and Unready are written on a change (the first observation of the
// process counts as one) and once an hour.
func (l *ReadyLog) Record(provider string, st Readiness, at time.Time) {
	l.RecordReason(provider, st, "", at)
}

// RecordReason is Record with the provider's reason (kept in the row).
func (l *ReadyLog) RecordReason(provider string, st Readiness, reason string, at time.Time) {
	if st == Pending {
		return
	}
	l.mu.Lock()
	if st == Unready && at.After(l.lastUnready[provider]) {
		l.lastUnready[provider] = at
	}
	prev, seen := l.lastState[provider]
	write := l.db != nil && (!seen || prev != st || at.Sub(l.lastWritten[provider]) >= readinessWriteEvery)
	if write {
		l.lastState[provider], l.lastWritten[provider] = st, at
	}
	l.mu.Unlock()
	if !write {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var err error
	if st == Ready {
		_, err = l.db.ExecContext(ctx, `
			INSERT INTO payment_provider_readiness (provider, last_ready_at, updated_at) VALUES ($1, $2, NOW())
			ON CONFLICT (provider) DO UPDATE SET last_ready_at = GREATEST(payment_provider_readiness.last_ready_at, EXCLUDED.last_ready_at),
			                                     updated_at = NOW()`, provider, at.UTC())
	} else {
		_, err = l.db.ExecContext(ctx, `
			INSERT INTO payment_provider_readiness (provider, last_unready_at, last_reason, updated_at) VALUES ($1, $2, NULLIF($3, ''), NOW())
			ON CONFLICT (provider) DO UPDATE SET last_unready_at = GREATEST(payment_provider_readiness.last_unready_at, EXCLUDED.last_unready_at),
			                                     last_reason = COALESCE(EXCLUDED.last_reason, payment_provider_readiness.last_reason), updated_at = NOW()`,
			provider, at.UTC(), reason)
	}
	if err != nil {
		l.logger.Warn("payments: readiness not persisted", zap.String("provider", provider), zap.String("state", st.String()), zap.Error(err))
		l.mu.Lock()
		delete(l.lastState, provider) // try again on the next observation
		l.mu.Unlock()
	}
}

// UnreadyWithin reports an unready observation in (now - window, now]: the
// process's own, else the table's (a previous process's).
func (l *ReadyLog) UnreadyWithin(ctx context.Context, provider string, window time.Duration, now time.Time) bool {
	l.mu.Lock()
	t, ok := l.lastUnready[provider]
	l.mu.Unlock()
	if ok && !t.Before(now.Add(-window)) {
		return true
	}
	if l.db == nil {
		return false
	}
	var last sql.NullTime
	if err := l.db.QueryRowContext(ctx, `SELECT last_unready_at FROM payment_provider_readiness WHERE provider = $1`, provider).Scan(&last); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			l.logger.Warn("payments: readiness not read", zap.String("provider", provider), zap.Error(err))
		}
		return false
	}
	return last.Valid && !last.Time.Before(now.Add(-window))
}
