package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/enrollment"
)

// FaceRepo stores and searches the face gallery.
type FaceRepo struct {
	db *DB

	// efSearch is how many candidates HNSW keeps while descending the graph.
	//
	// Set per query rather than per pool because it is the recall-versus-latency
	// dial, and a value set once at connection time would silently apply to
	// whatever ran next on a pooled connection.
	efSearch int
}

var _ enrollment.Repository = (*FaceRepo)(nil)

// NewFaceRepo wraps a pool as a face repository.
func NewFaceRepo(db *DB, efSearch int) (*FaceRepo, error) {
	if db == nil || db.Pool == nil {
		return nil, errors.New("postgres: face repository needs a pool")
	}
	if efSearch < 1 {
		return nil, fmt.Errorf("postgres: hnsw ef_search must be at least 1, got %d", efSearch)
	}
	return &FaceRepo{db: db, efSearch: efSearch}, nil
}

// Insert adds one enrolled capture.
func (r *FaceRepo) Insert(ctx context.Context, f enrollment.Face) error {
	if err := f.Validate(); err != nil {
		return fmt.Errorf("postgres: refusing to store face: %w", err)
	}

	const query = `
		INSERT INTO faces (id, subject_id, embedding, session_id, artifact_key)
		VALUES ($1, $2, $3, $4, $5)`

	var artifact any
	if f.ArtifactKey != "" {
		artifact = f.ArtifactKey
	}

	_, err := r.db.Pool.Exec(ctx, query,
		string(f.ID), f.SubjectID, vectorLiteral(f.Embedding), f.SessionID, artifact)
	if err != nil {
		// The id is safe to name; the subject is not, and neither is anything
		// derived from the template.
		return fmt.Errorf("postgres: insert face %s: %w", f.ID, err)
	}
	return nil
}

// Search returns the nearest faces by cosine similarity, most similar first.
//
// No threshold is applied here. Deciding what counts as a match belongs to the
// service, where it is one testable comparison rather than a filter buried in
// SQL that nobody can see from the outside.
func (r *FaceRepo) Search(ctx context.Context, query biometric.Embedding, topK int) ([]enrollment.Match, error) {
	if err := query.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: search vector: %w", err)
	}
	if topK < 1 {
		return nil, fmt.Errorf("postgres: topK must be at least 1, got %d", topK)
	}

	// Columns are named rather than selected wholesale. A SELECT * here would
	// carry the embedding of every candidate up towards a response, and the one
	// thing that must never leave this table is the template itself.
	const sql = `
		SELECT id, subject_id, 1 - (embedding <=> $1) AS similarity
		FROM faces
		ORDER BY embedding <=> $1
		LIMIT $2`

	conn, err := r.db.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: acquire connection: %w", err)
	}
	defer conn.Release()

	// ef_search is per transaction, so it cannot leak onto whatever runs next
	// on this pooled connection.
	if _, err := conn.Exec(ctx, "SET LOCAL hnsw.ef_search = "+strconv.Itoa(r.efSearch)); err != nil {
		return nil, fmt.Errorf("postgres: set hnsw.ef_search: %w", err)
	}

	rows, err := conn.Query(ctx, sql, vectorLiteral(query), topK)
	if err != nil {
		return nil, fmt.Errorf("postgres: search faces: %w", err)
	}
	defer rows.Close()

	var out []enrollment.Match
	for rows.Next() {
		var m enrollment.Match
		var id string
		if err := rows.Scan(&id, &m.SubjectID, &m.Score); err != nil {
			return nil, fmt.Errorf("postgres: scan match: %w", err)
		}
		m.FaceID = enrollment.FaceID(id)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: read matches: %w", err)
	}
	return out, nil
}

// DeleteSubject removes every face belonging to a subject.
//
// A hard delete, not a flag. A biometric template that is still readable has
// not been deleted, whatever a column says about it. The audit trail lives in
// its own table precisely so that the record of the deletion survives the thing
// deleted.
func (r *FaceRepo) DeleteSubject(ctx context.Context, subjectID string) (int, error) {
	if subjectID == "" {
		return 0, errors.New("postgres: subject id is required")
	}

	tag, err := r.db.Pool.Exec(ctx, `DELETE FROM faces WHERE subject_id = $1`, subjectID)
	if err != nil {
		return 0, fmt.Errorf("postgres: delete subject: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Count reports how many faces are enrolled.
func (r *FaceRepo) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.db.Pool.QueryRow(ctx, `SELECT count(*) FROM faces`).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: count faces: %w", err)
	}
	return n, nil
}

// vectorLiteral renders an embedding in pgvector's text form.
//
// Written by hand rather than pulled in with the pgvector Go package: the
// format is "[1,2,3]" and adding a dependency for it would be more code to
// audit than the six lines it replaces.
//
// Precision matters here. Formatted with -1 precision, so each float32 round
// trips exactly rather than being quietly truncated into a slightly different
// vector from the one that was measured.
func vectorLiteral(e biometric.Embedding) string {
	var b strings.Builder
	b.Grow(len(e)*12 + 2)

	b.WriteByte('[')
	for i, v := range e {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
