package api

// WP-R10-8 Part C (R10-24, R3-14): multipart parts are reserved. A part's
// bytes used to sit on the hub's staging disk unbilled until Complete — a
// free tenant could pin 50 GiB (MULTIPART_MAX_UPLOAD_BYTES) × N uploads for
// 48 h at no quota cost. Now:
//
//   - UploadPart reserves the part's declared bytes, less what the part
//     number already holds, on the upload's floor BEFORE the first byte is
//     staged (403 QuotaExceeded, nothing staged), and records it on the
//     upload row (multipart_uploads.reserved_bytes, 091) in the same
//     transaction;
//   - the part's install settles it to the measured size, under the
//     upload row's lock: a re-upload of a part number holds the difference,
//     a failed or cut part gives its reservation back;
//   - Complete takes what the upload holds into the object's bill inside
//     its commit (writeBill.Held) — parts not listed are released there;
//   - Abort, the reaper and the account erasure release exactly what the
//     row says (endUploadReleasing). A restart loses nothing.
//
// What an upload holds is billed bytes until it ends: the reconcile counts
// an active upload's reserved_bytes (usage/billed.go), so a reconcile during
// an upload never frees its staged parts.
//
// Lock order (blob_flip.go): the upload row (FOR UPDATE) → tenant_quotas →
// tenant_floor_quotas — the same as a Complete's commit, after its key and
// row locks.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/FairForge/vaultaire/internal/usage"
)

// errUploadEnded: the upload is no longer active (aborted, reaped,
// completed) — its holding was released or taken when it ended.
var errUploadEnded = errors.New("multipart upload is not active")

// errPartOverQuota: the part does not fit in the tenant's quota.
var errPartOverQuota = errors.New("part over quota")

// partOverQuota is errPartOverQuota naming the cap that refused (WP-KEY-1:
// the upload's key, its bucket, the tenant or the pool).
type partOverQuota struct{ cap string }

func (e *partOverQuota) Error() string        { return errPartOverQuota.Error() + " (" + e.cap + ")" }
func (e *partOverQuota) Is(target error) bool { return target == errPartOverQuota }

// refusedCap is the cap a part refusal names ("" = the tenant's).
func refusedCap(err error) string {
	var p *partOverQuota
	if errors.As(err, &p) {
		return p.cap
	}
	return usage.CapOf(err)
}

// uploadWriterKey is the key that created uploadID ("" = none, or no
// database): what its Complete's object is counted to.
func uploadWriterKey(ctx context.Context, db *sql.DB, uploadID string) string {
	if db == nil {
		return ""
	}
	var k string
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(written_by_key::text, '') FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(&k)
	return k
}

// uploadHolder is who holds an upload's parts besides the tenant: the key
// that created it and its bucket (WP-KEY-1), read under the row's lock.
func uploadHolder(ctx context.Context, tx *sql.Tx, uploadID string) (usage.Holder, error) {
	var h usage.Holder
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(written_by_key::text, ''), bucket FROM multipart_uploads WHERE upload_id = $1`,
		uploadID).Scan(&h.KeyID, &h.Bucket); err != nil {
		return usage.Holder{}, fmt.Errorf("read upload %s's holder: %w", uploadID, err)
	}
	return h, nil
}

// lockActiveUpload locks the upload row and returns what a part's
// reservation needs: the floor its parts are held on and what part
// partNumber holds now (0 when none). The part's size is read in a second
// statement, after the lock: a subquery of the locking statement reads the
// snapshot taken before it waited, so two attempts at one part number each
// missed the part the other had just installed and the upload kept both
// reservations (Stage 1a Prompt 7, Part 0).
func lockActiveUpload(ctx context.Context, tx *sql.Tx, tenantID, uploadID string, partNumber int) (floor string, partBytes int64, edge bool, err error) {
	var status string
	err = tx.QueryRowContext(ctx, `
		SELECT status, edge, `+uploadHoldingFloorSQL+`
		FROM multipart_uploads u
		WHERE u.upload_id = $1 AND u.tenant_id = $2
		FOR UPDATE`, uploadID, tenantID).Scan(&status, &edge, &floor)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", 0, false, errUploadEnded
	case err != nil:
		return "", 0, false, fmt.Errorf("lock upload %s: %w", uploadID, err)
	case status != "active":
		return "", 0, false, errUploadEnded
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT size_bytes FROM multipart_parts WHERE upload_id = $1 AND part_number = $2), 0)`,
		uploadID, partNumber).Scan(&partBytes); err != nil {
		return "", 0, false, fmt.Errorf("read part %d of upload %s: %w", partNumber, uploadID, err)
	}
	return floor, partBytes, edge, nil
}

