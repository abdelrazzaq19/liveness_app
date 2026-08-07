// Package liveness orchestrates challenge-response verification sessions.
//
// Nothing here knows about HTTP, storage, or ONNX. It receives a
// biometric.Face and decides what that means for a session, which is what lets
// the whole flow be tested without a camera, a database, or a model file.
package liveness

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
)

// ChallengeKind is one instruction the subject is asked to follow.
type ChallengeKind string

const (
	ChallengeBlink     ChallengeKind = "BLINK"
	ChallengeTurnLeft  ChallengeKind = "TURN_LEFT"
	ChallengeTurnRight ChallengeKind = "TURN_RIGHT"
	ChallengeNod       ChallengeKind = "NOD"
	ChallengeMouthOpen ChallengeKind = "MOUTH_OPEN"
)

// AllChallenges is every kind the generator can draw from.
//
// Order here is not the order a session sees: that is drawn fresh each time.
var AllChallenges = []ChallengeKind{
	ChallengeBlink,
	ChallengeTurnLeft,
	ChallengeTurnRight,
	ChallengeNod,
	ChallengeMouthOpen,
}

// Valid reports whether the kind is one this service knows.
func (c ChallengeKind) Valid() bool {
	for _, k := range AllChallenges {
		if c == k {
			return true
		}
	}
	return false
}

// ErrChallengeCount means more challenges were asked for than exist.
var ErrChallengeCount = errors.New("liveness: not enough distinct challenges available")

// NewChallengeSet draws count distinct challenges in a random order.
//
// The randomness is cryptographic, and that is the point rather than a detail.
// The unpredictable order is the defence against a pre-recorded video: an
// attacker who knows what will be asked, or can guess it often enough, can
// prepare a recording that satisfies it. math/rand seeded from the clock would
// look identical in a test and be worthless in front of an attacker.
func NewChallengeSet(entropy io.Reader, count int) ([]ChallengeKind, error) {
	if count < 1 {
		return nil, fmt.Errorf("%w: asked for %d", ErrChallengeCount, count)
	}
	if count > len(AllChallenges) {
		return nil, fmt.Errorf("%w: asked for %d, only %d exist", ErrChallengeCount, count, len(AllChallenges))
	}
	if entropy == nil {
		entropy = rand.Reader
	}

	pool := make([]ChallengeKind, len(AllChallenges))
	copy(pool, AllChallenges)

	// Fisher-Yates over the whole pool, then take the first count. Shuffling
	// everything rather than sampling keeps each subset equally likely.
	for i := len(pool) - 1; i > 0; i-- {
		n, err := randomIndex(entropy, i+1)
		if err != nil {
			return nil, err
		}
		pool[i], pool[n] = pool[n], pool[i]
	}
	return pool[:count], nil
}

// randomIndex returns a uniform value in [0,n).
func randomIndex(entropy io.Reader, n int) (int, error) {
	v, err := rand.Int(entropy, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("liveness: draw randomness: %w", err)
	}
	return int(v.Int64()), nil
}

// NewNonce returns a random value that binds frames to one session.
func NewNonce(entropy io.Reader) (string, error) {
	if entropy == nil {
		entropy = rand.Reader
	}

	var b [16]byte
	if _, err := io.ReadFull(entropy, b[:]); err != nil {
		return "", fmt.Errorf("liveness: generate nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
