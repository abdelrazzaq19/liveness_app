package biometric

import (
	"context"
	"errors"
	"fmt"
	"image"
)

// Quality summarises how usable a frame is, before any model has looked at it.
type Quality struct {
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Brightness float64 `json:"brightness"`
	Sharpness  float64 `json:"sharpness"`
}

// Face is everything the pipeline extracts from a single frame.
//
// Every coordinate is in the source image's pixel space. Fields that a
// particular call did not compute are left at their zero value, which is why
// Embedding is a slice: an absent descriptor is nil rather than 512 zeros that
// would compare as a real vector.
type Face struct {
	Quality Quality `json:"quality"`

	Box       BBox      `json:"box"`
	Keypoints Keypoints `json:"-"`
	Score     float64   `json:"score"`

	Landmarks Landmarks106 `json:"-"`
	Pose      Pose         `json:"pose"`

	// EAR and MAR drive the blink and mouth challenges.
	EAR float64 `json:"ear"`
	MAR float64 `json:"mar"`

	// LivenessScore is the passive anti-spoof probability, in [0,1].
	LivenessScore float64 `json:"liveness_score"`

	// Embedding is nil unless the caller asked for it.
	Embedding Embedding `json:"-"`
}

// AnalyzeOptions selects how much work a single frame is worth.
type AnalyzeOptions struct {
	// SkipEmbedding omits the descriptor.
	//
	// The embedder is by far the heaviest stage, and most frames only need the
	// challenge metrics. Identity consistency is checked on key frames, not on
	// every one, so this is the difference between a session that keeps up with
	// the camera and one that does not.
	SkipEmbedding bool
}

// Analyzer extracts a Face from a frame.
//
// The liveness and enrollment domains depend on this rather than on any model,
// which is what lets the deterministic stub stand in for the real thing.
type Analyzer interface {
	Analyze(ctx context.Context, img image.Image, opts AnalyzeOptions) (Face, error)
}

// QualityCheck measures a frame and reports whether it is worth analysing.
//
// It is a function rather than an interface so that the pipeline does not have
// to import the imaging package, which imports this one.
type QualityCheck func(img image.Image) (Quality, error)

// Pipeline runs the models in order and assembles a Face.
//
// It is the only place that knows the stages exist. Everything upstream sees a
// Face and never learns whether four models produced it or one stub did.
type Pipeline struct {
	Detector    Detector
	Landmarker  Landmarker
	AntiSpoofer AntiSpoofer
	Embedder    Embedder

	// Quality runs before any model. A nil value skips the gate.
	Quality QualityCheck

	// MinFaceWidth rejects faces too small to analyse, in source pixels. Below
	// the embedder's own input width the crop is upsampled guesswork.
	MinFaceWidth int
}

var _ Analyzer = (*Pipeline)(nil)

// NewPipeline validates that every stage is wired.
//
// Constructing an incomplete pipeline is a wiring mistake, and the alternative
// to catching it here is a nil dereference on the first frame of a real
// session.
func NewPipeline(p Pipeline) (*Pipeline, error) {
	var missing []error
	if p.Detector == nil {
		missing = append(missing, errors.New("Detector is required"))
	}
	if p.Landmarker == nil {
		missing = append(missing, errors.New("Landmarker is required"))
	}
	if p.AntiSpoofer == nil {
		missing = append(missing, errors.New("AntiSpoofer is required"))
	}
	if p.Embedder == nil {
		missing = append(missing, errors.New("Embedder is required"))
	}
	if err := errors.Join(missing...); err != nil {
		return nil, fmt.Errorf("biometric: pipeline: %w", err)
	}
	return &p, nil
}

// Analyze runs one frame through the stages.
//
// The order is deliberate and the early exits are the point: quality first
// because it costs nothing, then detection, then the cheap models, and the
// embedder last and only if asked. A blurry frame never reaches a model at all.
func (p *Pipeline) Analyze(ctx context.Context, img image.Image, opts AnalyzeOptions) (Face, error) {
	var face Face

	if img == nil {
		return face, errors.New("biometric: pipeline: image is nil")
	}

	if p.Quality != nil {
		quality, err := p.Quality(img)
		if err != nil {
			return face, err
		}
		face.Quality = quality
	}

	detection, err := p.Detector.Detect(ctx, img)
	if err != nil {
		return face, err
	}
	face.Box = detection.Box
	face.Keypoints = detection.Keypoints
	face.Score = detection.Score

	// The face-size gate sits here rather than with the other quality checks
	// because it needs the detection, but it still runs before the three
	// expensive stages.
	if p.MinFaceWidth > 0 && detection.Box.Width() < float64(p.MinFaceWidth) {
		return face, fmt.Errorf("%w: face is %.0f px wide, need %d",
			ErrLowQuality, detection.Box.Width(), p.MinFaceWidth)
	}

	landmarks, err := p.Landmarker.Landmarks(ctx, img, detection.Box)
	if err != nil {
		return face, err
	}
	face.Landmarks = landmarks
	face.EAR = landmarks.MeanEyeAspectRatio()
	face.MAR = landmarks.MouthAspectRatio()

	// A pose that cannot be determined is not fatal: the turn and nod
	// challenges will fail on their own, and blink and mouth still work.
	if pose, err := landmarks.EstimatePose(); err == nil {
		face.Pose = pose
	}

	score, err := p.AntiSpoofer.LivenessScore(ctx, img, detection.Box)
	if err != nil {
		return face, err
	}
	face.LivenessScore = score

	if opts.SkipEmbedding {
		return face, nil
	}

	embedding, err := p.Embedder.Embed(ctx, img, detection.Keypoints)
	if err != nil {
		return face, err
	}
	face.Embedding = embedding

	return face, nil
}
