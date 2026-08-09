//go:build integration

package integration

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/enrollment"
	"github.com/ziad/liveness-verifier/internal/storage/postgres"
)

// newFaceRepo returns a repository over a migrated schema, with the gallery
// emptied so one test's rows cannot decide another's recall.
func newFaceRepo(t *testing.T, efSearch int) *postgres.FaceRepo {
	t.Helper()

	db := migrated(t)
	if _, err := db.Pool.Exec(context.Background(), "DELETE FROM faces"); err != nil {
		t.Fatalf("could not clear the gallery: %v", err)
	}

	repo, err := postgres.NewFaceRepo(db, efSearch)
	if err != nil {
		t.Fatalf("NewFaceRepo() returned an unexpected error: %v", err)
	}
	return repo
}

// randomEmbedding returns a normalised vector from a seeded source, so a
// failure can be reproduced rather than rerun until it passes.
func randomEmbedding(rng *rand.Rand) biometric.Embedding {
	e := make(biometric.Embedding, biometric.EmbeddingDim)
	for i := range e {
		e[i] = float32(rng.NormFloat64())
	}
	return e.Normalize()
}

// nudged returns a vector close to the original: the same face captured again,
// not a different person.
//
// amount is per component, and the scale that matters is not obvious: each
// component of a normalised 512-dimensional vector averages about 0.044, so a
// nudge of 0.12 is three times the signal and produces an unrelated face. 0.012
// lands near cosine 0.97, which is what a second capture of one person looks
// like.
func nudged(e biometric.Embedding, rng *rand.Rand, amount float64) biometric.Embedding {
	out := make(biometric.Embedding, len(e))
	for i := range e {
		out[i] = e[i] + float32(rng.NormFloat64()*amount)
	}
	return out.Normalize()
}

func testFace(id, subject string, e biometric.Embedding) enrollment.Face {
	return enrollment.Face{
		ID:        enrollment.FaceID(id),
		SubjectID: subject,
		SessionID: "session-" + id,
		Embedding: e,
	}
}

// auditFor is the entry that must accompany a face. The repository will not
// take one without the other, which is the point.
func auditFor(f enrollment.Face) enrollment.AuditEntry {
	return enrollment.AuditEntry{
		At: time.Now(), Action: enrollment.AuditEnroll, Outcome: enrollment.AuditOK,
		SubjectID: f.SubjectID, FaceID: f.ID, SessionID: f.SessionID, Affected: 1,
	}
}

func deleteAudit(subject string) enrollment.AuditEntry {
	return enrollment.AuditEntry{
		At: time.Now(), Action: enrollment.AuditDelete, Outcome: enrollment.AuditOK,
		SubjectID: subject,
	}
}

// insert stores a face with its entry, which is the only way the repository
// accepts one.
func insert(repo *postgres.FaceRepo, ctx context.Context, f enrollment.Face) error {
	return repo.Insert(ctx, f, auditFor(f))
}

func TestFaceRoundTripAndSearch(t *testing.T) {
	repo := newFaceRepo(t, 64)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))

	mine := randomEmbedding(rng)
	if err := insert(repo, ctx, testFace("face-1", "subject-a", mine)); err != nil {
		t.Fatalf("Insert() returned an unexpected error: %v", err)
	}
	for i := 0; i < 20; i++ {
		f := testFace(fmt.Sprintf("other-%d", i), fmt.Sprintf("subject-%d", i), randomEmbedding(rng))
		if err := insert(repo, ctx, f); err != nil {
			t.Fatalf("Insert() returned an unexpected error: %v", err)
		}
	}

	// The same face captured again, not the same vector: an exact match would
	// prove only that the row came back.
	got, err := repo.Search(ctx, nudged(mine, rng, 0.012), 5)
	if err != nil {
		t.Fatalf("Search() returned an unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Search() returned nothing")
	}
	if got[0].SubjectID != "subject-a" {
		t.Errorf("nearest match is %q, want subject-a", got[0].SubjectID)
	}
	if got[0].Score <= 0.8 {
		t.Errorf("similarity to a re-capture of the same face = %.4f, want above 0.8", got[0].Score)
	}

	// Descending order is part of the contract; a caller reading only the first
	// result depends on it.
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score {
			t.Errorf("result %d scores %.4f, above result %d at %.4f; order is not descending",
				i, got[i].Score, i-1, got[i-1].Score)
		}
	}
}

