package biometric

import (
	"errors"
	"math"
	"testing"
)

// unitVector builds a normalised embedding pointing along one axis.
func unitVector(axis int) Embedding {
	e := make(Embedding, EmbeddingDim)
	e[axis] = 1
	return e
}

// rampVector builds a deterministic non-trivial embedding.
func rampVector(offset float64) Embedding {
	e := make(Embedding, EmbeddingDim)
	for i := range e {
		e[i] = float32(math.Sin(float64(i)*0.05 + offset))
	}
	return e
}

func TestEmbeddingNormalize(t *testing.T) {
	e := rampVector(0)

	got := e.Normalize()
	if n := got.Norm(); math.Abs(n-1) > 1e-6 {
		t.Errorf("norm after Normalize = %.9f, want 1", n)
	}

	// Direction must be preserved: normalising only changes the length.
	if c := e.Cosine(got); math.Abs(c-1) > 1e-6 {
		t.Errorf("cosine with the original = %.9f, want 1", c)
	}
}

// A zero vector has no direction. Dividing by its norm would produce NaNs that
// then compare false against every threshold, so it comes back unchanged and
// Validate is what reports the problem.
func TestEmbeddingNormalizeLeavesZeroAlone(t *testing.T) {
	zero := make(Embedding, EmbeddingDim)

	got := zero.Normalize()
	for i, v := range got {
		if v != 0 {
			t.Fatalf("element %d = %v, want 0", i, v)
		}
	}
	if err := zero.Validate(); !errors.Is(err, ErrEmbeddingShape) {
		t.Errorf("Validate() error = %v, want ErrEmbeddingShape", err)
	}
}

func TestEmbeddingCosine(t *testing.T) {
	tests := []struct {
		name string
		a, b Embedding
		want float64
	}{
		{"identical", rampVector(0).Normalize(), rampVector(0).Normalize(), 1},
		{"orthogonal", unitVector(0), unitVector(1), 0},
		{"opposite", unitVector(0), negate(unitVector(0)), -1},
		{"mismatched length", unitVector(0), make(Embedding, 8), 0},
		{"empty", Embedding{}, Embedding{}, 0},
		{"zero vector", unitVector(0), make(Embedding, EmbeddingDim), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.Cosine(tt.b)
			if math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("Cosine() = %.9f, want %.9f", got, tt.want)
			}
			// Cosine must be symmetric, or a match would depend on argument
			// order.
			if rev := tt.b.Cosine(tt.a); math.Abs(got-rev) > 1e-9 {
				t.Errorf("Cosine is not symmetric: %.9f then %.9f", got, rev)
			}
		})
	}
}

// Scaling either side must not change the similarity. Embeddings arrive from
// the model, the database, and tests, and an unnormalised one would otherwise
// score above 1 and pass every match threshold.
func TestEmbeddingCosineIgnoresMagnitude(t *testing.T) {
	a := rampVector(0)
	b := rampVector(0.3)
	want := a.Cosine(b)

	for _, scale := range []float32{0.001, 0.5, 2, 1000} {
		scaled := make(Embedding, len(a))
		for i, v := range a {
			scaled[i] = v * scale
		}

		if got := scaled.Cosine(b); math.Abs(got-want) > 1e-5 {
			t.Errorf("scaled by %g: cosine = %.9f, want %.9f", scale, got, want)
		}
	}
}

func TestEmbeddingCosineStaysInRange(t *testing.T) {
	for _, tc := range []struct{ a, b Embedding }{
		{rampVector(0), rampVector(0)},
		{rampVector(0), negate(rampVector(0))},
		{rampVector(0), rampVector(1.7)},
	} {
		got := tc.a.Cosine(tc.b)
		if got < -1 || got > 1 {
			t.Errorf("cosine = %.9f, outside [-1,1]", got)
		}
	}
}

func TestEmbeddingValidate(t *testing.T) {
	nan := rampVector(0)
	nan[42] = float32(math.NaN())

	inf := rampVector(0)
	inf[7] = float32(math.Inf(1))

	tests := []struct {
		name     string
		e        Embedding
		wantErr  bool
		wantHint string
	}{
		{"valid", rampVector(0).Normalize(), false, ""},
		{"too short", make(Embedding, 128), true, "128 dimensions"},
		{"too long", make(Embedding, 1024), true, "1024 dimensions"},
		{"nil", nil, true, "0 dimensions"},
		{"contains NaN", nan, true, "element 42"},
		{"contains Inf", inf, true, "element 7"},
		{"all zero", make(Embedding, EmbeddingDim), true, "magnitude"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.e.Validate()

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Validate() rejected a valid embedding: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("Validate() accepted an invalid embedding, want an error mentioning %q", tt.wantHint)
			}
			if !errors.Is(err, ErrEmbeddingShape) {
				t.Errorf("error does not wrap ErrEmbeddingShape: %v", err)
			}
			if tt.wantHint != "" && !contains(err.Error(), tt.wantHint) {
				t.Errorf("error does not mention %q: %v", tt.wantHint, err)
			}
		})
	}
}

func negate(e Embedding) Embedding {
	out := make(Embedding, len(e))
	for i, v := range e {
		out[i] = -v
	}
	return out
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
