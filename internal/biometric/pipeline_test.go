package biometric

import (
	"context"
	"errors"
	"image"
	"image/color"
	"strings"
	"testing"
)

// --------------------------------------------------------------- test doubles

type fakeDetector struct {
	detection Detection
	err       error
	calls     int
}

func (f *fakeDetector) Detect(context.Context, image.Image) (Detection, error) {
	f.calls++
	return f.detection, f.err
}

type fakeLandmarker struct {
	landmarks Landmarks106
	err       error
	calls     int
}

func (f *fakeLandmarker) Landmarks(context.Context, image.Image, BBox) (Landmarks106, error) {
	f.calls++
	return f.landmarks, f.err
}

type fakeAntiSpoofer struct {
	score float64
	err   error
	calls int
}

func (f *fakeAntiSpoofer) LivenessScore(context.Context, image.Image, BBox) (float64, error) {
	f.calls++
	return f.score, f.err
}

type fakeEmbedder struct {
	embedding Embedding
	err       error
	calls     int
}

func (f *fakeEmbedder) Embed(context.Context, image.Image, Keypoints) (Embedding, error) {
	f.calls++
	return f.embedding, f.err
}

// stages bundles the doubles so a test can assert on how far a frame got.
type stages struct {
	detector    *fakeDetector
	landmarker  *fakeLandmarker
	antiSpoofer *fakeAntiSpoofer
	embedder    *fakeEmbedder
}

func newStages() *stages {
	shape := defaultShape()
	shape.eyeOpening = 6
	shape.mouthGap = 8

	embedding := make(Embedding, EmbeddingDim)
	for i := range embedding {
		embedding[i] = float32(i%7) + 1
	}

	return &stages{
		detector: &fakeDetector{detection: Detection{
			Box:   BBox{MinX: 100, MinY: 100, MaxX: 300, MaxY: 340},
			Score: 0.93,
		}},
		landmarker:  &fakeLandmarker{landmarks: shape.build()},
		antiSpoofer: &fakeAntiSpoofer{score: 0.88},
		embedder:    &fakeEmbedder{embedding: embedding.Normalize()},
	}
}

func (s *stages) pipeline(t *testing.T, tweak func(*Pipeline)) *Pipeline {
	t.Helper()

	p := Pipeline{
		Detector:    s.detector,
		Landmarker:  s.landmarker,
		AntiSpoofer: s.antiSpoofer,
		Embedder:    s.embedder,
	}
	if tweak != nil {
		tweak(&p)
	}

	built, err := NewPipeline(p)
	if err != nil {
		t.Fatalf("NewPipeline() returned an unexpected error: %v", err)
	}
	return built
}

func testFrame() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 480, 640))
	for y := 0; y < 640; y++ {
		for x := 0; x < 480; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 90, A: 255})
		}
	}
	return img
}

// ------------------------------------------------------------------- the tests

// A pipeline missing a stage is a wiring mistake. The alternative to catching
// it here is a nil dereference on the first frame of a real session.
func TestNewPipelineRequiresEveryStage(t *testing.T) {
	full := newStages()

	tests := []struct {
		name     string
		tweak    func(*Pipeline)
		wantHint string
	}{
		{"no detector", func(p *Pipeline) { p.Detector = nil }, "Detector"},
		{"no landmarker", func(p *Pipeline) { p.Landmarker = nil }, "Landmarker"},
		{"no anti-spoofer", func(p *Pipeline) { p.AntiSpoofer = nil }, "AntiSpoofer"},
		{"no embedder", func(p *Pipeline) { p.Embedder = nil }, "Embedder"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Pipeline{
				Detector:    full.detector,
				Landmarker:  full.landmarker,
				AntiSpoofer: full.antiSpoofer,
				Embedder:    full.embedder,
			}
			tt.tweak(&p)

			_, err := NewPipeline(p)
			if err == nil {
				t.Fatalf("NewPipeline() succeeded, want an error mentioning %s", tt.wantHint)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error does not mention %s: %v", tt.wantHint, err)
			}
		})
	}

	if _, err := NewPipeline(Pipeline{}); err == nil {
		t.Error("NewPipeline() accepted an entirely empty pipeline")
	}
}