// A stored embedding must come back as the same vector. A silent precision loss
// on the way through the text encoding would move every similarity slightly and
// be nearly impossible to spot afterwards.
func TestEmbeddingSurvivesStorageExactly(t *testing.T) {
	repo := newFaceRepo(t, 64)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))

	e := randomEmbedding(rng)
	if err := insert(repo, ctx, testFace("exact-1", "subject-exact", e)); err != nil {
		t.Fatalf("Insert() returned an unexpected error: %v", err)
	}

	got, err := repo.Search(ctx, e, 1)
	if err != nil {
		t.Fatalf("Search() returned an unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search() returned %d results, want 1", len(got))
	}

	// Searching for exactly what was stored must score 1 to within float32.
	if diff := math.Abs(got[0].Score - 1); diff > 1e-6 {
		t.Errorf("self-similarity = %.9f, want 1; the vector changed in storage", got[0].Score)
	}
}

func TestInsertRefusesFacesThatCannotBeTraced(t *testing.T) {
	repo := newFaceRepo(t, 64)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(3))

	good := testFace("trace-1", "subject-t", randomEmbedding(rng))

	tests := []struct {
		name   string
		break_ func(*enrollment.Face)
	}{
		{"no id", func(f *enrollment.Face) { f.ID = "" }},
		{"no subject", func(f *enrollment.Face) { f.SubjectID = "" }},
		{"no session", func(f *enrollment.Face) { f.SessionID = "" }},
		{"no embedding", func(f *enrollment.Face) { f.Embedding = nil }},
		{"unnormalised embedding", func(f *enrollment.Face) {
			e := make(biometric.Embedding, biometric.EmbeddingDim)
			for i := range e {
				e[i] = 3
			}
			f.Embedding = e
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := good
			tt.break_(&f)

			if err := repo.Insert(ctx, f, auditFor(f)); err == nil {
				t.Error("Insert() accepted a face that cannot be traced or compared")
			}
		})
	}
}

// Deleting a subject must remove the template itself, not flag it. A biometric
// template that is still readable has not been deleted.
func TestDeleteSubjectRemovesEveryFace(t *testing.T) {
	repo := newFaceRepo(t, 64)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(11))

	var theirs biometric.Embedding
	for i := 0; i < 3; i++ {
		theirs = randomEmbedding(rng)
		if err := insert(repo, ctx, testFace(fmt.Sprintf("del-%d", i), "subject-gone", theirs)); err != nil {
			t.Fatalf("Insert() returned an unexpected error: %v", err)
		}
	}
	keep := randomEmbedding(rng)
	if err := insert(repo, ctx, testFace("keep-1", "subject-stays", keep)); err != nil {
		t.Fatalf("Insert() returned an unexpected error: %v", err)
	}

	n, err := repo.DeleteSubject(ctx, "subject-gone", deleteAudit("subject-gone"))
	if err != nil {
		t.Fatalf("DeleteSubject() returned an unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted %d faces, want 3", n)
	}

	// Searching with their own embedding must not find them.
	got, err := repo.Search(ctx, theirs, 5)
	if err != nil {
		t.Fatalf("Search() returned an unexpected error: %v", err)
	}
	for _, m := range got {
		if m.SubjectID == "subject-gone" {
			t.Errorf("a deleted subject is still searchable as %s", m.FaceID)
		}
	}

	count, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count() returned an unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("gallery holds %d faces after the delete, want 1", count)
	}
}

// Checkpoint B, the criterion that decides whether the index is worth having:
// 10,000 embeddings, p95 search under 50 ms, recall@1 at least 0.98 against
// brute force.
//
// Recall is measured against an exact scan of the same data rather than against
// what was inserted, because an approximate index is allowed to be wrong in
// exactly one way — missing a neighbour the exact search would have found — and
// that is the thing being measured.
func TestGalleryMeetsCheckpointB(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 10,000 row gallery; skipped in short mode")
	}

	const (
		gallerySize = 10_000
		probes      = 200
		efSearch    = 64
	)

	repo := newFaceRepo(t, efSearch)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	stored := make([]biometric.Embedding, gallerySize)
	subjects := make([]string, gallerySize)

	start := time.Now()
	for i := 0; i < gallerySize; i++ {
		stored[i] = randomEmbedding(rng)
		subjects[i] = fmt.Sprintf("subject-%05d", i)

		if err := insert(repo, ctx, testFace(fmt.Sprintf("bulk-%05d", i), subjects[i], stored[i])); err != nil {
			t.Fatalf("Insert() at row %d: %v", i, err)
		}
	}
	t.Logf("inserted %d faces in %s", gallerySize, time.Since(start).Round(time.Millisecond))

	var (
		latencies = make([]time.Duration, 0, probes)
		hits      int
	)

	for p := 0; p < probes; p++ {
		want := rng.Intn(gallerySize)
		query := nudged(stored[want], rng, 0.012)

		// Brute force over the same vectors, in Go, as the reference.
		bestIdx, bestScore := -1, math.Inf(-1)
		for i, e := range stored {
			if s := e.Cosine(query); s > bestScore {
				bestIdx, bestScore = i, s
			}
		}

		began := time.Now()
		got, err := repo.Search(ctx, query, 1)
		latencies = append(latencies, time.Since(began))
		if err != nil {
			t.Fatalf("Search() probe %d: %v", p, err)
		}
		if len(got) == 0 {
			continue
		}
		if got[0].SubjectID == subjects[bestIdx] {
			hits++
		}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[len(latencies)*50/100]
	p95 := latencies[len(latencies)*95/100]
	recall := float64(hits) / float64(probes)

	t.Logf("B: %d faces, ef_search %d — p50 %s, p95 %s, recall@1 %.3f over %d probes",
		gallerySize, efSearch, p50.Round(time.Microsecond), p95.Round(time.Microsecond), recall, probes)

	if p95 > 50*time.Millisecond {
		t.Errorf("p95 search = %s, want under 50 ms", p95)
	}
	if recall < 0.98 {
		t.Errorf("recall@1 = %.3f against brute force, want at least 0.98", recall)
	}
}

