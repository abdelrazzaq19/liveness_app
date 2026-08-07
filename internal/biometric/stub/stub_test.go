package stub

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// scene builds a deterministic frame. seed shifts the content so that two
// scenes differ in a controlled way.
func scene(w, h int, seed float64) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		fy := float64(y) / float64(h)
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(w)
			v := 0.5 + 0.25*math.Sin(2*math.Pi*(3*fx+seed)) + 0.2*math.Sin(2*math.Pi*(5*fy+seed))
			g := uint8(clamp(30+v*180, 0, 255))
			img.SetRGBA(x, y, color.RGBA{R: g, G: g, B: g, A: 255})
		}
	}
	return img
}

func halves(w, h int, top, bottom uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		v := top
		if y >= h/2 {
			v = bottom
		}
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

func sides(w, h int, left, right uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := left
			if x >= w/2 {
				v = right
			}
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

func analyze(t *testing.T, p *Pipeline, img image.Image) biometric.Face {
	t.Helper()

	face, err := p.Analyze(context.Background(), img, biometric.AnalyzeOptions{})
	if err != nil {
		t.Fatalf("Analyze() returned an unexpected error: %v", err)
	}
	return face
}

// The property everything downstream relies on: the same frame always produces
// the same result, so a session replayed in a test behaves the same way twice.
func TestAnalyzeIsDeterministic(t *testing.T) {
	var p Pipeline
	img := scene(320, 400, 0)

	first := analyze(t, &p, img)
	for i := 0; i < 4; i++ {
		got := analyze(t, &p, img)

		if got.Box != first.Box || got.EAR != first.EAR || got.MAR != first.MAR ||
			got.Pose != first.Pose || got.LivenessScore != first.LivenessScore {
			t.Fatalf("run %d differs from the first:\n got %+v\nfirst %+v", i, got, first)
		}
		if c := first.Embedding.Cosine(got.Embedding); math.Abs(c-1) > 1e-12 {
			t.Errorf("run %d: embedding cosine with the first = %.15f, want 1", i, c)
		}
	}
}

// The stub's embedding must behave the way a real one does: close for two
// captures of the same scene, far apart for different ones. A hash-derived
// vector would give exactly the opposite, and the identity-consistency check
// would then be untestable without models.
func TestEmbeddingTracksSceneContent(t *testing.T) {
	var p Pipeline

	base := analyze(t, &p, scene(320, 400, 0)).Embedding
	same := analyze(t, &p, scene(320, 400, 0)).Embedding
	nudged := analyze(t, &p, scene(320, 400, 0.01)).Embedding
	different := analyze(t, &p, scene(320, 400, 0.5)).Embedding

	identical := base.Cosine(same)
	near := base.Cosine(nudged)
	far := base.Cosine(different)

	t.Logf("identical %.6f  nudged %.6f  different %.6f", identical, near, far)

	if math.Abs(identical-1) > 1e-12 {
		t.Errorf("the same scene scored %.12f against itself, want 1", identical)
	}
	if near <= far {
		t.Errorf("a slightly changed scene scored %.4f but a different one scored %.4f; "+
			"the embedding does not track content", near, far)
	}
	if err := base.Validate(); err != nil {
		t.Errorf("the stub produced an invalid embedding: %v", err)
	}
}

// The stub is meant to be drivable by a person in front of a webcam, so the
// proxies have to actually respond to the frame.
func TestProxiesRespondToTheFrame(t *testing.T) {
	var p Pipeline

	t.Run("darkening the eye band lowers the blink metric", func(t *testing.T) {
		bright := analyze(t, &p, halves(320, 400, 220, 60)).EAR
		dark := analyze(t, &p, halves(320, 400, 20, 60)).EAR

		if !(dark < bright) {
			t.Errorf("EAR with a dark upper band = %.4f, with a bright one = %.4f; want the dark one lower",
				dark, bright)
		}
	})

	t.Run("brightness shifting sideways moves the yaw", func(t *testing.T) {
		left := analyze(t, &p, sides(320, 400, 240, 30)).Pose.Yaw
		right := analyze(t, &p, sides(320, 400, 30, 240)).Pose.Yaw

		if !(left < right) {
			t.Errorf("yaw with the light on the left = %.2f, on the right = %.2f; want the left one smaller",
				left, right)
		}
	})

	t.Run("a flat frame scores low for liveness", func(t *testing.T) {
		flat := analyze(t, &p, halves(320, 400, 128, 128)).LivenessScore
		textured := analyze(t, &p, scene(320, 400, 0)).LivenessScore

		if !(flat < textured) {
			t.Errorf("liveness of a flat frame = %.4f, of a textured one = %.4f; want the flat one lower",
				flat, textured)
		}
	})
}

// Every value must be in range whatever the frame, or a stub session would
// produce numbers no real one could and mask a bug downstream.
func TestValuesStayInRange(t *testing.T) {
	var p Pipeline

	frames := []struct {
		name string
		img  image.Image
	}{
		{"textured", scene(320, 400, 0)},
		{"pure black", halves(64, 64, 0, 0)},
		{"pure white", halves(64, 64, 255, 255)},
		{"single pixel", halves(1, 1, 128, 128)},
		{"very wide", scene(800, 40, 0.2)},
		{"very tall", scene(40, 800, 0.7)},
	}

	for _, f := range frames {
		t.Run(f.name, func(t *testing.T) {
			face := analyze(t, &p, f.img)

			for _, c := range []struct {
				name   string
				value  float64
				lo, hi float64
			}{
				{"EAR", face.EAR, 0, 1},
				{"MAR", face.MAR, 0, 1},
				{"liveness", face.LivenessScore, 0, 1},
				{"yaw", face.Pose.Yaw, -45, 45},
				{"pitch", face.Pose.Pitch, -30, 30},
				{"roll", face.Pose.Roll, -20, 20},
				{"score", face.Score, 0, 1},
			} {
				if math.IsNaN(c.value) || math.IsInf(c.value, 0) {
					t.Errorf("%s = %v", c.name, c.value)
					continue
				}
				if c.value < c.lo || c.value > c.hi {
					t.Errorf("%s = %.4f, outside [%.0f, %.0f]", c.name, c.value, c.lo, c.hi)
				}
			}

			if face.Box.Width() <= 0 || face.Box.Height() <= 0 {
				t.Errorf("box %+v has no area", face.Box)
			}
			if err := face.Embedding.Validate(); err != nil {
				t.Errorf("invalid embedding: %v", err)
			}
		})
	}
}

// The landmarks must be structurally sane, since code downstream reads them by
// index and would otherwise get a cloud of points that happens to parse.
func TestLandmarksAreStructurallySane(t *testing.T) {
	var p Pipeline
	face := analyze(t, &p, scene(480, 640, 0))

	leftEye := face.Landmarks.EyeCenter(biometric.LeftEye)
	rightEye := face.Landmarks.EyeCenter(biometric.RightEye)
	mouth := face.Landmarks.MouthCenter()

	if leftEye.X >= rightEye.X {
		t.Errorf("left eye at x=%.1f is not left of the right eye at x=%.1f", leftEye.X, rightEye.X)
	}
	if mouth.Y <= leftEye.Y {
		t.Errorf("mouth at y=%.1f is not below the eyes at y=%.1f", mouth.Y, leftEye.Y)
	}

	// Everything inside the reported box, give or take rounding.
	bounds := face.Landmarks.Bounds()
	if bounds.MinX < face.Box.MinX-1 || bounds.MaxX > face.Box.MaxX+1 ||
		bounds.MinY < face.Box.MinY-1 || bounds.MaxY > face.Box.MaxY+1 {
		t.Errorf("landmark bounds %+v escape the box %+v", bounds, face.Box)
	}

	// The five coarse keypoints must agree with the dense ones.
	if math.Abs(face.Keypoints[biometric.KeypointLeftEye].X-leftEye.X) > face.Box.Width()*0.15 {
		t.Errorf("coarse left eye at %.1f disagrees with the dense one at %.1f",
			face.Keypoints[biometric.KeypointLeftEye].X, leftEye.X)
	}
}

func TestSkipEmbeddingLeavesTheDescriptorNil(t *testing.T) {
	var p Pipeline

	face, err := p.Analyze(context.Background(), scene(320, 320, 0), biometric.AnalyzeOptions{SkipEmbedding: true})
	if err != nil {
		t.Fatalf("Analyze() returned an unexpected error: %v", err)
	}
	if face.Embedding != nil {
		t.Errorf("embedding = %v, want nil", face.Embedding[:4])
	}
	if face.EAR <= 0 {
		t.Error("skipping the embedding also skipped the cheap metrics")
	}
}

func TestAnalyzeRejectsUnusableFrames(t *testing.T) {
	var p Pipeline

	tests := []struct {
		name string
		img  image.Image
	}{
		{"nil", nil},
		{"zero sized", image.NewRGBA(image.Rect(0, 0, 0, 0))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.Analyze(context.Background(), tt.img, biometric.AnalyzeOptions{})
			if !errors.Is(err, biometric.ErrNoFaceFound) {
				t.Errorf("Analyze() error = %v, want ErrNoFaceFound", err)
			}
		})
	}
}

func TestFaceFractionControlsTheBoxSize(t *testing.T) {
	small := analyze(t, &Pipeline{FaceFraction: 0.25}, scene(400, 400, 0))
	large := analyze(t, &Pipeline{FaceFraction: 0.9}, scene(400, 400, 0))

	if !(small.Box.Width() < large.Box.Width()) {
		t.Errorf("box widths %.0f and %.0f do not follow FaceFraction",
			small.Box.Width(), large.Box.Width())
	}

	// Out-of-range values fall back to the default rather than producing an
	// inverted or absent box.
	for _, fraction := range []float64{0, -1, 5} {
		got := analyze(t, &Pipeline{FaceFraction: fraction}, scene(400, 400, 0))
		if got.Box.Width() <= 0 {
			t.Errorf("FaceFraction %.1f produced a box with no area", fraction)
		}
	}
}

// The stub must satisfy the same interface the real pipeline does, or it cannot
// stand in for it.
func TestStubImplementsAnalyzer(t *testing.T) {
	var _ biometric.Analyzer = (*Pipeline)(nil)
}
