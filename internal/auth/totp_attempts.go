// internal/auth/totp_attempts.go
//
// RP90-7 (Stage 1a Prompt 56 Part 0): ONE per-user counter of wrong
// authenticator codes for every verifier site — the agent consent (which
// mints a 30-day credential), the approvals (API and dashboard), the
// account's deletion, S3 MFA Delete, the translog ceremony and both sign-in
// second factors. NIST SP 800-63B § 5.2.2 asks the verifier to throttle:
// a hijacked session must not guess a 6-digit code at will. 10 wrong codes
// in 15 minutes lock every code path of that user for 15 minutes, audited
// auth.mfa_locked once per lock. In-process, like the replay guard
// (ConsumeTOTPCode) beside it: each slot counts its own.
package auth

import (
	"context"
	"errors"
	"time"

	"github.com/FairForge/vaultaire/internal/audit"
)

const (
	// TOTPMaxFailures wrong codes within TOTPFailureWindow lock the user's
	// code paths for TOTPLockFor.
	TOTPMaxFailures   = 10
	TOTPFailureWindow = 15 * time.Minute
	TOTPLockFor       = 15 * time.Minute
)

var (
	// ErrTOTPInvalid: the code is wrong, expired or already used.
	ErrTOTPInvalid = errors.New("the authenticator code is wrong, expired or already used")
	// ErrTOTPLocked: too many wrong codes; every code path waits.
	ErrTOTPLocked = errors.New("too many wrong authenticator codes: try again in 15 minutes")
)

// TOTPValidator checks a code against a secret (MFAService).
type TOTPValidator interface {
	ValidateCode(secret, code string) bool
}

type totpFailures struct {
	first       time.Time // the window's start
	count       int
	lockedUntil time.Time
}

// TOTPLocked reports whether userID's code paths are locked now.
func (a *AuthService) TOTPLocked(userID string) bool {
	a.mfaMu.Lock()
	defer a.mfaMu.Unlock()
	f := a.totpFails[userID]
	return f != nil && time.Now().Before(f.lockedUntil)
}

// NoteTOTPFailure counts one wrong code of userID at site; the failure that
// reaches TOTPMaxFailures locks the user and is audited auth.mfa_locked.
// It reports whether the user is locked now.
func (a *AuthService) NoteTOTPFailure(ctx context.Context, userID, site string) bool {
	if userID == "" {
		return false
	}
	now := time.Now()
	a.mfaMu.Lock()
	if a.totpFails == nil {
		a.totpFails = make(map[string]*totpFailures)
	}
	if len(a.totpFails) > 4096 {
		for id, f := range a.totpFails {
			if now.Sub(f.first) > TOTPFailureWindow && now.After(f.lockedUntil) {
				delete(a.totpFails, id)
			}
		}
	}
	f := a.totpFails[userID]
	if f == nil || now.Sub(f.first) > TOTPFailureWindow {
		f = &totpFailures{first: now}
		a.totpFails[userID] = f
	}
	if now.Before(f.lockedUntil) {
		a.mfaMu.Unlock()
		return true
	}
	f.count++
	lockedNow := f.count >= TOTPMaxFailures
	if lockedNow {
		f.lockedUntil = now.Add(TOTPLockFor)
		f.first, f.count = now, 0
	}
	a.mfaMu.Unlock()
	if lockedNow {
		a.record(ctx, audit.Entry{Actor: userID, UserID: userID, EventType: "auth", Action: "auth.mfa_locked", Severity: "error",
			Metadata: map[string]any{"site": site, "failures": TOTPMaxFailures, "locked_minutes": int(TOTPLockFor.Minutes())}})
	}
	return lockedNow
}

// clearTOTPFailures forgets userID's wrong codes (a code was right).
func (a *AuthService) clearTOTPFailures(userID string) {
	a.mfaMu.Lock()
	defer a.mfaMu.Unlock()
	if f := a.totpFails[userID]; f != nil && !time.Now().Before(f.lockedUntil) {
		delete(a.totpFails, userID)
	}
}

// CheckFreshTOTP is THE check of a fresh authenticator code at a verifier
// site (RP90-7): refused while the user is locked (ErrTOTPLocked, whatever
// the code); else the code must be the user's current one, accepted once
// (ConsumeTOTPCode) — a wrong one is counted (ErrTOTPInvalid, or
// ErrTOTPLocked when it is the one that locks). A user without a secret
// is ErrTOTPInvalid and counts nothing.
func (a *AuthService) CheckFreshTOTP(ctx context.Context, v TOTPValidator, userID, code, site string) error {
	if a.TOTPLocked(userID) {
		return ErrTOTPLocked
	}
	if v == nil || userID == "" {
		return ErrTOTPInvalid
	}
	secret, err := a.GetMFASecret(ctx, userID)
	if err != nil || secret == "" {
		return ErrTOTPInvalid
	}
	if code != "" && v.ValidateCode(secret, code) && a.ConsumeTOTPCode(userID, code) {
		a.clearTOTPFailures(userID)
		return nil
	}
	if code == "" {
		return ErrTOTPInvalid // nothing guessed
	}
	if a.NoteTOTPFailure(ctx, userID, site) {
		return ErrTOTPLocked
	}
	return ErrTOTPInvalid
}
