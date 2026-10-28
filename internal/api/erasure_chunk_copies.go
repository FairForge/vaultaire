package api

// Stage b1b of the account deletion (RP76-3): a codec tenant's chunk copies
// on the mirror legs. A copy of a sealed chunk sits at the chunk's one
// address under the shared container (`_global/_chunks/<tenant>/…`) on its
// leg, named by its blob_copies row alone: the sweep never walks _global
// and account.EraseRows deletes the rows, so after the erase nothing would
// find it again. The runner deletes each copy on its leg THROUGH the rows,
// before the row erase; a leg that is not registered, whose breaker is open
// or that refuses a delete defers the tenant — as the sweep does.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FairForge/vaultaire/internal/engine"
)

// eraseChunkCopies deletes the tenant's chunk copies (blob_copies rows with
// no bucket) on their legs, then their rows; te counts the deletes.
func (r *AccountDeletionRunner) eraseChunkCopies(ctx context.Context, tenantID string, te *TenantErasure) error {
	ce, ok := r.eng.(*engine.CoreEngine)
	if !ok {
		return errors.New("the engine does not expose its drivers: no chunk copy can be deleted")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT storage_key, leg, shard_index FROM blob_copies WHERE tenant_id = $1 AND bucket = ''`, tenantID)
	if err != nil {
		return fmt.Errorf("read the chunk copies: %w", err)
	}
	var copies []blobCopy
	for rows.Next() {
		c := blobCopy{TenantID: tenantID}
		if err := rows.Scan(&c.StorageKey, &c.Leg, &c.Index); err != nil {
			_ = rows.Close()
			return err
		}
		copies = append(copies, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// One failure line per leg: a leg that is down fails every copy on it.
	var failed []string
	seen := map[string]bool{}
	fail := func(leg, why string) {
		if !seen[leg] {
			seen[leg] = true
			failed = append(failed, leg+": "+why)
		}
	}
	for _, c := range copies {
		if err := ctx.Err(); err != nil {
			return err
		}
		drv, ok := ce.GetDriver(c.Leg)
		if !ok {
			fail(c.Leg, "not registered")
			continue
		}
		if err := ce.CanAsk(c.Leg); err != nil {
			fail(c.Leg, err.Error())
			continue
		}
		cctx, container, artifact := c.address(ctx)
		dctx, cancel := context.WithTimeout(cctx, blobCopiesCallTimeout)
		err := drv.Delete(dctx, container, artifact)
		cancel()
		if err != nil && !isObjectMissingErr(err) {
			fail(c.Leg, err.Error())
			continue
		}
		if _, err := r.db.ExecContext(ctx, `DELETE FROM blob_copies WHERE tenant_id = $1 AND storage_key = $2 AND leg = $3 AND shard_index = $4`,
			tenantID, c.StorageKey, c.Leg, c.Index); err != nil {
			return err
		}
		te.ChunkCopiesDeleted++
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}
