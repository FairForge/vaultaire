package api

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/FairForge/vaultaire/internal/account"
	"github.com/FairForge/vaultaire/internal/recovery"
	"github.com/FairForge/vaultaire/internal/recovery/client"
)

// The account erasure at the recovery realms (RP52-3, the WP-P37 review).
// A realm keys a user's record — key shares, the encrypted secret — by the
// opaque subject HMAC(recovery_subject_key, user id) (recovery_tokens.go),
// so before the row erase the runner mints each realm its own token for the
// subject (mint's claims, the same 10 min life) and sends a Delete through
// internal/recovery/client. A Delete needs no forward secrecy, so it rides
// the Noise handshake (HTTPTransport). A realm answers a Delete of nothing
// with OK (realm.go), so a realm that never held a record, or one an
// earlier run already reached, counts as done. A realm that cannot be
// reached, answers 5xx or Unavailable, or refuses the token defers the
// tenant, as a backend the sweep cannot list does: never account.erased
// while a realm may hold the record.
//
// One user per erasure: account.Due is one user and EraseRows deletes that
// user only, so "every user of the tenant" is d.UserID.

const (
	// recoveryEraseTimeout bounds one realm's Delete, the transport's
	// retries included.
	recoveryEraseTimeout = 30 * time.Second
	// recoveryTokenGrace is added to a token's life when the runner asks
	// whether one may still be used: the realms accept exp plus their clock
	// skew (5 s, cmd/recovery-realm/auth.go).
	recoveryTokenGrace = time.Minute
)

// The outcomes of one realm's Delete, as vaultaire_account_deletion_realms_total
// counts them (RP54-3): deleted; invalid_auth = the realm refused the token (a
// rotated key, or its clock behind ours past its 5 s leeway on nbf); unavailable
// = it answered without confirming (5xx, Unavailable, rate limited, an answer
// that is not one); unreachable = no answer at all (a dial that failed, nothing
// back within the deadline).
const (
	realmDeleted     = "deleted"
	realmInvalidAuth = "invalid_auth"
	realmUnavailable = "unavailable"
	realmUnreachable = "unreachable"
)

var accountDeletionRealms = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "vaultaire_account_deletion_realms_total",
	Help: "Recovery-realm Deletes sent by the account-deletion runner, by realm and outcome (deleted, invalid_auth, unavailable, unreachable).",
}, []string{"realm", "outcome"})

// initRealmSeries starts every configured realm's outcomes at 0, so the rule's
// increase sees the first refusal after a restart (RP14-9).
func (v *recoveryVault) initRealmSeries() {
	if v == nil {
		return
	}
	for _, r := range v.realms {
		for _, o := range []string{realmDeleted, realmInvalidAuth, realmUnavailable, realmUnreachable} {
			accountDeletionRealms.WithLabelValues(r.ID, o)
		}
	}
}

// realmErasure is one user's Delete across the configured realms.
type realmErasure struct {
	deleted  int
	deferred []string // "<realm id>: <why>"
}

// clientRealm is the realm as internal/recovery/client is configured with it.
func (r recoveryRealm) clientRealm() (client.Realm, error) {
	id, err := recovery.ParseRealmID(r.ID)
	if err != nil {
		return client.Realm{}, fmt.Errorf("realm id: %w", err)
	}
	pub, err := hex.DecodeString(r.PublicKey)
	if err != nil {
		return client.Realm{}, fmt.Errorf("realm public key: %w", err)
	}
	return client.Realm{ID: id, Address: r.Address, PublicKey: pub}, nil
}

// realmOutcome names what a realm's Delete came to. ctx is the caller's:
// once it is done the failure is ours (the tenant's deadline, a shutdown),
// never the realm's — "" is counted as nothing; so is a realm token we could
// not mint (client.ErrTokenMint, RP56-6).
func realmOutcome(ctx context.Context, err error) string {
	var urlErr *url.Error
	var netErr net.Error
	switch {
	case err == nil:
		return realmDeleted
	case ctx.Err() != nil:
		return ""
	case errors.Is(err, client.ErrTokenMint):
		// Our own Token() failed: the realm was never asked (RP56-6). It
		// still defers the tenant, with the reason.
		return ""
	case errors.Is(err, client.ErrInvalidAuth):
		return realmInvalidAuth
	case errors.As(err, &urlErr), errors.As(err, &netErr), errors.Is(err, context.DeadlineExceeded):
		// http.Client.Do's error, a body cut by the network, or the Delete's
		// own 30 s: nothing came back.
		return realmUnreachable
	default:
		return realmUnavailable
	}
}

