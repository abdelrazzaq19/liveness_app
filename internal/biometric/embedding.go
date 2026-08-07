package biometric

import (
	"errors"
	"fmt"
	"math"
)

// EmbeddingDim is the length of a face embedding.
const EmbeddingDim = 512

// ErrEmbeddingShape means an embedding is the wrong length or is not a usable
// vector.
var ErrEmbeddingShape = errors.New("biometric: malformed embedding")

// Embedding is an L2-normalised face descriptor.
//
// Normalisation is part of the contract, not an optimisation: it makes the
// cosine similarity a plain dot product, and it means a stored vector can be
// compared against a fresh one without either side having to remember how the
// other was scaled.
type Embedding []float32

// Validate reports whether the embedding is usable.
func (e Embedding) Validate() error {
	if len(e) != EmbeddingDim {
		return fmt.Errorf("%w: %d dimensions, want %d", ErrEmbeddingShape, len(e), EmbeddingDim)
	}
	for i, v := range e {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return fmt.Errorf("%w: element %d is %v", ErrEmbeddingShape, i, v)
		}
	}
	if e.Norm() < 1e-6 {
		return fmt.Errorf("%w: vector has no magnitude", ErrEmbeddingShape)
	}
	return nil
}

// Norm returns the Euclidean length.
func (e Embedding) Norm() float64 {
	var sum float64
	for _, v := range e {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum)
}

// Normalize returns the unit-length version of the embedding.
//
// A zero vector is returned unchanged rather than producing NaNs: the caller
// finds out from Validate, not from a similarity score that silently compares
// false against every threshold.
func (e Embedding) Normalize() Embedding {
	n := e.Norm()
	if n < 1e-12 {
		return e
	}

	out := make(Embedding, len(e))
	for i, v := range e {
		out[i] = float32(float64(v) / n)
	}
	return out
}

// Cosine returns the cosine similarity with another embedding, in [-1,1].
//
// It divides by the norms rather than assuming both sides are unit length.
// Embeddings arrive here from the model, from the database, and from tests, and
// an unnormalised one would otherwise produce a similarity above 1 that quietly
// passes every match threshold.
func (e Embedding) Cosine(other Embedding) float64 {
	if len(e) != len(other) || len(e) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range e {
		a := float64(e[i])
		b := float64(other[i])
		dot += a * b
		normA += a * a
		normB += b * b
	}

	if normA < 1e-24 || normB < 1e-24 {
		return 0
	}
	return math.Max(-1, math.Min(1, dot/(math.Sqrt(normA)*math.Sqrt(normB))))
}
