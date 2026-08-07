package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/biometric/onnx"
	"github.com/ziad/liveness-verifier/internal/imaging"
)

// stageTimings records how long each part of a single frame took.
//
// The total is only actionable if you know where it goes, and the stages differ
// by an order of magnitude: measuring the whole alone would leave every tuning
// decision a guess.
type stageTimings struct {
	quality   time.Duration
	detect    time.Duration
	landmarks time.Duration
	antiSpoof time.Duration
	embed     time.Duration
	total     time.Duration
}

// fullPipeline holds every stage, wired.
type fullPipeline struct {
	gate        imaging.Gate
	detector    *onnx.SCRFD
	landmarker  *onnx.Landmarker2d106
	antiSpoofer *onnx.AntiSpoofMiniFASNet
	embedder    *onnx.EmbedderArcFace
}

// runFullPipeline wires all four models and reports a per-stage breakdown.
//
// This is the measurement the A4 latency criterion is actually about: the
// detector alone was never the whole cost.
func runFullPipeline(o options, frames []frame, out *os.File) error {
	rt, err := onnx.NewRuntime(o.library)
	if err != nil {
		return err
	}
	defer func() { _ = rt.Close() }()

	p, err := buildFullPipeline(rt, o)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "pipeline     %s (input %d) + 2d106det + minifasnet_v2 + w600k_r50\n", o.model, o.inputSize)
	fmt.Fprintf(out, "runtime      ONNX Runtime %s on %d cores, threads %d, pool %d\n",
		onnx.Version(), runtime.NumCPU(), o.threads, o.poolSize)
	fmt.Fprintf(out, "frames       %d x %d passes\n\n", len(frames), o.repeat)

	// Warm up outside the measurement: the first inference of every model pays
	// for lazy allocation inside ONNX Runtime.
	for i := 0; i < o.warmup; i++ {
		_, _ = p.run(frames[i%len(frames)].img)
	}

	var timings []stageTimings
	var substituted int

	for pass := 0; pass < o.repeat; pass++ {
		for _, f := range frames {
			t, err := p.run(f.img)
			if err != nil {
				if !errors.Is(err, errBoxSubstituted) {
					return fmt.Errorf("frame %s: %w", f.name, err)
				}
				substituted++
			}
			timings = append(timings, t)
		}
	}

	reportStages(out, timings, substituted, len(frames)*o.repeat)
	return nil
}

func buildFullPipeline(rt *onnx.Runtime, o options) (*fullPipeline, error) {
	load := func(name, file string) (*onnx.Pool, error) {
		path := filepath.Join(o.modelsDir, file)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("model %s: %w\nrun: docker compose --profile setup run --rm modelctl download", path, err)
		}
		return rt.LoadModel(onnx.ModelSpec{
			Name: name, Path: path, PoolSize: o.poolSize, IntraOpThreads: o.threads,
		})
	}

	detectorPool, err := load("detector", o.model)
	if err != nil {
		return nil, err
	}
	landmarkPool, err := load("landmarker", "2d106det.onnx")
	if err != nil {
		return nil, err
	}
	spoofPool, err := load("antispoof", "minifasnet_v2.onnx")
	if err != nil {
		return nil, err
	}
	embedPool, err := load("embedder", "w600k_r50.onnx")
	if err != nil {
		return nil, err
	}

	detector, err := onnx.NewSCRFD(detectorPool, onnx.SCRFDOptions{
		InputSize: o.inputSize, MinScore: o.minScore, NMSIoU: o.nmsIoU,
	})
	if err != nil {
		return nil, err
	}
	landmarker, err := onnx.NewLandmarker2d106(landmarkPool)
	if err != nil {
		return nil, err
	}
	antiSpoofer, err := onnx.NewAntiSpoofMiniFASNet(spoofPool)
	if err != nil {
		return nil, err
	}
	embedder, err := onnx.NewEmbedderArcFace(embedPool)
	if err != nil {
		return nil, err
	}

	return &fullPipeline{
		// Thresholds wide open: the gate is being timed here, not enforced.
		gate:        imaging.Gate{MaxBrightness: 255},
		detector:    detector,
		landmarker:  landmarker,
		antiSpoofer: antiSpoofer,
		embedder:    embedder,
	}, nil
}

// errBoxSubstituted reports that no face was detected and a stand-in box was
// used so the later stages could still be timed.
var errBoxSubstituted = errors.New("no face detected; measured the later stages against a substitute box")

