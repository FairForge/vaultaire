package api

// A tenant's residency (WP-ACCT-1, Stage 1a Prompt 53 PR B; docs/ACCOUNTS.md
// "Residency"; D-STRIPE-30): tenants.residency — eu, uk or us; NULL =
// anywhere, every tenant until one is set — is where the tenant's bytes may
// be STORED AT REST. Bytes in transit through an edge colo are not storage.
//
//   - set at creation (a platform's child names it, or inherits its
//     parent's — auth.CreateChildTenant; a registration has none), never
//     changed, except by an admin while every bucket of the tenant's tree
//     complies (PUT /api/v1/admin/tenants/{id}/residency);
//   - a bucket is created only in a region of the residency's jurisdiction
//     (drivers.RegionJurisdiction: eu-west-3 is London, the UK), and a
//     create that names no region gets the first enabled region inside it
//     (bucket_create.go);
//   - the mirror leg (edge_mirror.go) and the parity leg (vault_parity.go)
//     are only legs of that jurisdiction: a leg whose jurisdiction we do not
//     know is outside every residency, and no leg inside means no second
//     copy — never one outside.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/audit"
	"github.com/FairForge/vaultaire/internal/auth"
	"github.com/FairForge/vaultaire/internal/drivers"
)

// tenantResidency is the tenant's residency ("" = anywhere).
func (s *Server) tenantResidency(ctx context.Context, tenantID string) (string, error) {
	if s.db == nil || tenantID == "" {
		return "", nil
	}
	var r sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT residency FROM tenants WHERE id = $1`, tenantID).Scan(&r)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the residency of %s: %w", tenantID, err)
	}
	return r.String, nil
}

// regionInside reports whether a bucket in region stores inside residency
// ("" = anywhere).
func regionInside(region, residency string) bool {
	return residency == "" || drivers.RegionJurisdiction(region) == residency
}

// firstRegionInside is the region a create that names none gets under
// residency: the deployment's default region when it is inside, else the
// first enabled region inside it (by id); "" when none is.
func firstRegionInside(residency string) string {
	if def := drivers.IDriveDefaultRegion(os.Getenv); regionInside(def, residency) {
		return def
	}
	var ids []string
	for id := range drivers.IDriveRegions {
		if regionInside(id, residency) && drivers.IDriveRegionAvailable(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// errBucketsOutside: a residency an admin asked for while a bucket of the
// tenant's tree is outside it.
var errBucketsOutside = errors.New("a bucket of the tenant's tree is outside the residency")

// setTenantResidency writes the residency of tenantID and of every tenant of
// its tree that has none of its own or the same one, in one transaction:
// refused while a bucket of the tree is outside it (errBucketsOutside), or
// while a tenant of the tree holds another residency (auth.ErrResidencyWidens).
// "" clears it on the tenant alone.
func (s *Server) setTenantResidency(ctx context.Context, tenantID, residency string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set residency of %s: %w", tenantID, err)
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.ErrTenantNotFound
		}
		return fmt.Errorf("set residency of %s: %w", tenantID, err)
	}
	if residency == "" {
		if _, err := tx.ExecContext(ctx, `UPDATE tenants SET residency = NULL WHERE id = $1`, tenantID); err != nil {
			return fmt.Errorf("set residency of %s: %w", tenantID, err)
		}
		return tx.Commit()
	}
	var other int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants t WHERE `+residencyTree+` AND t.residency IS NOT NULL AND t.residency <> $2`,
		tenantID, residency).Scan(&other); err != nil {
		return fmt.Errorf("set residency of %s: %w", tenantID, err)
	}
	if other > 0 {
		return auth.ErrResidencyWidens
	}
	regions, err := treeBucketRegions(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	def := drivers.IDriveDefaultRegion(os.Getenv)
	for _, region := range regions {
		if region == "" {
			region = def
		}
		if !regionInside(region, residency) {
			return errBucketsOutside
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenants t SET residency = $2 WHERE `+residencyTree, tenantID, residency); err != nil {
		return fmt.Errorf("set residency of %s: %w", tenantID, err)
	}
	if err := audit.RecordTx(ctx, tx, audit.Entry{TenantID: tenantID, EventType: "admin", Action: "admin.residency_set",
		Resource: "tenant:" + tenantID, Metadata: map[string]any{"residency": residency}}); err != nil {
		return fmt.Errorf("set residency of %s: %w", tenantID, err)
	}
	return tx.Commit()
}

// residencyTree is "t is $1 or a tenant under it".
const residencyTree = `(t.id = $1 OR ` + auth.DescendantT1 + `)`

// treeBucketRegions is every distinct bucket region of tenantID's tree
// ("" for a bucket that names none).
func treeBucketRegions(ctx context.Context, tx *sql.Tx, tenantID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT COALESCE(b.region, '') FROM buckets b JOIN tenants t ON t.id = b.tenant_id WHERE `+residencyTree, tenantID)
	if err != nil {
		return nil, fmt.Errorf("the buckets of %s: %w", tenantID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var region string
		if err := rows.Scan(&region); err != nil {
			return nil, fmt.Errorf("the buckets of %s: %w", tenantID, err)
		}
		out = append(out, region)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the buckets of %s: %w", tenantID, err)
	}
	return out, nil
}

// handleAdminSetResidency is PUT /api/v1/admin/tenants/{tenant_id}/residency
// {"residency": "eu"|"uk"|"us"|null}.
func (s *Server) handleAdminSetResidency(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenant_id")
	var req struct {
		Residency *string `json:"residency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_json", `the body must be {"residency": "eu"|"uk"|"us"|null}`, "residency")
		return
	}
	residency := ""
	if req.Residency != nil {
		residency = *req.Residency
	}
	if residency != "" && !auth.ValidResidency(residency) {
		writeManagementError(w, ErrTypeInvalidRequest, "invalid_residency", auth.ErrBadResidency.Error(), "residency")
		return
	}
	err := s.setTenantResidency(r.Context(), tenantID, residency)
	switch {
	case errors.Is(err, auth.ErrTenantNotFound):
		writeManagementError(w, ErrTypeNotFound, "tenant_not_found", "no such tenant", "tenant_id")
		return
	case errors.Is(err, errBucketsOutside):
		writeManagementError(w, ErrTypeConflict, "buckets_outside_residency",
			"a bucket of the tenant (or of a tenant under it) is in a region outside "+residency+": move or delete it first", "residency")
		return
	case errors.Is(err, auth.ErrResidencyWidens):
		writeManagementError(w, ErrTypeConflict, "children_outside_residency", "a tenant under it holds another residency", "residency")
		return
	case err != nil:
		s.logger.Error("admin: set residency", zap.Error(err))
		writeManagementError(w, ErrTypeAPI, "internal_error", "failed to set the residency", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "tenant", "id": tenantID, "residency": req.Residency, "request_id": getRequestID(w)})
}