// eraseUser sends a Delete for userID's subject to every configured realm,
// each on its own deadline. httpc nil = a client with a 15 s timeout.
//
// silent is the run's record of realms that gave no answer at all (RP54-4):
// a realm in it is not asked again — the tenant is deferred with its reason —
// and one whose Delete is unreachable is added to it, so a dead realm costs
// one deadline per run, not one per due tenant. A realm that answered (a 5xx,
// a refused token) is asked for every tenant. nil = ask every realm.
func (v *recoveryVault) eraseUser(ctx context.Context, userID string, httpc *http.Client, silent map[string]string) realmErasure {
	if httpc == nil {
		httpc = &http.Client{Timeout: 15 * time.Second}
	}
	sub := v.subject(userID)
	var out realmErasure
	configured := map[recovery.RealmID]recoveryRealm{}
	targets := make([]client.Realm, 0, len(v.realms))
	for _, r := range v.realms {
		cr, err := r.clientRealm()
		if err != nil {
			out.deferred = append(out.deferred, r.ID+": "+err.Error())
			continue
		}
		configured[cr.ID] = r
		targets = append(targets, cr)
	}
	tr := &client.HTTPTransport{HTTP: httpc, Token: func(_ context.Context, rm client.Realm) (string, error) {
		r, ok := configured[rm.ID]
		if !ok {
			return "", fmt.Errorf("realm %s is not configured", rm.ID)
		}
		return r.token(sub, v.now())
	}}
	for _, cr := range targets {
		id := cr.ID.String()
		if why, ok := silent[id]; ok {
			out.deferred = append(out.deferred, id+": not asked, no answer earlier in this run ("+why+")")
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, recoveryEraseTimeout)
		err := client.Delete(rctx, client.Config{Realms: []client.Realm{cr}, RegisterThreshold: 1, RecoverThreshold: 1, Transport: tr})
		cancel()
		outcome := realmOutcome(ctx, err)
		if outcome != "" {
			accountDeletionRealms.WithLabelValues(id, outcome).Inc()
		}
		if err != nil {
			if outcome == realmUnreachable && silent != nil {
				silent[id] = err.Error()
			}
			out.deferred = append(out.deferred, id+": "+err.Error())
			continue
		}
		out.deleted++
	}
	return out
}

// recentRealmToken returns when the newest realm token a realm may still
// accept was minted for userID (the recovery.tokens_issued audit row), if
// one was.
func recentRealmToken(ctx context.Context, db *sql.DB, userID string) (time.Time, bool, error) {
	var at sql.NullTime
	err := db.QueryRowContext(ctx, `
		SELECT MAX(timestamp) FROM audit_logs
		 WHERE user_id = $1 AND action = 'recovery.tokens_issued'
		   AND timestamp > NOW() - make_interval(secs => $2)`,
		userID, (recoveryTokenLifetime + recoveryTokenGrace).Seconds()).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the newest realm token: %w", err)
	}
	return at.Time, at.Valid, nil
}

// eraseRealms is the runner's stage b3: the user's record at every realm,
// while the user row exists. A token minted within its life (plus the
// realms' skew) could still Register after the Delete, so such a tenant
// waits for the next run. Not configured = nothing asked; RunOnce writes the
// note.
func (r *AccountDeletionRunner) eraseRealms(ctx context.Context, d account.Due, te *TenantErasure) error {
	if r.Recovery == nil {
		te.RealmsNotConfigured = true
		return nil
	}
	at, recent, err := recentRealmToken(ctx, r.db, d.UserID)
	if err != nil {
		return err
	}
	if recent {
		return fmt.Errorf("a realm token minted at %s may still register until %s — the realms are asked at the next run",
			at.UTC().Format(time.RFC3339), at.Add(recoveryTokenLifetime+recoveryTokenGrace).UTC().Format(time.RFC3339))
	}
	if r.realmsSilent == nil {
		r.realmsSilent = map[string]string{}
	}
	res := r.Recovery.eraseUser(ctx, d.UserID, r.realmHTTP, r.realmsSilent)
	te.RealmsDeleted, te.RealmsDeferred = res.deleted, len(res.deferred)
	if len(res.deferred) > 0 {
		return fmt.Errorf("%d of %d realms did not confirm the delete: %s",
			len(res.deferred), len(r.Recovery.realms), strings.Join(res.deferred, "; "))
	}
	return nil
}

// realmsNotConfiguredNote is the run's note when accounts were erased with no
// recovery vault configured.
func realmsNotConfiguredNote(n int) string {
	return fmt.Sprintf("recovery vault not configured: no realm was asked for %d erased account(s) — "+
		"this process minted no realm token, so no realm holds a record of its minting; a record minted while RECOVERY_* "+
		"was set and since unset stays at its realm, and the user id in account.erased with the same subject key "+
		"derives its subject, so it can still be deleted once the vault is configured again (RP53-1)", n)
}