// run times one frame through every stage.
//
// When detection finds nothing — which is what the synthetic scenes do — a
// stand-in box and keypoints are substituted so the remaining stages are still
// measured. That is legitimate for a cost measurement and meaningless for an
// accuracy one: the models do the same amount of arithmetic either way, and
// none of it says whether the answer is right.
func (p *fullPipeline) run(img image.Image) (stageTimings, error) {
	var t stageTimings
	ctx := context.Background()
	start := time.Now()

	qStart := time.Now()
	_, _ = p.gate.QualityCheck(img)
	t.quality = time.Since(qStart)

	dStart := time.Now()
	det, detErr := p.detector.Detect(ctx, img)
	t.detect = time.Since(dStart)

	substituted := false
	if detErr != nil {
		if !errors.Is(detErr, biometric.ErrNoFaceFound) {
			t.total = time.Since(start)
			return t, detErr
		}
		det = substituteDetection(img.Bounds())
		substituted = true
	}

	lStart := time.Now()
	_, err := p.landmarker.Landmarks(ctx, img, det.Box)
	t.landmarks = time.Since(lStart)
	if err != nil {
		t.total = time.Since(start)
		return t, err
	}

	sStart := time.Now()
	_, err = p.antiSpoofer.LivenessScore(ctx, img, det.Box)
	t.antiSpoof = time.Since(sStart)
	if err != nil {
		t.total = time.Since(start)
		return t, err
	}

	eStart := time.Now()
	_, err = p.embedder.Embed(ctx, img, det.Keypoints)
	t.embed = time.Since(eStart)
	t.total = time.Since(start)

	if err != nil {
		return t, err
	}
	if substituted {
		return t, errBoxSubstituted
	}
	return t, nil
}

// substituteDetection is a face-shaped box in the middle of the frame, with
// keypoints at the fractions a frontal face has them.
func substituteDetection(bounds image.Rectangle) biometric.Detection {
	w := float64(bounds.Dx())
	h := float64(bounds.Dy())

	side := w
	if h < side {
		side = h
	}
	side *= 0.5

	cx := w / 2
	cy := h / 2

	box := biometric.BBox{
		MinX: cx - side/2, MinY: cy - side/2,
		MaxX: cx + side/2, MaxY: cy + side/2,
	}

	var k biometric.Keypoints
	k[biometric.KeypointLeftEye] = biometric.Point{X: box.MinX + side*0.34, Y: box.MinY + side*0.40}
	k[biometric.KeypointRightEye] = biometric.Point{X: box.MinX + side*0.66, Y: box.MinY + side*0.40}
	k[biometric.KeypointNose] = biometric.Point{X: box.MinX + side*0.50, Y: box.MinY + side*0.58}
	k[biometric.KeypointMouthLeft] = biometric.Point{X: box.MinX + side*0.38, Y: box.MinY + side*0.75}
	k[biometric.KeypointMouthRight] = biometric.Point{X: box.MinX + side*0.62, Y: box.MinY + side*0.75}

	return biometric.Detection{Box: box, Keypoints: k, Score: 0}
}

func reportStages(out *os.File, timings []stageTimings, substituted, total int) {
	if len(timings) == 0 {
		fmt.Fprintln(out, "no frames measured")
		return
	}

	stages := []struct {
		name string
		get  func(stageTimings) time.Duration
	}{
		{"quality gate", func(t stageTimings) time.Duration { return t.quality }},
		{"detector", func(t stageTimings) time.Duration { return t.detect }},
		{"landmarker", func(t stageTimings) time.Duration { return t.landmarks }},
		{"anti-spoof", func(t stageTimings) time.Duration { return t.antiSpoof }},
		{"embedder", func(t stageTimings) time.Duration { return t.embed }},
		{"TOTAL", func(t stageTimings) time.Duration { return t.total }},

		// The cost of an ordinary frame. Identity consistency is checked on key
		// frames, not on every one, so this is what the camera actually has to
		// keep up with — and percentiles do not add, so it has to be computed
		// per frame rather than subtracted from the two rows above.
		{"per frame*", func(t stageTimings) time.Duration { return t.total - t.embed }},
	}

	fmt.Fprintf(out, "%-14s %11s %11s %11s\n", "stage", "p50", "p95", "p99")
	for _, s := range stages {
		durations := make([]time.Duration, 0, len(timings))
		for _, t := range timings {
			if d := s.get(t); d > 0 {
				durations = append(durations, d)
			}
		}
		if len(durations) == 0 {
			fmt.Fprintf(out, "%-14s %11s %11s %11s\n", s.name, "-", "-", "-")
			continue
		}

		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		fmt.Fprintf(out, "%-14s %9.1f ms %9.1f ms %9.1f ms\n", s.name,
			ms(percentile(durations, 50)), ms(percentile(durations, 95)), ms(percentile(durations, 99)))
	}

	fmt.Fprintln(out, "\n* per frame excludes the embedder, which only runs on key frames")

	fmt.Fprintf(out, "\nframes       %d measured\n", total)
	if substituted > 0 {
		fmt.Fprintf(out, "note         %d frames had no detectable face; the later stages were timed\n"+
			"             against a substitute box. Costs are real, accuracy says nothing.\n", substituted)
	}
}
