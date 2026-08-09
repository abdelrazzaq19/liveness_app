//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ziad/liveness-verifier/internal/enrollment"
	"github.com/ziad/liveness-verifier/internal/storage/postgres"
)

func newTokenService(t *testing.T, ttl time.Duration) *enrollment.TokenService {
	t.Helper()

	db := migrated(t)
	if _, err := db.Pool.Exec(context.Background(), "DELETE FROM liveness_tokens"); err != nil {
		t.Fatalf("could not clear the token table: %v", err)
	}

	store, err := postgres.NewTokenStore(db)
	if err != nil {
		t.Fatalf("NewTokenStore() returned an unexpected error: %v", err)
	}

	svc, err := enrollment.NewTokenService(enrollment.TokenDeps{
		Store:  store,
		Secret: "test-signing-secret",
		TTL:    ttl,
	})
	if err != nil {
		t.Fatalf("NewTokenService() returned an unexpected error: %v", err)
	}
	return svc
}

func TestTokenIsRedeemedOnceAndOnlyOnce(t *testing.T) {
	svc := newTokenService(t, 5*time.Minute)
	ctx := context.Background()
	now := time.Now()

	token, err := svc.Issue(ctx, "session-abc", now)
	if err != nil {
		t.Fatalf("Issue() returned an unexpected error: %v", err)
	}
	if token == "" {
		t.Fatal("Issue() returned an empty token")
	}

	got, err := svc.Redeem(ctx, token, now)
	if err != nil {
		t.Fatalf("Redeem() returned an unexpected error: %v", err)
	}
	if got != "session-abc" {
		t.Errorf("redeemed session = %q, want session-abc", got)
	}

	// The second attempt is the attack: a captured token replayed.
	if _, err := svc.Redeem(ctx, token, now); !errors.Is(err, enrollment.ErrTokenInvalid) {
		t.Errorf("second Redeem() error = %v, want ErrTokenInvalid", err)
	}
}

// The guarantee that matters, and the one a read-then-write implementation
// would quietly lose: two requests arriving together with the same token must
// produce exactly one enrollment.
func TestConcurrentRedeemsProduceExactlyOneWinner(t *testing.T) {
	svc := newTokenService(t, 5*time.Minute)
	ctx := context.Background()
	now := time.Now()

	token, err := svc.Issue(ctx, "session-race", now)
	if err != nil {
		t.Fatalf("Issue() returned an unexpected error: %v", err)
	}

	const racers = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		others  []error
	)

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			_, err := svc.Redeem(ctx, token, now)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, enrollment.ErrTokenInvalid):
				// Expected for everyone who lost.
			default:
				others = append(others, err)
			}
		}()
	}

	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("%d of %d concurrent redemptions succeeded, want exactly 1", winners, racers)
	}
	for _, err := range others {
		t.Errorf("a racer failed for the wrong reason: %v", err)
	}
}

func TestTokenRefusalsAreIndistinguishable(t *testing.T) {
	svc := newTokenService(t, time.Minute)
	ctx := context.Background()
	now := time.Now()

	spent, err := svc.Issue(ctx, "session-spent", now)
	if err != nil {
		t.Fatalf("Issue() returned an unexpected error: %v", err)
	}
	if _, err := svc.Redeem(ctx, spent, now); err != nil {
		t.Fatalf("Redeem() returned an unexpected error: %v", err)
	}

	expired, err := svc.Issue(ctx, "session-expired", now)
	if err != nil {
		t.Fatalf("Issue() returned an unexpected error: %v", err)
	}

	tests := []struct {
		name  string
		token string
		when  time.Time
	}{
		{"never existed", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", now},
		{"already spent", spent, now},
		{"past its expiry", expired, now.Add(2 * time.Minute)},
		{"empty", "", now},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Redeem(ctx, tt.token, tt.when)
			if !errors.Is(err, enrollment.ErrTokenInvalid) {
				t.Errorf("error = %v, want ErrTokenInvalid", err)
			}
			// Every refusal must read the same. A caller that could tell these
			// apart learns whether a token they captured was ever real.
			if err != nil && err.Error() != enrollment.ErrTokenInvalid.Error() {
				t.Errorf("refusal message %q differs from the others; it distinguishes the cause", err)
			}
		})
	}
}

// The raw token must not be recoverable from storage. What is kept is its HMAC,
// so a dump of the table is useless without the secret.
func TestStoredTokenIsNotTheTokenItself(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()

	if _, err := db.Pool.Exec(ctx, "DELETE FROM liveness_tokens"); err != nil {
		t.Fatalf("could not clear the token table: %v", err)
	}

	store, err := postgres.NewTokenStore(db)
	if err != nil {
		t.Fatalf("NewTokenStore() returned an unexpected error: %v", err)
	}
	svc, err := enrollment.NewTokenService(enrollment.TokenDeps{
		Store: store, Secret: "test-signing-secret", TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewTokenService() returned an unexpected error: %v", err)
	}

	token, err := svc.Issue(ctx, "session-secret", time.Now())
	if err != nil {
		t.Fatalf("Issue() returned an unexpected error: %v", err)
	}

	var stored []byte
	if err := db.Pool.QueryRow(ctx, "SELECT token_hash FROM liveness_tokens LIMIT 1").Scan(&stored); err != nil {
		t.Fatalf("could not read the stored row: %v", err)
	}

	if string(stored) == token {
		t.Fatal("the raw token is stored in the database")
	}
	if len(stored) != 32 {
		t.Errorf("stored hash is %d bytes, want 32 (SHA-256)", len(stored))
	}

	// A different secret must not be able to redeem the same token, which is
	// what makes the stored row worthless on its own.
	other, err := enrollment.NewTokenService(enrollment.TokenDeps{
		Store: store, Secret: "a-different-secret", TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewTokenService() returned an unexpected error: %v", err)
	}
	if _, err := other.Redeem(ctx, token, time.Now()); !errors.Is(err, enrollment.ErrTokenInvalid) {
		t.Errorf("a service with a different secret redeemed the token: %v", err)
	}
}

func TestPurgeRemovesOnlyExpiredTokens(t *testing.T) {
	svc := newTokenService(t, time.Minute)
	ctx := context.Background()
	now := time.Now()

	live, err := svc.Issue(ctx, "session-live", now)
	if err != nil {
		t.Fatalf("Issue() returned an unexpected error: %v", err)
	}
	if _, err := svc.Issue(ctx, "session-old", now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("Issue() returned an unexpected error: %v", err)
	}

	n, err := svc.PurgeExpired(ctx, now)
	if err != nil {
		t.Fatalf("PurgeExpired() returned an unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d tokens, want 1", n)
	}

	if _, err := svc.Redeem(ctx, live, now); err != nil {
		t.Errorf("the live token was purged too: %v", err)
	}
}
