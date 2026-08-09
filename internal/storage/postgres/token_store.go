package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ziad/liveness-verifier/internal/enrollment"
)

// TokenStore persists single-use liveness tokens.
type TokenStore struct {
	db *DB
}

var _ enrollment.TokenStore = (*TokenStore)(nil)

// NewTokenStore wraps a pool as a token store.
func NewTokenStore(db *DB) (*TokenStore, error) {
	if db == nil || db.Pool == nil {
		return nil, errors.New("postgres: token store needs a pool")
	}
	return &TokenStore{db: db}, nil
}

// Save records a newly issued token.
func (s *TokenStore) Save(ctx context.Context, rec enrollment.TokenRecord) error {
	if len(rec.Hash) == 0 {
		return errors.New("postgres: token hash is required")
	}
	if rec.SessionID == "" {
		return errors.New("postgres: token needs the session that earned it")
	}

	const query = `
		INSERT INTO liveness_tokens (token_hash, session_id, issued_at, expires_at)
		VALUES ($1, $2, $3, $4)`

	if _, err := s.db.Pool.Exec(ctx, query, rec.Hash, rec.SessionID, rec.IssuedAt, rec.ExpiresAt); err != nil {
		// The hash is not in the message. It is the stored secret, and an error
		// string travels further than the row ever does.
		return fmt.Errorf("postgres: save liveness token: %w", err)
	}
	return nil
}

// Consume spends a token and returns the session it carried.
//
// One statement, not a read followed by a write. The UPDATE matches only rows
// that are unspent and unexpired, so two requests racing with the same token
// produce exactly one winner — the loser matches nothing and is refused. A
// check in Go would leave a window between the read and the write, which is
// precisely the race a replayed token is trying to win.
func (s *TokenStore) Consume(ctx context.Context, hash []byte, now time.Time) (string, error) {
	if len(hash) == 0 {
		return "", enrollment.ErrTokenInvalid
	}

	const query = `
		UPDATE liveness_tokens
		SET used_at = $2
		WHERE token_hash = $1
		  AND used_at IS NULL
		  AND expires_at > $2
		RETURNING session_id`

	var sessionID string
	err := s.db.Pool.QueryRow(ctx, query, hash, now).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown, already spent, and expired are one answer on purpose. Any
		// distinction here tells a caller something about a token they should
		// not have had in the first place.
		return "", enrollment.ErrTokenInvalid
	}
	if err != nil {
		return "", fmt.Errorf("postgres: consume liveness token: %w", err)
	}
	return sessionID, nil
}

// PurgeExpired removes tokens past their expiry.
//
// Spent tokens are removed too once expired: the record that a token was used
// belongs to the audit log, not to a table whose only job is to answer "may
// this be spent".
func (s *TokenStore) PurgeExpired(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.db.Pool.Exec(ctx, `DELETE FROM liveness_tokens WHERE expires_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("postgres: purge liveness tokens: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
