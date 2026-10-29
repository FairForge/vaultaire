package api

// The local server's first-boot tenant (WP-DX-1 PR C, Stage 1a Prompt 50;
// docs/guides/local.md): with VAULTAIRE_BOOTSTRAP_EMAIL set — the Docker
// image's compose file sets it, prod never does — the first boot makes one
// account (registration's four tables, a random password) and prints its
// key pair ONCE to stdout; every later boot finds the account and prints
// no secret. The keys exist only in that first log; rotate them in the
// dashboard to get new ones.

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

// bootstrapTenant makes the account email when it does not exist and
// prints its pair to out; made reports whether it was made now.
func bootstrapTenant(ctx context.Context, svc *auth.AuthService, logger *zap.Logger, email string, out io.Writer) (bool, error) {
	if existing, err := svc.GetUserByEmail(ctx, email); err == nil && existing != nil {
		_, _ = fmt.Fprintf(out, "stored: the first-boot account %s already exists — its keys were printed at the first boot (rotate them in the dashboard)\n", email)
		return false, nil
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return false, err
	}
	_, _, key, err := svc.CreateUserWithTenant(ctx, email, hex.EncodeToString(b[:]), "local")
	if err != nil {
		if errors.Is(err, auth.ErrSignupsDisabled) {
			return false, fmt.Errorf("first-boot account: signups are closed on this server (SIGNUPS_ENABLED=false): %w", err)
		}
		return false, fmt.Errorf("first-boot account: %w", err)
	}
	_, _ = fmt.Fprintf(out, "stored: first-boot account %s made. Its key pair (shown once, never logged again):\n  AWS_ACCESS_KEY_ID=%s\n  AWS_SECRET_ACCESS_KEY=%s\n",
		email, key.Key, key.Secret)
	logger.Info("first-boot account made", zap.String("email", email))
	return true, nil
}