// uploadIsCodec reads the upload's codec mark inside the caller's
// transaction, which holds the upload row's lock (lockActiveUpload).
func uploadIsCodec(ctx context.Context, tx *sql.Tx, uploadID string) (bool, error) {
	var codec bool
	if err := tx.QueryRowContext(ctx, `SELECT codec FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(&codec); err != nil {
		return false, fmt.Errorf("read upload %s codec: %w", uploadID, err)
	}
	return codec, nil
}

// uploadHasStagedParts reports, inside the caller's transaction (which holds
// the upload row's lock), whether a not-yet-edge upload has a staged part:
// a part row, or a staged reservation in flight (reserved_bytes on the row
// before the part's install writes it — only reserveUploadPart holds bytes
// on an upload that is neither edge nor codec).
func uploadHasStagedParts(ctx context.Context, tx *sql.Tx, uploadID string) (bool, error) {
	var parts int
	var reserved int64
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM multipart_parts WHERE upload_id = $1), COALESCE(reserved_bytes, 0)
		FROM multipart_uploads WHERE upload_id = $1`, uploadID).Scan(&parts, &reserved); err != nil {
		return false, fmt.Errorf("count the upload's parts: %w", err)
	}
	return parts > 0 || reserved > 0, nil
}

// uploadHoldingFloorSQL is the floor an upload's holding is on, over the
// alias u (usage.UploadHoldingFloor: downstairs for an upload created before
// 091, which holds nothing).
const uploadHoldingFloorSQL = usage.UploadHoldingFloor

// reserveUploadPart reserves a part of declared bytes before it is staged:
// the declared size less what the part number holds now (a re-upload holds
// the difference). Returns what it reserved (settleUploadPart and
// releaseUploadPart take it).
func reserveUploadPart(ctx context.Context, db *sql.DB, tenantID, uploadID string, partNumber int, declared int64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin part reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	floor, held, edge, err := lockActiveUpload(ctx, tx, tenantID, uploadID, partNumber)
	if err != nil {
		return 0, err
	}
	if edge {
		return 0, errUploadEdge // a staged part on an edge upload (WP-EDGE-1): one upload is one path
	}
	if codec, cErr := uploadIsCodec(ctx, tx, uploadID); cErr != nil {
		return 0, cErr
	} else if codec {
		return 0, errUploadCodec // a staged part on a codec upload (WP-EDGE-2): one upload is one path
	}
	up := declared - held
	if up <= 0 {
		return 0, nil // the part number already holds as much
	}
	// The upload row first (already locked), the ledger last: a refusal
	// rolls both back.
	if _, err := tx.ExecContext(ctx, `UPDATE multipart_uploads SET reserved_bytes = reserved_bytes + $2 WHERE upload_id = $1`,
		uploadID, up); err != nil {
		return 0, fmt.Errorf("record the part reservation: %w", err)
	}
	h, err := uploadHolder(ctx, tx, uploadID)
	if err != nil {
		return 0, err
	}
	res, err := usage.ReserveTxFor(ctx, tx, tenantID, floor, up, h)
	if err != nil {
		return 0, err
	}
	if !res.OK {
		return 0, &partOverQuota{cap: res.Cap}
	}
	return up, tx.Commit()
}

// settleUploadPart installs a staged part (install: the rename over the
// part path, run under the upload row's lock) and settles its reservation
// to the measured size in one transaction with the part row: the upload
// then holds the part's bytes exactly — measured, less what the part number
// held before. A part that grew past its reservation and no longer fits is
// refused: errPartOverQuota, its reservation given back, nothing installed.
func settleUploadPart(ctx context.Context, db *sql.DB, tenantID, uploadID string, partNumber int,
	reserved, measured int64, etag, sealedMD5 string, install func() error) error {
	return settleUploadPartWith(ctx, db, tenantID, uploadID, partNumber, reserved, measured, etag, sealedMD5, "", install, false)
}

