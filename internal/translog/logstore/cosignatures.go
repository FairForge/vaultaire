package logstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/FairForge/vaultaire/internal/translog"
)

// The collected cosignatures (K4c-2, migration 105, translog_cosignatures):
// what witnesses answered, verified by the caller against each witness's
// pinned key before it is kept, and appended to the checkpoint the read
// side serves.

// AddCosignature keeps one verified cosignature line of the checkpoint of
// size (which must be stored). A line already kept is not kept twice.
func (s *Store) AddCosignature(ctx context.Context, size int64, witness string, c translog.Cosignature) error {
	if !strings.HasPrefix(c.Line, "— ") || !strings.HasSuffix(c.Line, "\n") {
		return errors.New("translog: a cosignature line is \"— <name> <base64>\\n\"")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO translog_cosignatures (checkpoint_size, witness, key_id, line, cosigned_at)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (checkpoint_size, md5(line)) DO NOTHING`,
		size, witness, c.KeyID(), c.Line, c.Time.UTC())
	if err != nil {
		return fmt.Errorf("translog: keep the cosignature of %d by %s: %w", size, witness, err)
	}
	return nil
}

// WitnessState is what the log holds from one witness key: the largest
// checkpoint it cosigned (0 = none) and the newest cosignature of that
// checkpoint (zero time = none).
func (s *Store) WitnessState(ctx context.Context, keyID string) (size int64, newest time.Time, err error) {
	var at sql.NullTime
	err = s.db.QueryRowContext(ctx, `SELECT checkpoint_size, cosigned_at FROM translog_cosignatures
		WHERE key_id = $1 ORDER BY checkpoint_size DESC, cosigned_at DESC LIMIT 1`, keyID).Scan(&size, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("translog: the cosignatures of %s: %w", keyID, err)
	}
	return size, at.Time, nil
}

// LatestCheckpointNote is the newest signed checkpoint as the read side
// serves it: the log's note with the newest cosignature line of every
// witness key appended (c2sp.org/signed-note: more signature lines), ordered
// by key so the bytes are stable between two collections.
func (s *Store) LatestCheckpointNote(ctx context.Context) (Stored, []byte, bool, error) {
	c, ok, err := s.LatestCheckpoint(ctx)
	if err != nil || !ok {
		return Stored{}, nil, ok, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT ON (key_id) line FROM translog_cosignatures
		WHERE checkpoint_size = $1 ORDER BY key_id, cosigned_at DESC, received_at DESC`, c.Size)
	if err != nil {
		return Stored{}, nil, false, fmt.Errorf("translog: the cosignatures of %d: %w", c.Size, err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	b.WriteString(c.Note)
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return Stored{}, nil, false, fmt.Errorf("translog: the cosignatures of %d: %w", c.Size, err)
		}
		b.WriteString(line)
	}
	if err := rows.Err(); err != nil {
		return Stored{}, nil, false, fmt.Errorf("translog: the cosignatures of %d: %w", c.Size, err)
	}
	return c, []byte(b.String()), true, nil
}
