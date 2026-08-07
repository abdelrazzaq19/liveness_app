package liveness

import (
	"crypto/rand"
	"errors"
	"testing"
)

func TestNewChallengeSetDrawsDistinctKinds(t *testing.T) {
	for count := 1; count <= len(AllChallenges); count++ {
		got, err := NewChallengeSet(rand.Reader, count)
		if err != nil {
			t.Fatalf("count %d: NewChallengeSet() returned an unexpected error: %v", count, err)
		}
		if len(got) != count {
			t.Fatalf("count %d: drew %d challenges", count, len(got))
		}

		seen := make(map[ChallengeKind]bool, count)
		for _, c := range got {
			if !c.Valid() {
				t.Errorf("drew %q, which is not a known challenge", c)
			}
			if seen[c] {
				t.Errorf("drew %s twice", c)
			}
			seen[c] = true
		}
	}
}

func TestNewChallengeSetRejectsImpossibleCounts(t *testing.T) {
	for _, count := range []int{0, -1, len(AllChallenges) + 1, 100} {
		if _, err := NewChallengeSet(rand.Reader, count); !errors.Is(err, ErrChallengeCount) {
			t.Errorf("count %d: error = %v, want ErrChallengeCount", count, err)
		}
	}
}

// The unpredictable order is the defence against a pre-recorded video. A
// generator that returned the same sequence every time would look correct in
// every other test and be worthless in front of an attacker.
func TestChallengeOrderVaries(t *testing.T) {
	const draws = 200

	orders := make(map[string]int, draws)
	firsts := make(map[ChallengeKind]int, len(AllChallenges))

	for i := 0; i < draws; i++ {
		got, err := NewChallengeSet(rand.Reader, 3)
		if err != nil {
			t.Fatalf("NewChallengeSet() returned an unexpected error: %v", err)
		}

		key := ""
		for _, c := range got {
			key += string(c) + "|"
		}
		orders[key]++
		firsts[got[0]]++
	}

	// 5 kinds taken 3 at a time in order gives 60 possibilities. Seeing only a
	// handful across 200 draws would mean the shuffle is not shuffling.
	if len(orders) < 20 {
		t.Errorf("200 draws produced only %d distinct sequences; the order is not random enough", len(orders))
	}

	// Every kind should be able to come first.
	for _, kind := range AllChallenges {
		if firsts[kind] == 0 {
			t.Errorf("%s never appeared first across %d draws", kind, draws)
		}
	}
}

// Shuffling the whole pool and taking a prefix keeps every subset equally
// likely; sampling without that step biases towards the earlier entries.
func TestEveryChallengeCanBeDrawn(t *testing.T) {
	seen := make(map[ChallengeKind]int, len(AllChallenges))

	for i := 0; i < 300; i++ {
		got, err := NewChallengeSet(rand.Reader, 2)
		if err != nil {
			t.Fatalf("NewChallengeSet() returned an unexpected error: %v", err)
		}
		for _, c := range got {
			seen[c]++
		}
	}

	for _, kind := range AllChallenges {
		if seen[kind] == 0 {
			t.Errorf("%s was never drawn across 300 attempts", kind)
		}
	}
}

func TestChallengeKindValid(t *testing.T) {
	for _, kind := range AllChallenges {
		if !kind.Valid() {
			t.Errorf("%s is in AllChallenges but reports itself invalid", kind)
		}
	}
	for _, kind := range []ChallengeKind{"", "SMILE", "blink", "TURN"} {
		if kind.Valid() {
			t.Errorf("%q reports itself valid", kind)
		}
	}
}

func TestNewNonceIsUniqueAndHex(t *testing.T) {
	seen := make(map[string]bool, 100)

	for i := 0; i < 100; i++ {
		got, err := NewNonce(rand.Reader)
		if err != nil {
			t.Fatalf("NewNonce() returned an unexpected error: %v", err)
		}
		if len(got) != 32 {
			t.Fatalf("nonce %q is %d characters, want 32", got, len(got))
		}
		for _, c := range got {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Fatalf("nonce %q contains a non-hex character %q", got, c)
			}
		}
		if seen[got] {
			t.Fatalf("nonce %q was generated twice", got)
		}
		seen[got] = true
	}
}

// A failing entropy source must be an error, never a predictable fallback: the
// whole security value of the shuffle is that it cannot be guessed.
func TestGeneratorsFailWhenEntropyFails(t *testing.T) {
	if _, err := NewChallengeSet(failingReader{}, 3); err == nil {
		t.Error("NewChallengeSet() succeeded with a broken entropy source")
	}
	if _, err := NewNonce(failingReader{}); err == nil {
		t.Error("NewNonce() succeeded with a broken entropy source")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }
