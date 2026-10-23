package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/FairForge/vaultaire/internal/flags"
	"github.com/FairForge/vaultaire/internal/translog"
	"github.com/FairForge/vaultaire/internal/translog/adminread"
	"github.com/FairForge/vaultaire/internal/translog/logstore"
)

// The transparency log's own switches are in the log (Stage 1a Prompt 34a,
// RP44-2; docs/ARCHITECTURE_CONTRACT.md rule 7 — switching off the record of
// admin reads is itself an act that widens our access). Every write of a
// translog_* feature-flag row — set or clear, global or per tenant, from the
// admin API or the dashboard (both go through flags.Service, whose write
// guard this is) — appends a log_policy entry BEFORE the row is written;
// an append that fails refuses the change and nothing is written (fail
// closed). A tenant's row carries a commitment under the tenant's salt, so
// the tenant finds it in its own view. A row written by SQL on the box
// leaves no entry (docs/TRANSPARENCY.md § What the log does not see).

// useFlags makes fl the server's flags service, with the server's write
// guard on it (NewServer, and the tests that build a server by hand).
func (s *Server) useFlags(fl *flags.Service) {
	s.flags = fl
	if fl != nil {
		fl.SetWriteGuard(s.guardTranslogFlagWrite)
	}
}

// guardTranslogFlagWrite appends the log_policy entry of a translog_* row
// write; any other flag passes. A tenant row must name a tenant that exists
// (adminread.ErrNoSuchTenant otherwise: the commitment needs its salt).
func (s *Server) guardTranslogFlagWrite(ctx context.Context, c flags.Change) error {
	if !isPolicyFlag(c.Key) {
		return nil
	}
	tenant := ""
	if c.TenantID != flags.GlobalTenant {
		ok, err := adminread.TenantExists(ctx, s.db, c.TenantID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("flag %s for %q: %w", c.Key, c.TenantID, adminread.ErrNoSuchTenant)
		}
		tenant = c.TenantID
	}
	if s.adminReads == nil {
		return errors.New("the transparency log is not configured")
	}
	action := translog.PolicyActionSet
	if c.Clear {
		action = translog.PolicyActionClear
	}
	_, err := s.adminReads.RecordPolicy(ctx, c.Key, action, c.Enabled && !c.Clear, tenant)
	return err
}

func isPolicyFlag(key string) bool {
	for _, f := range translog.PolicyFlags() {
		if f == key {
			return true
		}
	}
	return false
}

// flagWriteError answers a refused flag write with its code, true; false =
// not one of the refusals (the caller's 500).
func flagWriteError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, flags.ErrNotExemptible):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "flag_not_exemptible"})
	case errors.Is(err, flags.ErrGlobalOnly):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "flag_global_only"})
	case errors.Is(err, adminread.ErrNoSuchTenant):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tenant_not_found"})
	case errors.Is(err, logstore.ErrLogFrozen):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "log_frozen"})
	case errors.Is(err, flags.ErrChangeRefused):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "flag_change_not_logged"})
	default:
		return false
	}
	return true
}
