package middleware

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAccountLockout_LocksAfterThresholdAndExpires(t *testing.T) {
	l := NewAccountLockout(3, 15*time.Minute, 10*time.Minute)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	assert.False(t, l.Fail("Victim@Example.com"))
	assert.False(t, l.Fail("victim@example.com"))
	locked, _ := l.Locked("victim@example.com")
	assert.False(t, locked, "two failures are not a lockout")

	assert.True(t, l.Fail("victim@example.com "), "third failure locks (case/space-insensitive key)")
	locked, until := l.Locked("victim@example.com")
	assert.True(t, locked)
	assert.Equal(t, now.Add(10*time.Minute), until)

	now = now.Add(10*time.Minute + time.Second)
	locked, _ = l.Locked("victim@example.com")
	assert.False(t, locked, "the lock expires")
}

func TestAccountLockout_WindowResetsAndSuccessClears(t *testing.T) {
	l := NewAccountLockout(3, 15*time.Minute, 10*time.Minute)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	l.Fail("a@example.com")
	l.Fail("a@example.com")
	now = now.Add(16 * time.Minute)
	assert.False(t, l.Fail("a@example.com"), "failures outside the window do not accumulate")

	l.Fail("a@example.com")
	l.Reset("a@example.com")
	assert.False(t, l.Fail("a@example.com"), "a successful sign-in clears the count")
	assert.Equal(t, 1, l.Len())
}

func TestAccountLockout_MapIsBounded(t *testing.T) {
	l := NewAccountLockout(3, time.Minute, time.Minute)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	for i := 0; i < lockoutSweepEvery-1; i++ {
		l.Fail(string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@x.test")
	}
	before := l.Len()
	now = now.Add(2 * time.Minute) // every window has passed
	l.Fail("trigger@x.test")       // the Nth write sweeps
	assert.Less(t, l.Len(), before, "expired entries are swept on the periodic write")
}
