package calibrate

import (
	"errors"
	"math"
	"testing"
)

// samples builds a measurement set from two lists.
func samples(genuine, attacks []float64) []Measurement {
	var out []Measurement
	for _, v := range genuine {
		out = append(out, Measurement{Label: Genuine, Value: v})
	}
	for _, v := range attacks {
		out = append(out, Measurement{Label: Attack, Value: v})
	}
	return out
}

// A sweep with one class is refused rather than answered.
//
// A curve computed from genuine samples alone produces a confident-looking
// threshold that has never seen an attack, which is worse than no threshold
// because somebody would deploy it.
func TestSweepRefusesOneSidedData(t *testing.T) {
	tests := []struct {
		name string
		ms   []Measurement
	}{
		{"genuine only", samples([]float64{0.9, 0.8}, nil)},
		{"attacks only", samples(nil, []float64{0.1, 0.2})},
		{"nothing", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Sweep("liveness", tt.ms, HigherIsGenuine); !errors.Is(err, ErrNotEnoughData) {
				t.Errorf("error = %v, want ErrNotEnoughData", err)
			}
		})
	}
}

// Two classes that do not overlap have a threshold between them where both
// rates are zero.
func TestPerfectSeparationIsFound(t *testing.T) {
	res, err := Sweep("liveness",
		samples([]float64{0.90, 0.92, 0.95}, []float64{0.10, 0.12, 0.15}),
		HigherIsGenuine)
	if err != nil {
		t.Fatalf("Sweep() returned an unexpected error: %v", err)
	}

	if !res.Separable() {
		t.Error("two classes with a gap between them were not reported as separable")
	}

	got, err := res.AtFAR(0)
	if err != nil {
		t.Fatalf("AtFAR(0) returned an unexpected error: %v", err)
	}
	if got.FRR != 0 {
		t.Errorf("FRR at a zero-FAR threshold = %.4f, want 0", got.FRR)
	}
	if got.Threshold <= 0.15 || got.Threshold > 0.90 {
		t.Errorf("threshold = %.4f, want it between the two clusters", got.Threshold)
	}
}

// The measured case: the classes overlap, so every threshold trades one error
// for the other.
func TestOverlappingClassesTradeOneErrorForTheOther(t *testing.T) {
	res, err := Sweep("liveness",
		samples(
			[]float64{0.40, 0.50, 0.55, 0.60, 0.70},
			[]float64{0.30, 0.45, 0.52, 0.58, 0.65},
		),
		HigherIsGenuine)
	if err != nil {
		t.Fatalf("Sweep() returned an unexpected error: %v", err)
	}

	if res.Separable() {
		t.Error("overlapping classes were reported as separable")
	}

	// Tightening the threshold must never make both rates worse; that would
	// mean the curve is not monotone and the sweep is wrong.
	for i := 1; i < len(res.Points); i++ {
		prev, cur := res.Points[i-1], res.Points[i]
		if cur.FAR > prev.FAR && cur.FRR > prev.FRR {
			t.Errorf("both rates rose between threshold %.4f and %.4f: FAR %.3f→%.3f, FRR %.3f→%.3f",
				prev.Threshold, cur.Threshold, prev.FAR, cur.FAR, prev.FRR, cur.FRR)
		}
	}

	// And the equal error rate sits between the two extremes rather than at one
	// of them.
	if res.EERRate <= 0 || res.EERRate >= 1 {
		t.Errorf("EER = %.4f, want it strictly between 0 and 1 for overlapping classes", res.EERRate)
	}
}

// Security first: the target FAR is a constraint to satisfy, and usability is
// maximised only inside it. The reverse gives a threshold that is pleasant and
// porous.
func TestAtFARMeetsTheConstraintBeforeOptimisingComfort(t *testing.T) {
	res, err := Sweep("liveness",
		samples(
			[]float64{0.50, 0.60, 0.70, 0.80, 0.90},
			[]float64{0.10, 0.20, 0.55, 0.65, 0.75},
		),
		HigherIsGenuine)
	if err != nil {
		t.Fatalf("Sweep() returned an unexpected error: %v", err)
	}

	got, err := res.AtFAR(0.20)
	if err != nil {
		t.Fatalf("AtFAR() returned an unexpected error: %v", err)
	}

	if got.FAR > 0.20 {
		t.Errorf("FAR = %.4f, above the 0.20 target", got.FAR)
	}

	// No point that also meets the target may reject fewer genuine people.
	for _, p := range res.Points {
		if p.FAR <= 0.20 && p.FRR < got.FRR {
			t.Errorf("threshold %.4f meets the target with FRR %.4f, better than the chosen %.4f",
				p.Threshold, p.FRR, got.FRR)
		}
	}
}