func TestAnalyzeAssemblesEveryStage(t *testing.T) {
	s := newStages()
	p := s.pipeline(t, nil)

	face, err := p.Analyze(context.Background(), testFrame(), AnalyzeOptions{})
	if err != nil {
		t.Fatalf("Analyze() returned an unexpected error: %v", err)
	}

	if face.Box != s.detector.detection.Box {
		t.Errorf("box = %+v, want %+v", face.Box, s.detector.detection.Box)
	}
	if face.Score != 0.93 {
		t.Errorf("score = %v, want 0.93", face.Score)
	}
	if face.LivenessScore != 0.88 {
		t.Errorf("liveness score = %v, want 0.88", face.LivenessScore)
	}
	if face.EAR <= 0 {
		t.Errorf("EAR = %v, want it computed from the landmarks", face.EAR)
	}
	if face.MAR <= 0 {
		t.Errorf("MAR = %v, want it computed from the landmarks", face.MAR)
	}
	if len(face.Embedding) != EmbeddingDim {
		t.Errorf("embedding has %d dimensions, want %d", len(face.Embedding), EmbeddingDim)
	}
}

// The embedder is by far the heaviest stage. Skipping it is the difference
// between a session that keeps up with the camera and one that does not, so the
// option has to actually skip it rather than merely discard the result.
func TestAnalyzeSkipsTheEmbedderWhenAsked(t *testing.T) {
	s := newStages()
	p := s.pipeline(t, nil)

	face, err := p.Analyze(context.Background(), testFrame(), AnalyzeOptions{SkipEmbedding: true})
	if err != nil {
		t.Fatalf("Analyze() returned an unexpected error: %v", err)
	}

	if s.embedder.calls != 0 {
		t.Errorf("the embedder ran %d times despite SkipEmbedding", s.embedder.calls)
	}
	// Nil rather than 512 zeros: an absent descriptor must not compare as a
	// real vector.
	if face.Embedding != nil {
		t.Errorf("embedding = %v, want nil", face.Embedding[:4])
	}

	// Everything cheaper still ran.
	if s.detector.calls != 1 || s.landmarker.calls != 1 || s.antiSpoofer.calls != 1 {
		t.Errorf("stage calls: detector %d, landmarker %d, anti-spoofer %d; want 1 each",
			s.detector.calls, s.landmarker.calls, s.antiSpoofer.calls)
	}
}

// A blurry frame must never reach a model. The gate costs nothing and the
// models cost everything.
func TestAnalyzeShortCircuitsOnLowQuality(t *testing.T) {
	s := newStages()
	p := s.pipeline(t, func(p *Pipeline) {
		p.Quality = func(image.Image) (Quality, error) {
			return Quality{}, ErrLowQuality
		}
	})

	_, err := p.Analyze(context.Background(), testFrame(), AnalyzeOptions{})
	if !errors.Is(err, ErrLowQuality) {
		t.Fatalf("Analyze() error = %v, want ErrLowQuality", err)
	}

	if s.detector.calls != 0 || s.landmarker.calls != 0 || s.antiSpoofer.calls != 0 || s.embedder.calls != 0 {
		t.Errorf("a rejected frame still reached a model: detector %d, landmarker %d, anti-spoofer %d, embedder %d",
			s.detector.calls, s.landmarker.calls, s.antiSpoofer.calls, s.embedder.calls)
	}
}

