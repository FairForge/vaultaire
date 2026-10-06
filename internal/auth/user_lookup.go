package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The users table is the authority for every auth decision; the in-memory
// maps are a cache of it. LoadFromDB fills them once at boot, but two app
// instances overlap during a zero-downtime deploy (#605): a registration,
// password change, MFA change or erasure done on the old instance after the
// new one booted used to be invisible to the new one until its next restart
// (a new user got "user not found", an old password kept working, MFA could
// be skipped). Sign-in, session, MFA, password and registration flows —
// low volume — therefore read the user's row on every call and refresh the
// cache from it. The S3 hot path never comes here (Auth.LookupCredential
// queries api_keys itself). With no database (unit tests) the maps are the
// only store, as before.

// userColumns is one users row plus the owning tenant, linked by e-mail the
// way LoadFromDB links it.
const userColumns = `
	SELECT u.id::text, u.email, u.password_hash, COALESCE(u.company, ''),
	       u.created_at, u.updated_at, COALESCE(u.email_verified, FALSE),
	       u.password_changed_at,
	       COALESCE((SELECT t.id FROM tenants t WHERE t.email = u.email
	                 ORDER BY t.created_at DESC LIMIT 1), '')
	FROM users u`

// fetchUserRow reads one user. (nil, nil) = no such row.
func (a *AuthService) fetchUserRow(ctx context.Context, where string, arg any) (*User, error) {
	u := &User{}
	var changed sql.NullTime
	err := a.sqlDB.QueryRowContext(ctx, userColumns+" WHERE "+where, arg).Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.Company, &u.CreatedAt, &u.UpdatedAt,
		&u.EmailVerified, &changed, &u.TenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read user: %w", err)
	}
	if changed.Valid {
		t := changed.Time
		u.PasswordChangedAt = &t
	}
	return u, nil
}

// cacheUser writes row into the maps and returns the cached pointer. An
// existing entry is updated in place, so a pointer another caller holds sees
// the same values; an e-mail that changed is re-keyed.
func (a *AuthService) cacheUser(row *User) *User {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	u, ok := a.userIndex[row.ID]
	if !ok {
		a.users[row.Email] = row
		a.userIndex[row.ID] = row
		return row
	}
	if u.Email != row.Email {
		delete(a.users, u.Email)
	}
	tenantID := u.TenantID
	*u = *row
	if u.TenantID == "" {
		u.TenantID = tenantID
	}
	a.users[u.Email] = u
	return u
}

// forgetUser drops a user the database no longer has (erased elsewhere).
func (a *AuthService) forgetUser(id, email string) {
	a.cacheMu.Lock()
	if id == "" {
		if u, ok := a.users[email]; ok {
			id = u.ID
		}
	}
	if u, ok := a.userIndex[id]; ok {
		delete(a.users, u.Email)
		delete(a.userIndex, id)
	}
	if email != "" {
		delete(a.users, email)
	}
	a.cacheMu.Unlock()
	if id != "" {
		a.mfaMu.Lock()
		delete(a.mfaSettings, id)
		a.mfaMu.Unlock()
	}
}

// userByEmail returns the cached *User for email, read from the database
// first when there is one. (nil, nil) = no such user. The pointer is the
// cache's: read or write its fields under cacheMu.
func (a *AuthService) userByEmail(ctx context.Context, email string) (*User, error) {
	if a.sqlDB == nil {
		a.cacheMu.RLock()
		defer a.cacheMu.RUnlock()
		return a.users[email], nil
	}
	row, err := a.fetchUserRow(ctx, "u.email = $1", email)
	if err != nil {
		return nil, err
	}
	if row == nil {
		a.forgetUser("", email)
		return nil, nil
	}
	return a.cacheUser(row), nil
}

// userByID is userByEmail by user id. An id that is not a UUID cannot be a
// users row (the column is UUID) and is not found.
func (a *AuthService) userByID(ctx context.Context, userID string) (*User, error) {
	if a.sqlDB == nil {
		a.cacheMu.RLock()
		defer a.cacheMu.RUnlock()
		return a.userIndex[userID], nil
	}
	if _, err := uuid.Parse(userID); err != nil {
		a.forgetUser(userID, "")
		return nil, nil
	}
	row, err := a.fetchUserRow(ctx, "u.id = $1", userID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		a.forgetUser(userID, "")
		return nil, nil
	}
	return a.cacheUser(row), nil
}

// jwtLookupTimeout bounds the one database read ValidateJWT makes for a user
// this process has not cached (it has no request context).
const jwtLookupTimeout = 3 * time.Second

// refreshMFA reads the user's user_mfa row into mfaSettings (an enabled row
// replaces the entry, none removes it). With no database it does nothing.
func (a *AuthService) refreshMFA(ctx context.Context, userID string) error {
	if a.sqlDB == nil {
		return nil
	}
	var (
		s         = MFASettings{UserID: userID}
		codesJSON sql.NullString
	)
	err := a.sqlDB.QueryRowContext(ctx, `
		SELECT secret, enabled, backup_codes FROM user_mfa
		WHERE user_id = $1 AND enabled = TRUE`, userID).Scan(&s.Secret, &s.Enabled, &codesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		a.mfaMu.Lock()
		delete(a.mfaSettings, userID)
		a.mfaMu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("read mfa settings: %w", err)
	}
	if codesJSON.Valid && codesJSON.String != "" {
		_ = json.Unmarshal([]byte(codesJSON.String), &s.BackupCodes)
	}
	a.mfaMu.Lock()
	a.mfaSettings[userID] = &s
	a.mfaMu.Unlock()
	return nil
}