// Refusing everybody has a false accept rate of zero, so a naive search always
// "succeeds" — by recommending the threshold that turns away every genuine
// subject.
//
// That is the failure this project has hit four times. A calibration tool that
// answers it with a number is worse than one that answers it with nothing.
func TestAnUnreachableTargetIsRefusedWithTheReason(t *testing.T) {
	// Classes that overlap completely: no threshold separates them at all.
	res, err := Sweep("liveness",
		samples([]float64{0.5, 0.5, 0.5}, []float64{0.5, 0.5, 0.5}),
		HigherIsGenuine)
	if err != nil {
		t.Fatalf("Sweep() returned an unexpected error: %v", err)
	}

	_, err = res.AtFAR(0.0001)
	if err == nil {
		t.Fatal("AtFAR() returned a threshold for a target the data cannot reach")
	}
	if got := err.Error(); !contains(got, "rejecting") && !contains(got, "cannot separate") {
		t.Errorf("the refusal does not explain why: %v", err)
	}
}

// Some metrics run the other way: a distance is genuine when it is small.
func TestLowerIsGenuineFlipsTheComparison(t *testing.T) {
	res, err := Sweep("distance",
		samples([]float64{0.05, 0.08, 0.10}, []float64{0.60, 0.70, 0.80}),
		LowerIsGenuine)
	if err != nil {
		t.Fatalf("Sweep() returned an unexpected error: %v", err)
	}

	got, err := res.AtFAR(0)
	if err != nil {
		t.Fatalf("AtFAR(0) returned an unexpected error: %v", err)
	}
	if got.FRR != 0 {
		t.Errorf("FRR = %.4f, want 0 for cleanly separated classes", got.FRR)
	}
	if got.Threshold < 0.10 || got.Threshold >= 0.60 {
		t.Errorf("threshold = %.4f, want it between the clusters", got.Threshold)
	}
}

// The thresholds tried are the observed values, not a grid. A grid either
// misses where the curve moves or spends most of its samples where nothing
// changes.
func TestCandidateThresholdsComeFromTheData(t *testing.T) {
	values := []float64{0.10, 0.42, 0.87}
	res, err := Sweep("liveness", samples(values, []float64{0.05}), HigherIsGenuine)
	if err != nil {
		t.Fatalf("Sweep() returned an unexpected error: %v", err)
	}

	for _, v := range values {
		found := false
		for _, p := range res.Points {
			if p.Threshold == v {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the observed value %.2f is not among the thresholds tried", v)
		}
	}

	// And the ends, so "accept everything" and "accept nothing" are on the
	// curve rather than being whatever happened to be measured.
	first, last := res.Points[0], res.Points[len(res.Points)-1]
	if first.FRR != 0 {
		t.Errorf("the loosest threshold rejects %.4f of genuine attempts, want 0", first.FRR)
	}
	if last.FAR != 0 {
		t.Errorf("the tightest threshold admits %.4f of attacks, want 0", last.FAR)
	}
}

// The bundled anti-spoof model, as measured. This is not a hypothetical: the
// live class scored 0.006 on a real session against a 0.80 threshold.
//
// The harness has to say plainly that no threshold works, rather than producing
// one that looks usable.
func TestTheMeasuredAntiSpoofFailureIsReportedAsUnreachable(t *testing.T) {
	// Real faces and non-faces alike score near zero, which is what was
	// actually observed.
	res, err := Sweep("liveness",
		samples(
			[]float64{0.0060, 0.0062, 0.0061, 0.0064, 0.0060},
			[]float64{0.0050, 0.0054, 0.0051, 0.0078, 0.0044},
		),
		HigherIsGenuine)
	if err != nil {
		t.Fatalf("Sweep() returned an unexpected error: %v", err)
	}

	t.Logf("EER %.3f at threshold %.6f", res.EERRate, res.EER.Threshold)

	if res.Separable() {
		t.Error("a metric that scores both classes identically was called separable")
	}
	// No claim about the exact EER: these are five samples per class, and a
	// number computed from ten measurements is a description of ten
	// measurements. What matters is the next assertion.

	// And the threshold the service ships with rejects everybody.
	shipped := 0.80
	var frr float64
	for _, p := range res.Points {
		if p.Threshold >= shipped {
			frr = p.FRR
			break
		}
	}
	if frr < 1 && len(res.Points) > 0 {
		// Nothing measured reaches 0.80, so the tightest point stands in.
		frr = res.Points[len(res.Points)-1].FRR
	}
	if frr != 1 {
		t.Errorf("at the shipped threshold the false reject rate is %.4f, want 1: "+
			"the measurement says it rejects every genuine subject", frr)
	}
}

func TestRatesAreFractionsNotCounts(t *testing.T) {
	res, err := Sweep("liveness",
		samples([]float64{0.9, 0.9, 0.9, 0.9}, []float64{0.1, 0.1}),
		HigherIsGenuine)
	if err != nil {
		t.Fatalf("Sweep() returned an unexpected error: %v", err)
	}

	for _, p := range res.Points {
		if p.FAR < 0 || p.FAR > 1 || math.IsNaN(p.FAR) {
			t.Errorf("FAR = %v at threshold %.4f", p.FAR, p.Threshold)
		}
		if p.FRR < 0 || p.FRR > 1 || math.IsNaN(p.FRR) {
			t.Errorf("FRR = %v at threshold %.4f", p.FRR, p.Threshold)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