// The face-size gate needs the detection, so it runs after the detector — but
// still before the three expensive stages.
func TestAnalyzeRejectsFacesBelowTheMinimumWidth(t *testing.T) {
	s := newStages()
	s.detector.detection.Box = BBox{MinX: 0, MinY: 0, MaxX: 40, MaxY: 50}

	p := s.pipeline(t, func(p *Pipeline) { p.MinFaceWidth = 112 })

	_, err := p.Analyze(context.Background(), testFrame(), AnalyzeOptions{})
	if !errors.Is(err, ErrLowQuality) {
		t.Fatalf("Analyze() error = %v, want ErrLowQuality", err)
	}
	if !strings.Contains(err.Error(), "40 px wide") {
		t.Errorf("error does not say how wide the face was: %v", err)
	}

	if s.detector.calls != 1 {
		t.Errorf("the detector ran %d times, want 1", s.detector.calls)
	}
	if s.landmarker.calls != 0 || s.antiSpoofer.calls != 0 || s.embedder.calls != 0 {
		t.Errorf("an undersized face still reached a later model: landmarker %d, anti-spoofer %d, embedder %d",
			s.landmarker.calls, s.antiSpoofer.calls, s.embedder.calls)
	}
}

func TestAnalyzePropagatesNoFaceFound(t *testing.T) {
	s := newStages()
	s.detector.err = ErrNoFaceFound

	p := s.pipeline(t, nil)

	_, err := p.Analyze(context.Background(), testFrame(), AnalyzeOptions{})
	if !errors.Is(err, ErrNoFaceFound) {
		t.Errorf("Analyze() error = %v, want ErrNoFaceFound", err)
	}
	if s.landmarker.calls != 0 {
		t.Errorf("the landmarker ran despite no face being found")
	}
}

func TestAnalyzePropagatesStageFailures(t *testing.T) {
	sentinel := errors.New("the model exploded")

	tests := []struct {
		name   string
		break_ func(*stages)
	}{
		{"landmarker", func(s *stages) { s.landmarker.err = sentinel }},
		{"anti-spoofer", func(s *stages) { s.antiSpoofer.err = sentinel }},
		{"embedder", func(s *stages) { s.embedder.err = sentinel }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStages()
			tt.break_(s)

			p := s.pipeline(t, nil)
			if _, err := p.Analyze(context.Background(), testFrame(), AnalyzeOptions{}); !errors.Is(err, sentinel) {
				t.Errorf("Analyze() error = %v, want it to wrap the stage failure", err)
			}
		})
	}
}

// A pose that cannot be determined must not fail the frame: the turn and nod
// challenges will fail on their own, and blink and mouth still work.
func TestAnalyzeSurvivesAnUndeterminablePose(t *testing.T) {
	s := newStages()
	s.landmarker.landmarks = Landmarks106{} // every point at the origin

	p := s.pipeline(t, nil)

	face, err := p.Analyze(context.Background(), testFrame(), AnalyzeOptions{})
	if err != nil {
		t.Fatalf("Analyze() failed on collapsed landmarks: %v", err)
	}
	if face.Pose != (Pose{}) {
		t.Errorf("pose = %+v, want the zero value when it cannot be estimated", face.Pose)
	}
	if s.embedder.calls != 1 {
		t.Error("the pipeline stopped early instead of carrying on without a pose")
	}
}

func TestAnalyzeRejectsANilFrame(t *testing.T) {
	s := newStages()
	p := s.pipeline(t, nil)

	if _, err := p.Analyze(context.Background(), nil, AnalyzeOptions{}); err == nil {
		t.Error("Analyze() accepted a nil image, want an error")
	}
	if s.detector.calls != 0 {
		t.Error("a nil frame reached the detector")
	}
}

func TestAnalyzeRecordsQuality(t *testing.T) {
	s := newStages()
	want := Quality{Width: 480, Height: 640, Brightness: 120, Sharpness: 900}

	p := s.pipeline(t, func(p *Pipeline) {
		p.Quality = func(image.Image) (Quality, error) { return want, nil }
	})

	face, err := p.Analyze(context.Background(), testFrame(), AnalyzeOptions{})
	if err != nil {
		t.Fatalf("Analyze() returned an unexpected error: %v", err)
	}
	if face.Quality != want {
		t.Errorf("quality = %+v, want %+v", face.Quality, want)
	}
}