// settleUploadPartWith is settleUploadPart for both kinds of part: a staged
// one (backendETag "", viaEdge false — refused on an edge upload) and an
// edge one (backendETag the backend's, viaEdge true — refused on an upload
// that is not edge; WP-EDGE-1 Part A).
func settleUploadPartWith(ctx context.Context, db *sql.DB, tenantID, uploadID string, partNumber int,
	reserved, measured int64, etag, sealedMD5, backendETag string, install func() error, viaEdge bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin part install: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	floor, held, edge, err := lockActiveUpload(ctx, tx, tenantID, uploadID, partNumber)
	if err != nil {
		return err // ended: Abort/the reaper released everything, this part's reservation too
	}
	if edge != viaEdge {
		return releaseMismatchedPart(ctx, tx, tenantID, uploadID, floor, reserved, edge)
	}
	if !viaEdge {
		// The upload became codec while this part staged (the pre-check in
		// stageUploadPart is not under the lock): refused here, its
		// reservation given back — never a staged part beside chunk lists.
		codec, cErr := uploadIsCodec(ctx, tx, uploadID)
		if cErr != nil {
			return cErr
		}
		if codec {
			if err := giveBackPart(ctx, tx, tenantID, uploadID, floor, reserved); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit the part's release: %w", err)
			}
			return errUploadCodec
		}
	}
	delta := measured - held - reserved
	h, err := uploadHolder(ctx, tx, uploadID)
	if err != nil {
		return err
	}
	if delta > 0 {
		// A part that measured more than it reserved (no declared length,
		// R108-8): checked before the install, which may replace the part
		// number's previous file — so this one path holds the quota row
		// through the rename below.
		res, err := usage.ReserveTxFor(ctx, tx, tenantID, floor, delta, h)
		if err != nil {
			return err
		}
		if !res.OK {
			if err := giveBackPart(ctx, tx, tenantID, uploadID, floor, reserved); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit the part's release: %w", err)
			}
			return &partOverQuota{cap: res.Cap}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE multipart_uploads SET reserved_bytes = GREATEST(0, reserved_bytes + $2) WHERE upload_id = $1`,
		uploadID, delta); err != nil {
		return fmt.Errorf("settle the part reservation: %w", err)
	}
	if err := install(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes, sealed_md5, backend_etag)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		ON CONFLICT (upload_id, part_number) DO UPDATE SET
			etag         = EXCLUDED.etag,
			size_bytes   = EXCLUDED.size_bytes,
			sealed_md5   = EXCLUDED.sealed_md5,
			backend_etag = EXCLUDED.backend_etag,
			created_at   = NOW()`, uploadID, partNumber, etag, measured, sealedMD5, backendETag); err != nil {
		return fmt.Errorf("record part metadata: %w", err)
	}
	// What the part holds less than it reserved goes back last: the quota
	// row is held from here to COMMIT only (ledger_tx.go).
	if delta < 0 {
		if err := usage.ApplyLedgerTx(ctx, tx, tenantID, usage.LedgerChange{Total: delta, Floors: map[string]int64{floor: delta},
			Holders: []usage.HolderMove{{Holder: h, Bytes: delta}}}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// releaseMismatchedPart gives a part's reservation back when the part
// arrived on the wrong path — staged on an edge upload (errUploadEdge), or
// at the edge on an upload that holds staged parts (errUploadStaged) — and
// commits the release: the ledger is the transaction's last statement.
func releaseMismatchedPart(ctx context.Context, tx *sql.Tx, tenantID, uploadID, floor string, reserved int64, edge bool) error {
	if err := giveBackPart(ctx, tx, tenantID, uploadID, floor, reserved); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the part's release: %w", err)
	}
	if edge {
		return errUploadEdge
	}
	return errUploadStaged
}

// releaseUploadPart gives back a part's reservation when the part was not
// installed (a cut body, a refused size, a failed write). Nothing to give
// when the upload has ended: its end released everything it held.
func releaseUploadPart(ctx context.Context, db *sql.DB, tenantID, uploadID string, partNumber int, reserved int64) error {
	if reserved <= 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin part release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	floor, _, _, err := lockActiveUpload(ctx, tx, tenantID, uploadID, partNumber)
	if errors.Is(err, errUploadEnded) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := giveBackPart(ctx, tx, tenantID, uploadID, floor, reserved); err != nil {
		return err
	}
	return tx.Commit()
}

// giveBackPart releases n held bytes of the upload, inside tx (the upload
// row locked).
func giveBackPart(ctx context.Context, tx *sql.Tx, tenantID, uploadID, floor string, n int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE multipart_uploads SET reserved_bytes = GREATEST(0, reserved_bytes - $2) WHERE upload_id = $1`,
		uploadID, n); err != nil {
		return fmt.Errorf("release the part reservation: %w", err)
	}
	h, err := uploadHolder(ctx, tx, uploadID)
	if err != nil {
		return err
	}
	return usage.ApplyLedgerTx(ctx, tx, tenantID, usage.LedgerChange{Total: -n, Floors: map[string]int64{floor: -n},
		Holders: []usage.HolderMove{{Holder: h, Bytes: -n}}})
}

// endUploadReleasing ends an active upload (status aborted) and releases
// what it holds, in one transaction: Abort (the tenant's own, keyed by its
// bucket and key), the reaper and the account erasure. false = it was not
// active (nothing changed).
func endUploadReleasing(ctx context.Context, db *sql.DB, uploadID, tenantID, bucket, key string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin abort: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var owner, floor string
	var held int64
	var h usage.Holder
	err = tx.QueryRowContext(ctx, `
		SELECT u.tenant_id, u.reserved_bytes, `+uploadHoldingFloorSQL+`, COALESCE(u.written_by_key::text, ''), u.bucket
		FROM multipart_uploads u
		WHERE u.upload_id = $1 AND u.status = 'active'
		  AND ($2 = '' OR u.tenant_id = $2) AND ($3 = '' OR u.bucket = $3) AND ($4 = '' OR u.object_key = $4)
		FOR UPDATE`, uploadID, tenantID, bucket, key).Scan(&owner, &held, &floor, &h.KeyID, &h.Bucket)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock upload %s: %w", uploadID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE multipart_uploads SET status = 'aborted', reserved_bytes = 0 WHERE upload_id = $1`, uploadID); err != nil {
		return false, fmt.Errorf("abort upload %s: %w", uploadID, err)
	}
	if held > 0 {
		if err := usage.ApplyLedgerTx(ctx, tx, owner, usage.LedgerChange{Total: -held, Floors: map[string]int64{floor: -held},
			Holders: []usage.HolderMove{{Holder: h, Bytes: -held}}}); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit abort: %w", err)
	}
	return true, nil
}

// uploadHeld is a Complete's writeBill.Held: what the upload holds, read
// under its row's lock inside the commit (markCompleted has locked it) and
// cleared — the commit's settle turns it into the object's bill, on the
// floor the parts were held on (the same expression the parts and the
// reconcile use). An upload created before 091 holds nothing: the object's
// size is charged whole.
func uploadHeld(uploadID string) func(ctx context.Context, tx *sql.Tx) (string, int64, error) {
	return func(ctx context.Context, tx *sql.Tx) (string, int64, error) {
		var held int64
		var f string
		if err := tx.QueryRowContext(ctx, `
			SELECT u.reserved_bytes, `+uploadHoldingFloorSQL+` FROM multipart_uploads u WHERE u.upload_id = $1 FOR UPDATE`,
			uploadID).Scan(&held, &f); err != nil {
			return "", 0, fmt.Errorf("read the upload's holding: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE multipart_uploads SET reserved_bytes = 0 WHERE upload_id = $1`, uploadID); err != nil {
			return "", 0, fmt.Errorf("clear the upload's holding: %w", err)
		}
		return f, held, nil
	}
}