// The rule this whole table exists for: no template is stored without a record
// of it. Proved by making the audit write impossible and checking that the face
// did not survive either.
func TestAFaceCannotBeStoredWithoutItsAuditEntry(t *testing.T) {
	repo := newFaceRepo(t, 64)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(21))

	f := testFace("atomic-1", "subject-atomic", randomEmbedding(rng))

	// An entry the schema will reject: the CHECK constraint on action fires
	// inside the same transaction as the face insert.
	bad := auditFor(f)
	bad.Action = "NOT_AN_ACTION"

	if err := repo.Insert(ctx, f, bad); err == nil {
		t.Fatal("Insert() succeeded with an audit entry the schema cannot accept")
	}

	// The face must have gone with it.
	got, err := repo.Search(ctx, f.Embedding, 1)
	if err != nil {
		t.Fatalf("Search() returned an unexpected error: %v", err)
	}
	for _, m := range got {
		if m.FaceID == f.ID {
			t.Error("the face was stored even though its audit entry was not")
		}
	}

	n, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count() returned an unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("the gallery holds %d faces after a rolled-back insert, want 0", n)
	}
}

// The record of a deletion must outlive the rows it describes. That is why the
// trail is a separate table.
func TestTheAuditTrailSurvivesTheDeletion(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()

	if _, err := db.Pool.Exec(ctx, "DELETE FROM faces"); err != nil {
		t.Fatalf("could not clear the gallery: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, "DELETE FROM face_audit"); err != nil {
		t.Fatalf("could not clear the audit trail: %v", err)
	}

	repo, err := postgres.NewFaceRepo(db, 64)
	if err != nil {
		t.Fatalf("NewFaceRepo() returned an unexpected error: %v", err)
	}

	rng := rand.New(rand.NewSource(23))
	for i := 0; i < 2; i++ {
		f := testFace(fmt.Sprintf("gone-%d", i), "subject-erased", randomEmbedding(rng))
		if err := insert(repo, ctx, f); err != nil {
			t.Fatalf("Insert() returned an unexpected error: %v", err)
		}
	}

	n, err := repo.DeleteSubject(ctx, "subject-erased", deleteAudit("subject-erased"))
	if err != nil {
		t.Fatalf("DeleteSubject() returned an unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d faces, want 2", n)
	}

	// Two enrollments and one deletion, all still readable.
	var enrolls, deletes, affected int
	err = db.Pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE action = 'ENROLL'),
			count(*) FILTER (WHERE action = 'DELETE'),
			coalesce(sum(affected) FILTER (WHERE action = 'DELETE'), 0)
		FROM face_audit WHERE subject_id = $1`, "subject-erased").Scan(&enrolls, &deletes, &affected)
	if err != nil {
		t.Fatalf("could not read the audit trail: %v", err)
	}

	if enrolls != 2 || deletes != 1 {
		t.Errorf("trail holds %d enrolments and %d deletions, want 2 and 1", enrolls, deletes)
	}
	if affected != 2 {
		t.Errorf("the deletion recorded %d faces removed, want 2", affected)
	}
}

// The trail must never carry the template. It is the record kept longest and
// read most widely.
func TestTheAuditTableHoldsNoBiometrics(t *testing.T) {
	db := migrated(t)

	rows, err := db.Pool.Query(context.Background(), `
		SELECT column_name, data_type FROM information_schema.columns
		WHERE table_name = 'face_audit'`)
	if err != nil {
		t.Fatalf("could not read the schema: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if typ == "USER-DEFINED" || typ == "ARRAY" || typ == "bytea" {
			t.Errorf("column %s is %s; the audit trail must hold no vectors or blobs", name, typ)
		}
		for _, banned := range []string{"embedding", "vector", "descriptor", "image", "crop"} {
			if strings.Contains(name, banned) {
				t.Errorf("column %s looks like biometric data", name)
			}
		}
	}
}
