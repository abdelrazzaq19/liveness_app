// Package calibrate turns measured biometric metrics into thresholds.
//
// It exists because every threshold this system ships with is a guess. Some are
// literature figures for a different landmark scheme; some come from a single
// session with a single person. The one honest way to set them is to measure
// both classes — genuine attempts and attacks — and read the operating point
// off the curve. This package does the reading.
//
// It holds no images and no models. It takes measurements, which are scalars,
// and returns a threshold. That separation is what lets the sweep be tested
// exhaustively without a dataset, and what keeps a calibration run reproducible
// from a file somebody can inspect.
package calibrate

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// Label says which class a measurement belongs to.
type Label string

const (
	// Genuine is a real person making an honest attempt.
	Genuine Label = "genuine"

	// Attack is a presentation attack: a printed photograph, a replayed
	// screen, a mask.
	Attack Label = "attack"
)

// Measurement is one observation of one metric.
//
// Deliberately not a whole frame or a whole session. A calibration file is
// something an operator will look at, share with a colleague, and keep — so it
// holds numbers and a label, never a face.
type Measurement struct {
	Label Label   `json:"label"`
	Value float64 `json:"value"`

	// Source is where it came from, for tracing a surprising number back to the
	// capture that produced it. A file name or a session id, never a subject.
	Source string `json:"source,omitempty"`
}

// Direction says which side of the threshold counts as genuine.
type Direction int

const (
	// HigherIsGenuine suits a liveness score: a real face scores high.
	HigherIsGenuine Direction = iota

	// LowerIsGenuine suits a distance: a real face scores close to zero.
	LowerIsGenuine
)

// Point is the error rate pair at one threshold.
type Point struct {
	Threshold float64 `json:"threshold"`

	// FAR is the false accept rate: attacks that got through. This is the one
	// that costs money and trust.
	FAR float64 `json:"far"`

	// FRR is the false reject rate: genuine people turned away. This is the one
	// that costs users, and it is the failure this project has hit four times.
	FRR float64 `json:"frr"`
}

// Result is a completed sweep.
type Result struct {
	Metric    string  `json:"metric"`
	Genuine   int     `json:"genuine_samples"`
	Attacks   int     `json:"attack_samples"`
	Points    []Point `json:"points"`
	EER       Point   `json:"eer"`
	EERRate   float64 `json:"eer_rate"`
	Direction string  `json:"direction"`
}

// ErrNotEnoughData means a sweep was asked for without both classes.
//
// Refused rather than answered with a curve computed from one class, which
// would produce a confident-looking threshold that has never seen an attack.
var ErrNotEnoughData = errors.New("calibrate: need measurements from both classes")

// Sweep computes the error curve across every threshold worth trying.
//
// The candidate thresholds are the measured values themselves rather than an
// evenly spaced grid. A grid either misses the point where the curve actually
// moves or wastes most of its samples in regions where nothing changes; the
// observed values are exactly the points where a decision can flip.
func Sweep(metric string, ms []Measurement, dir Direction) (Result, error) {
	var genuine, attacks []float64
	for _, m := range ms {
		switch m.Label {
		case Genuine:
			genuine = append(genuine, m.Value)
		case Attack:
			attacks = append(attacks, m.Value)
		default:
			return Result{}, fmt.Errorf("calibrate: unknown label %q", m.Label)
		}
	}

	if len(genuine) == 0 || len(attacks) == 0 {
		return Result{}, fmt.Errorf("%w: %d genuine and %d attack samples",
			ErrNotEnoughData, len(genuine), len(attacks))
	}

	candidates := make([]float64, 0, len(ms)+2)
	for _, m := range ms {
		candidates = append(candidates, m.Value)
	}
	// The ends, so a threshold that accepts everything or nothing is on the
	// curve too. Without them the reported extremes are whatever happened to be
	// measured, which reads as a property of the metric rather than of the
	// sample.
	lo, hi := minMax(candidates)
	candidates = append(candidates, math.Nextafter(lo, math.Inf(-1)), math.Nextafter(hi, math.Inf(1)))

	sort.Float64s(candidates)
	candidates = dedupe(candidates)

	res := Result{
		Metric:    metric,
		Genuine:   len(genuine),
		Attacks:   len(attacks),
		Direction: directionName(dir),
	}

	for _, t := range candidates {
		res.Points = append(res.Points, Point{
			Threshold: t,
			FAR:       rate(attacks, t, dir, accepted),
			FRR:       rate(genuine, t, dir, rejected),
		})
	}

	res.EER, res.EERRate = equalErrorPoint(res.Points)
	return res, nil
}

