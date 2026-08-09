//go:build integration

// Package integration exercises the adapters against the real services from
// docker compose.
//
// Against the compose Postgres rather than testcontainers, which the spec
// suggested. Testcontainers would need the Docker socket mounted into the test
// container, and handing a test process the ability to start arbitrary
// containers is a privilege this does not need: the database it wants is
// already running on the same compose network.
//
//	docker compose up -d postgres
//	docker compose run --rm dev go test -tags=integration ./tests/integration/...
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/config"
	"github.com/ziad/liveness-verifier/internal/liveness"
	"github.com/ziad/liveness-verifier/internal/storage/postgres"
)

// dsn returns the connection string, skipping the test when there is none.
func dsn(t *testing.T) string {
	t.Helper()

	v := os.Getenv("LV_DATABASE_URL")
	if v == "" {
		t.Skip("LV_DATABASE_URL is not set; run: docker compose up -d postgres")
	}
	return v
}

// openDB connects, skipping when the database is unreachable rather than
// failing: a developer running unit tests should not see a red suite because
// they have not started the stack.
func openDB(t *testing.T) *postgres.DB {
	t.Helper()

	url := dsn(t)
	cfg := config.Database{
		URL:            config.Secret(url),
		MaxConns:       4,
		MinConns:       1,
		ConnectTimeout: 5 * time.Second,
	}

	db, err := postgres.Open(context.Background(), cfg)
	if err != nil {
		t.Skipf("postgres is not reachable: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// migrated brings the schema up and returns a connected pool.
func migrated(t *testing.T) *postgres.DB {
	t.Helper()

	url := dsn(t)
	if err := postgres.Migrate(context.Background(), url); err != nil {
		t.Fatalf("Migrate() returned an unexpected error: %v", err)
	}
	return openDB(t)
}

// The criterion the spec calls X5: a schema that can only go forwards is a
// schema nobody can roll back from.
func TestMigrationsGoUpDownAndUpAgain(t *testing.T) {
	url := dsn(t)
	ctx := context.Background()

	if err := postgres.Migrate(ctx, url); err != nil {
		t.Fatalf("first Migrate() returned an unexpected error: %v", err)
	}
	up, err := postgres.MigrationVersion(ctx, url)
	if err != nil {
		t.Fatalf("MigrationVersion() returned an unexpected error: %v", err)
	}
	if up == 0 {
		t.Fatal("no migrations were applied")
	}

	if err := postgres.MigrateDown(ctx, url); err != nil {
		t.Fatalf("MigrateDown() returned an unexpected error: %v", err)
	}
	down, err := postgres.MigrationVersion(ctx, url)
	if err != nil {
		t.Fatalf("MigrationVersion() returned an unexpected error: %v", err)
	}
	if down >= up {
		t.Errorf("version after rolling back = %d, want below %d", down, up)
	}

	if err := postgres.Migrate(ctx, url); err != nil {
		t.Fatalf("second Migrate() returned an unexpected error: %v", err)
	}
	again, err := postgres.MigrationVersion(ctx, url)
	if err != nil {
		t.Fatalf("MigrationVersion() returned an unexpected error: %v", err)
	}
	if again != up {
		t.Errorf("version after reapplying = %d, want %d", again, up)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	url := dsn(t)

	for i := 0; i < 3; i++ {
		if err := postgres.Migrate(context.Background(), url); err != nil {
			t.Fatalf("Migrate() run %d returned an unexpected error: %v", i, err)
		}
	}
}

// newRepo returns a repository and a session id nobody else is using.
func newRepo(t *testing.T) (*postgres.SessionRepo, liveness.SessionID) {
	t.Helper()

	db := migrated(t)
	repo, err := postgres.NewSessionRepo(db)
	if err != nil {
		t.Fatalf("NewSessionRepo() returned an unexpected error: %v", err)
	}

	id := liveness.SessionID(fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano()))
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM liveness_sessions WHERE id = $1`, string(id))
	})
	return repo, id
}

func newStoredSession(t *testing.T, id liveness.SessionID) *liveness.Session {
	t.Helper()

	s, err := liveness.NewSession(time.Now().UTC(), liveness.NewSessionParams{
		ID:               id,
		ChallengeCount:   3,
		TTL:              90 * time.Second,
		ChallengeTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSession() returned an unexpected error: %v", err)
	}
	return s
}

// Every field must survive the round trip, or a session resumed on another
// request would quietly lose part of its state.
func TestSessionRoundTrip(t *testing.T) {
	repo, id := newRepo(t)
	ctx := context.Background()

	want := newStoredSession(t, id)
	want.LastSeq = 42
	want.RecentHashes = []uint64{0xFFFF_FFFF_FFFF_FFFF, 1, 0x8000_0000_0000_0000}
	want.DuplicateStreak = 3
	want.Retries = 2
	want.Progress = liveness.Progress{
		ClosedFrames: 2, SawClose: true,
		BaselineYaw: -12.5, BaselinePitch: 3.25, HaveBaseline: true,
	}

	embedding := make(biometric.Embedding, biometric.EmbeddingDim)
	for i := range embedding {
		embedding[i] = float32(i) / 1000
	}
	want.ReferenceEmbedding = embedding.Normalize()

	if err := repo.Create(ctx, want); err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() returned an unexpected error: %v", err)
	}

	if got.Nonce != want.Nonce || got.State != want.State {
		t.Errorf("nonce/state = %q/%s, want %q/%s", got.Nonce, got.State, want.Nonce, want.State)
	}
	if len(got.Challenges) != len(want.Challenges) {
		t.Fatalf("stored %d challenges, want %d", len(got.Challenges), len(want.Challenges))
	}
	for i := range want.Challenges {
		if got.Challenges[i] != want.Challenges[i] {
			t.Errorf("challenge %d = %s, want %s", i, got.Challenges[i], want.Challenges[i])
		}
	}
	if got.LastSeq != want.LastSeq || got.DuplicateStreak != want.DuplicateStreak {
		t.Errorf("last seq / streak = %d/%d, want %d/%d",
			got.LastSeq, got.DuplicateStreak, want.LastSeq, want.DuplicateStreak)
	}
	// The retry budget has to survive the round trip, or a session reloaded
	// between frames would hand back attempts it had already spent.
	if got.Retries != want.Retries {
		t.Errorf("retries = %d, want %d", got.Retries, want.Retries)
	}
	if got.Progress != want.Progress {
		t.Errorf("progress = %+v, want %+v", got.Progress, want.Progress)
	}

	// Hashes are unsigned and Postgres is not; the bits must survive anyway.
	if len(got.RecentHashes) != len(want.RecentHashes) {
		t.Fatalf("stored %d hashes, want %d", len(got.RecentHashes), len(want.RecentHashes))
	}
	for i := range want.RecentHashes {
		if got.RecentHashes[i] != want.RecentHashes[i] {
			t.Errorf("hash %d = %#x, want %#x", i, got.RecentHashes[i], want.RecentHashes[i])
		}
	}

	if c := got.ReferenceEmbedding.Cosine(want.ReferenceEmbedding); c < 0.999999 {
		t.Errorf("embedding cosine after the round trip = %.9f, want 1", c)
	}
}

func TestGetUnknownSession(t *testing.T) {
	repo, _ := newRepo(t)

	if _, err := repo.Get(context.Background(), "definitely-not-stored"); !errors.Is(err, liveness.ErrSessionNotFound) {
		t.Errorf("Get() error = %v, want ErrSessionNotFound", err)
	}
}

// Two frames of one session can be in flight at once. Without the version in
// the WHERE clause the second write silently discards the first's progress.
func TestUpdateRefusesAStaleWrite(t *testing.T) {
	repo, id := newRepo(t)
	ctx := context.Background()

	s := newStoredSession(t, id)
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	// Two readers see the same version.
	first, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() returned an unexpected error: %v", err)
	}
	second, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() returned an unexpected error: %v", err)
	}

	// The first advances the session.
	if err := first.Begin(time.Now().UTC()); err != nil {
		t.Fatalf("Begin() returned an unexpected error: %v", err)
	}
	if err := repo.Update(ctx, first); err != nil {
		t.Fatalf("the first update failed: %v", err)
	}

	// The second is now working from a stale copy.
	second.LastSeq = 99
	if err := repo.Update(ctx, second); !errors.Is(err, liveness.ErrVersionConflict) {
		t.Fatalf("the stale update returned %v, want ErrVersionConflict", err)
	}

	// And the first writer's progress survived.
	stored, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() returned an unexpected error: %v", err)
	}
	if stored.State != liveness.StateInProgress {
		t.Errorf("state = %s, want the first writer's %s", stored.State, liveness.StateInProgress)
	}
	if stored.LastSeq == 99 {
		t.Error("the stale write landed anyway")
	}
}

func TestUpdateUnknownSession(t *testing.T) {
	repo, id := newRepo(t)

	s := newStoredSession(t, id) // never created
	if err := repo.Update(context.Background(), s); !errors.Is(err, liveness.ErrSessionNotFound) {
		t.Errorf("Update() error = %v, want ErrSessionNotFound", err)
	}
}

func TestDeleteExpiredRemovesOnlyOldSessions(t *testing.T) {
	repo, id := newRepo(t)
	ctx := context.Background()

	old := newStoredSession(t, id)
	old.ExpiresAt = time.Now().UTC().Add(-time.Hour)
	if err := repo.Create(ctx, old); err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}

	fresh := newStoredSession(t, id+"-fresh")
	if err := repo.Create(ctx, fresh); err != nil {
		t.Fatalf("Create() returned an unexpected error: %v", err)
	}
	t.Cleanup(func() { _, _ = repo.DeleteExpired(ctx, time.Now().UTC().Add(time.Hour)) })

	n, err := repo.DeleteExpired(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("DeleteExpired() returned an unexpected error: %v", err)
	}
	if n < 1 {
		t.Errorf("purged %d sessions, want at least the expired one", n)
	}

	if _, err := repo.Get(ctx, id); !errors.Is(err, liveness.ErrSessionNotFound) {
		t.Error("the expired session survived the purge")
	}
	if _, err := repo.Get(ctx, fresh.ID); err != nil {
		t.Errorf("the live session was purged too: %v", err)
	}
}

// The check constraint is the last line of defence against a state the domain
// does not understand reaching the table.
func TestUnknownStateIsRefusedByTheSchema(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()

	_, err := db.Pool.Exec(ctx, `
		INSERT INTO liveness_sessions
			(id, nonce, state, challenges, created_at, expires_at, challenge_deadline)
		VALUES ('bad-state', 'n', 'SOMETHING_ELSE', ARRAY['BLINK'], now(), now(), now())`)

	if err == nil {
		_, _ = db.Pool.Exec(ctx, `DELETE FROM liveness_sessions WHERE id = 'bad-state'`)
		t.Fatal("the schema accepted a state the domain does not define")
	}
}
