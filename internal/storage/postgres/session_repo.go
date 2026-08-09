package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/liveness"
)

// SessionRepo stores liveness sessions.
type SessionRepo struct {
	db *DB
}

var _ liveness.Repository = (*SessionRepo)(nil)

// NewSessionRepo wraps a pool as a session repository.
func NewSessionRepo(db *DB) (*SessionRepo, error) {
	if db == nil || db.Pool == nil {
		return nil, errors.New("postgres: session repository needs a pool")
	}
	return &SessionRepo{db: db}, nil
}

const sessionColumns = `
	id, nonce, state, challenges, current_index,
	created_at, expires_at, challenge_deadline,
	last_seq, recent_hashes, duplicate_streak,
	reference_embedding, progress, failure_reason, retries, version`

// Create inserts a new session.
func (r *SessionRepo) Create(ctx context.Context, s *liveness.Session) error {
	progress, err := json.Marshal(s.Progress)
	if err != nil {
		return fmt.Errorf("postgres: encode progress: %w", err)
	}

	const query = `
		INSERT INTO liveness_sessions (` + sessionColumns + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`

	_, err = r.db.Pool.Exec(ctx, query,
		string(s.ID), s.Nonce, string(s.State),
		challengesToText(s.Challenges), s.Current,
		s.CreatedAt, s.ExpiresAt, s.ChallengeDeadline,
		s.LastSeq, hashesToInt64(s.RecentHashes), s.DuplicateStreak,
		embeddingToFloat32(s.ReferenceEmbedding), progress, s.FailureReason, s.Retries, s.Version,
	)
	if err != nil {
		return fmt.Errorf("postgres: insert session %s: %w", s.ID, err)
	}
	return nil
}

// Get loads a session.
func (r *SessionRepo) Get(ctx context.Context, id liveness.SessionID) (*liveness.Session, error) {
	const query = `SELECT ` + sessionColumns + ` FROM liveness_sessions WHERE id = $1`

	s, err := scanSession(r.db.Pool.QueryRow(ctx, query, string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, liveness.ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: load session %s: %w", id, err)
	}
	return s, nil
}

// Update writes a session back, refusing to overwrite a newer one.
//
// The version in the WHERE clause is what makes concurrent frames safe. Two
// frames of one session can be in flight at once; without this the second write
// would silently discard whatever the first recorded, and a satisfied challenge
// would simply vanish.
func (r *SessionRepo) Update(ctx context.Context, s *liveness.Session) error {
	progress, err := json.Marshal(s.Progress)
	if err != nil {
		return fmt.Errorf("postgres: encode progress: %w", err)
	}

	const query = `
		UPDATE liveness_sessions SET
			state = $2, current_index = $3, challenge_deadline = $4,
			last_seq = $5, recent_hashes = $6, duplicate_streak = $7,
			reference_embedding = $8, progress = $9, failure_reason = $10,
			retries = $11, version = $12
		WHERE id = $1 AND version <= $12`

	tag, err := r.db.Pool.Exec(ctx, query,
		string(s.ID), string(s.State), s.Current, s.ChallengeDeadline,
		s.LastSeq, hashesToInt64(s.RecentHashes), s.DuplicateStreak,
		embeddingToFloat32(s.ReferenceEmbedding), progress, s.FailureReason,
		s.Retries, s.Version,
	)
	if err != nil {
		return fmt.Errorf("postgres: update session %s: %w", s.ID, err)
	}

	if tag.RowsAffected() == 0 {
		// Either the row is gone or somebody else wrote a newer version. Tell
		// them apart, because one is a bug and the other is ordinary
		// concurrency.
		var exists bool
		if err := r.db.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM liveness_sessions WHERE id = $1)`, string(s.ID),
		).Scan(&exists); err != nil {
			return fmt.Errorf("postgres: check session %s: %w", s.ID, err)
		}
		if !exists {
			return liveness.ErrSessionNotFound
		}
		return liveness.ErrVersionConflict
	}
	return nil
}

// DeleteExpired removes sessions that expired before the cutoff.
func (r *SessionRepo) DeleteExpired(ctx context.Context, before time.Time) (int, error) {
	const query = `DELETE FROM liveness_sessions WHERE expires_at < $1`

	tag, err := r.db.Pool.Exec(ctx, query, before)
	if err != nil {
		return 0, fmt.Errorf("postgres: purge expired sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// CountActive reports how many sessions are still running, for readiness.
func (r *SessionRepo) CountActive(ctx context.Context) (int, error) {
	const query = `
		SELECT count(*) FROM liveness_sessions
		WHERE state IN ('PENDING', 'IN_PROGRESS')`

	var n int
	if err := r.db.Pool.QueryRow(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: count active sessions: %w", err)
	}
	return n, nil
}

// scanSession reads one row.
func scanSession(row pgx.Row) (*liveness.Session, error) {
	var (
		s          liveness.Session
		id, state  string
		challenges []string
		hashes     []int64
		embedding  []float32
		progress   []byte
	)

	err := row.Scan(
		&id, &s.Nonce, &state, &challenges, &s.Current,
		&s.CreatedAt, &s.ExpiresAt, &s.ChallengeDeadline,
		&s.LastSeq, &hashes, &s.DuplicateStreak,
		&embedding, &progress, &s.FailureReason, &s.Retries, &s.Version,
	)
	if err != nil {
		return nil, err
	}

	s.ID = liveness.SessionID(id)
	s.State = liveness.State(state)
	s.Challenges = challengesFromText(challenges)
	s.RecentHashes = hashesFromInt64(hashes)

	if len(embedding) > 0 {
		s.ReferenceEmbedding = biometric.Embedding(embedding)
	}
	if len(progress) > 0 {
		if err := json.Unmarshal(progress, &s.Progress); err != nil {
			return nil, fmt.Errorf("decode progress: %w", err)
		}
	}
	return &s, nil
}

func challengesToText(cs []liveness.ChallengeKind) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

func challengesFromText(ss []string) []liveness.ChallengeKind {
	out := make([]liveness.ChallengeKind, len(ss))
	for i, s := range ss {
		out[i] = liveness.ChallengeKind(s)
	}
	return out
}

// hashesToInt64 reinterprets unsigned hashes as signed.
//
// Postgres has no unsigned 64-bit type. The bits are preserved exactly and both
// sides reinterpret them the same way, so equality still works — but a query
// that sorts this column will produce an order that means nothing.
func hashesToInt64(hs []uint64) []int64 {
	out := make([]int64, len(hs))
	for i, h := range hs {
		out[i] = int64(h)
	}
	return out
}

func hashesFromInt64(is []int64) []uint64 {
	if len(is) == 0 {
		return nil
	}
	out := make([]uint64, len(is))
	for i, v := range is {
		out[i] = uint64(v)
	}
	return out
}

func embeddingToFloat32(e biometric.Embedding) []float32 {
	if len(e) == 0 {
		return nil
	}
	return []float32(e)
}