// AtFAR returns the operating point that holds the false accept rate at or
// below the target, and among those the one that rejects fewest genuine people.
//
// Ordered that way on purpose: FAR is the security constraint and is met first,
// then usability is maximised inside it. Choosing the reverse gives a threshold
// that is pleasant and porous.
func (r Result) AtFAR(target float64) (Point, error) {
	if target < 0 || target > 1 {
		return Point{}, fmt.Errorf("calibrate: target FAR must be in [0,1], got %g", target)
	}

	best := Point{}
	found := false
	for _, p := range r.Points {
		if p.FAR > target {
			continue
		}
		if !found || p.FRR < best.FRR {
			best, found = p, true
		}
	}

	if !found {
		return Point{}, fmt.Errorf("calibrate: no threshold reaches a FAR of %.4f; "+
			"the closest is %.4f, so this metric cannot separate the two classes that well",
			target, lowestFAR(r.Points))
	}

	// Refusing everybody has a false accept rate of zero, so without this the
	// search always "succeeds" — and it succeeds by recommending the threshold
	// that turns away every genuine subject.
	//
	// That is not a hypothetical failure mode. It is the one this project has
	// hit four times, most recently with an anti-spoof model that scored real
	// faces at 0.006 against a 0.80 threshold and refused every person who
	// tried. A calibration tool that answers that question with a number is
	// worse than one that answers it with nothing.
	if best.FRR >= 1 {
		return Point{}, fmt.Errorf("calibrate: a FAR of %.4f is only reachable by rejecting "+
			"every genuine subject; this metric does not separate the two classes", target)
	}
	return best, nil
}

// AtFRR returns the operating point that holds the false reject rate at or
// below the target, and among those the one that admits fewest attacks.
//
// The mirror of AtFAR, for the case where turning genuine people away is the
// cost that matters most — a self-service kiosk with no staff to fall back on,
// for instance.
func (r Result) AtFRR(target float64) (Point, error) {
	if target < 0 || target > 1 {
		return Point{}, fmt.Errorf("calibrate: target FRR must be in [0,1], got %g", target)
	}

	best := Point{}
	found := false
	for _, p := range r.Points {
		if p.FRR > target {
			continue
		}
		if !found || p.FAR < best.FAR {
			best, found = p, true
		}
	}

	if !found {
		return Point{}, fmt.Errorf("calibrate: no threshold reaches an FRR of %.4f", target)
	}
	return best, nil
}

// Separable reports whether any threshold gets both rates to zero.
//
// A metric that separates perfectly on the sample almost never separates
// perfectly in the world, so this is a warning that the sample is too small or
// too clean rather than good news.
func (r Result) Separable() bool {
	for _, p := range r.Points {
		if p.FAR == 0 && p.FRR == 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- internals

type verdict int

const (
	accepted verdict = iota
	rejected
)

// rate is the fraction of values that the threshold treats the given way.
func rate(values []float64, threshold float64, dir Direction, want verdict) float64 {
	if len(values) == 0 {
		return 0
	}

	var n int
	for _, v := range values {
		if accepts(v, threshold, dir) == (want == accepted) {
			n++
		}
	}
	return float64(n) / float64(len(values))
}

// accepts reports whether a value passes the threshold.
//
// The comparison is inclusive on the genuine side, matching how every threshold
// in this codebase is written: a score exactly at the minimum passes.
func accepts(v, threshold float64, dir Direction) bool {
	if dir == LowerIsGenuine {
		return v <= threshold
	}
	return v >= threshold
}

// equalErrorPoint finds where the two curves cross.
//
// The EER is not a recommendation. It is the single number that summarises how
// well a metric separates two classes at all, which is what tells you whether a
// target is reachable before you go looking for the threshold that reaches it.
func equalErrorPoint(points []Point) (Point, float64) {
	best := Point{}
	bestGap := math.Inf(1)
	bestRate := 0.0

	for _, p := range points {
		gap := math.Abs(p.FAR - p.FRR)
		if gap < bestGap {
			best, bestGap, bestRate = p, gap, (p.FAR+p.FRR)/2
		}
	}
	return best, bestRate
}

func lowestFAR(points []Point) float64 {
	lowest := math.Inf(1)
	for _, p := range points {
		lowest = math.Min(lowest, p.FAR)
	}
	return lowest
}

func minMax(vs []float64) (float64, float64) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range vs {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	return lo, hi
}

func dedupe(sorted []float64) []float64 {
	if len(sorted) == 0 {
		return sorted
	}
	out := sorted[:1]
	for _, v := range sorted[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

func directionName(d Direction) string {
	if d == LowerIsGenuine {
		return "lower is genuine"
	}
	return "higher is genuine"
}
