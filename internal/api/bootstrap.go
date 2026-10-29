package api

// The local server's first-boot tenant (WP-DX-1 PR C, Stage 1a Prompt 50;
// docs/guides/local.md): with VAULTAIRE_BOOTSTRAP_EMAIL set — the Docker
// image's compose file sets it, prod never does — the first boot makes one
// account (registration's four tables, a random password) and prints its
// key pair and a one-time password ONCE to stdout; every later boot finds
// the account and prints no secret. The keys and the password exist only in
// that first log: sign in with the password and rotate both in the
// dashboard. It runs only with STORAGE_MODE=local (RP80-7): a server with a
// real backend never makes an account, nor prints a secret to its journal.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/auth"
)

// errBootstrapNotLocal: the first-boot account is the local server's only.
var errBootstrapNotLocal = errors.New("the first-boot account is made only with STORAGE_MODE=local")

// bootstrapTenant makes the account email when it does not exist and
// prints its pair and password to out; made reports whether it was made
// now. storageMode is STORAGE_MODE: anything but "local" is refused.
func bootstrapTenant(ctx context.Context, svc *auth.AuthService, logger *zap.Logger, email, storageMode string, out io.Writer) (bool, error) {
	if storageMode != "local" {
		return false, fmt.Errorf("first-boot account %s (STORAGE_MODE=%q): %w", email, storageMode, errBootstrapNotLocal)
	}
	if existing, err := svc.GetUserByEmail(ctx, email); err == nil && existing != nil {
		_, _ = fmt.Fprintf(out, "stored: the first-boot account %s already exists — its keys and password were printed at the first boot (rotate them in the dashboard)\n", email)
		return false, nil
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return false, err
	}
	password := hex.EncodeToString(b[:])
	_, _, key, err := svc.CreateUserWithTenant(ctx, email, password, "local")
	if err != nil {
		if errors.Is(err, auth.ErrSignupsDisabled) {
			return false, fmt.Errorf("first-boot account: signups are closed on this server (SIGNUPS_ENABLED=false): %w", err)
		}
		return false, fmt.Errorf("first-boot account: %w", err)
	}
	_, _ = fmt.Fprintf(out, "stored: first-boot account %s made. Its key pair and dashboard password (shown once, never logged again):\n  AWS_ACCESS_KEY_ID=%s\n  AWS_SECRET_ACCESS_KEY=%s\n  password=%s\n",
		email, key.Key, key.Secret, password)
	logger.Info("first-boot account made", zap.String("email", email))
	return true, nil
}
