package enrollment

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"
)

// Errors a caller is expected to branch on.
var (
	// ErrTokenInvalid means the token is unknown, already spent, or expired.
	//
	// One error for all three, deliberately. A caller who could tell "already
	// used" from "never existed" learns whether a token they captured was real,
	// and a caller who could tell "expired" from "unknown" learns the window.
	// None of that helps an honest integrator, who has the token they were just
	// handed and needs only to know whether it worked.
	ErrTokenInvalid = errors.New("enrollment: liveness token is not valid")
)

// tokenBytes is the size of the random part of a token.
//
// 256 bits. The token is a bearer credential that authorises writing a face
// into the gallery, so it is sized to be unguessable rather than to be short.
const tokenBytes = 32

// TokenRecord is what the store keeps about an issued token.
//
// IssuedAt is carried rather than defaulted in SQL. The domain never reads the
// clock itself, and taking one timestamp from the database while the other came
// from Go is how a backdated token ends up violating its own expiry constraint —
// which is exactly what happened the first time this was written.
type TokenRecord struct {
	Hash      []byte
	SessionID string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// TokenStore persists issued tokens.
type TokenStore interface {
	// Save records a newly issued token.
	Save(ctx context.Context, rec TokenRecord) error

	// Consume marks a token spent and returns the session it carried.
	//
	// It must be atomic: two requests arriving with the same token must not
	// both succeed. Implementations that read, check, then write are wrong, and
	// the race is exactly the one an attacker replaying a captured token would
	// try to win.
	//
	// It must return ErrTokenInvalid when the hash is unknown, already spent, or
	// past its expiry, without distinguishing between them.
	Consume(ctx context.Context, hash []byte, now time.Time) (sessionID string, err error)

	// PurgeExpired removes tokens that expired before the cutoff.
	PurgeExpired(ctx context.Context, before time.Time) (int, error)
}

// TokenService issues and redeems liveness tokens.
//
// A token is what lets an enrollment request prove that the face it carries was
// verified live, without the enrollment path having to trust its caller. It is
// single use, short lived, and bound to one session.
type TokenService struct {
	store  TokenStore
	secret []byte
	ttl    time.Duration

	// entropy is the randomness source, injectable so a test can be
	// deterministic. It is never allowed to be a weak source in production:
	// NewTokenService defaults it to crypto/rand.
	entropy io.Reader
}

// TokenDeps is what the token service needs built for it.
type TokenDeps struct {
	Store   TokenStore
	Secret  string
	TTL     time.Duration
	Entropy io.Reader
}

// NewTokenService validates the wiring.
func NewTokenService(d TokenDeps) (*TokenService, error) {
	var problems []error

	if d.Store == nil {
		problems = append(problems, errors.New("Store is required"))
	}
	if d.Secret == "" {
		problems = append(problems, errors.New("Secret is required; tokens are stored as HMACs and cannot be keyed with nothing"))
	}
	if d.TTL <= 0 {
		problems = append(problems, fmt.Errorf("TTL must be positive, got %s", d.TTL))
	}

	if err := errors.Join(problems...); err != nil {
		return nil, fmt.Errorf("enrollment: token service: %w", err)
	}

	entropy := d.Entropy
	if entropy == nil {
		entropy = rand.Reader
	}

	return &TokenService{
		store:   d.Store,
		secret:  []byte(d.Secret),
		ttl:     d.TTL,
		entropy: entropy,
	}, nil
}

// Issue mints a token for a session that has passed.
//
// The raw token is returned exactly once and never stored. What is stored is
// its HMAC, so a dump of the table yields nothing usable without the secret.
func (s *TokenService) Issue(ctx context.Context, sessionID string, now time.Time) (string, error) {
	if sessionID == "" {
		return "", errors.New("enrollment: a token needs the session that earned it")
	}

	raw := make([]byte, tokenBytes)
	if _, err := io.ReadFull(s.entropy, raw); err != nil {
		return "", fmt.Errorf("enrollment: generate token: %w", err)
	}

	token := base64.RawURLEncoding.EncodeToString(raw)

	rec := TokenRecord{
		Hash:      s.hash(token),
		SessionID: sessionID,
		IssuedAt:  now,
		ExpiresAt: now.Add(s.ttl),
	}
	if err := s.store.Save(ctx, rec); err != nil {
		return "", fmt.Errorf("enrollment: persist token: %w", err)
	}
	return token, nil
}

// Redeem spends a token and returns the session it was issued for.
//
// Spending is the storage layer's atomic operation, not a read followed by a
// write here: two requests arriving with the same token must not both succeed,
// and doing the check in Go would open exactly the race a replayed token wants.
func (s *TokenService) Redeem(ctx context.Context, token string, now time.Time) (string, error) {
	if token == "" {
		return "", ErrTokenInvalid
	}

	sessionID, err := s.store.Consume(ctx, s.hash(token), now)
	if err != nil {
		if errors.Is(err, ErrTokenInvalid) {
			return "", err
		}
		return "", fmt.Errorf("enrollment: redeem token: %w", err)
	}
	return sessionID, nil
}

// PurgeExpired removes tokens past their expiry.
func (s *TokenService) PurgeExpired(ctx context.Context, before time.Time) (int, error) {
	return s.store.PurgeExpired(ctx, before)
}

// hash is the keyed digest stored in place of the token.
//
// HMAC rather than a plain SHA-256: the token is high-entropy so a plain hash
// would resist a dictionary attack too, but keying it means a stolen database
// is useless without the secret, which lives in the environment.
func (s *TokenService) hash(token string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(token))
	return mac.Sum(nil)
}
