//go:build integration

package integration

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
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

func TestFaceRoundTripAndSearch(t *testing.T) {
	repo := newFaceRepo(t, 64)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))

	mine := randomEmbedding(rng)
	if err := repo.Insert(ctx, testFace("face-1", "subject-a", mine)); err != nil {
		t.Fatalf("Insert() returned an unexpected error: %v", err)
	}
	for i := 0; i < 20; i++ {
		f := testFace(fmt.Sprintf("other-%d", i), fmt.Sprintf("subject-%d", i), randomEmbedding(rng))
		if err := repo.Insert(ctx, f); err != nil {
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
	if err := repo.Insert(ctx, testFace("exact-1", "subject-exact", e)); err != nil {
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

			if err := repo.Insert(ctx, f); err == nil {
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
		if err := repo.Insert(ctx, testFace(fmt.Sprintf("del-%d", i), "subject-gone", theirs)); err != nil {
			t.Fatalf("Insert() returned an unexpected error: %v", err)
		}
	}
	keep := randomEmbedding(rng)
	if err := repo.Insert(ctx, testFace("keep-1", "subject-stays", keep)); err != nil {
		t.Fatalf("Insert() returned an unexpected error: %v", err)
	}

	n, err := repo.DeleteSubject(ctx, "subject-gone")
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

		if err := repo.Insert(ctx, testFace(fmt.Sprintf("bulk-%05d", i), subjects[i], stored[i])); err != nil {
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
