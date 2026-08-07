//go:build models

package onnx

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files from the current model output")

// detectorModel is the detector the service actually runs.
//
// SCRFD-500M rather than SCRFD-10G: the heavier model needs 127 ms per frame
// even at 320x320, which leaves nothing of the 150 ms budget for the three
// models that follow it in the pipeline.
const detectorModel = "det_500m.onnx"

// syntheticScene draws a deterministic image with face-like structure.
//
// It is drawn rather than committed as a fixture for two reasons: no photograph
// of a real person belongs in this repository, and a generated scene cannot
// drift or go missing. What matters for a golden file is that the input is
// byte-identical on every run, not that it is a convincing face.
func syntheticScene(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	// Vertical gradient background.
	for y := 0; y < h; y++ {
		shade := uint8(40 + (160 * y / h))
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: shade, G: shade / 2, B: 200 - shade/2, A: 255})
		}
	}

	fill := func(cx, cy, rx, ry int, c color.RGBA) {
		for y := cy - ry; y <= cy+ry; y++ {
			for x := cx - rx; x <= cx+rx; x++ {
				if x < 0 || y < 0 || x >= w || y >= h {
					continue
				}
				dx := float64(x-cx) / float64(rx)
				dy := float64(y-cy) / float64(ry)
				if dx*dx+dy*dy <= 1 {
					img.SetRGBA(x, y, c)
				}
			}
		}
	}

	cx, cy := w/2, h/2
	head := color.RGBA{R: 226, G: 190, B: 160, A: 255}
	dark := color.RGBA{R: 45, G: 35, B: 30, A: 255}

	fill(cx, cy, w/5, h/4, head)                                              // head
	fill(cx-w/13, cy-h/16, w/40, h/50, dark)                                  // left eye
	fill(cx+w/13, cy-h/16, w/40, h/50, dark)                                  // right eye
	fill(cx, cy+h/12, w/22, h/60, dark)                                       // mouth
	fill(cx, cy+h/40, w/50, h/40, color.RGBA{R: 200, G: 165, B: 140, A: 255}) // nose

	return img
}

// strideFingerprint records what one feature map produced.
type strideFingerprint struct {
	Stride   int     `json:"stride"`
	Anchors  int     `json:"anchors"`
	MaxScore float64 `json:"max_score"`
}

// scrfdGolden is the recorded behaviour of the detector on a fixed input.
//
// The synthetic scene is not a real face, so the interesting content is the
// shape of the graph and the exact numbers it emits — both of which change the
// moment the model file is swapped, which is what this guards against.
type scrfdGolden struct {
	Model      string              `json:"model"`
	InputSize  int                 `json:"input_size"`
	SceneW     int                 `json:"scene_width"`
	SceneH     int                 `json:"scene_height"`
	Strides    []strideFingerprint `json:"strides"`
	Detections []detectionRecord   `json:"detections_above_threshold"`
	Threshold  float64             `json:"threshold"`
}

type detectionRecord struct {
	Score float64             `json:"score"`
	Box   biometric.BBox      `json:"box"`
	Keys  biometric.Keypoints `json:"keypoints"`
}

const goldenPath = "testdata/golden/scrfd_synthetic.json"

func loadGolden(t *testing.T) scrfdGolden {
	t.Helper()

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden file: %v\nrun with -update to create it", err)
	}
	var g scrfdGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("parse golden file: %v", err)
	}
	return g
}

func writeGolden(t *testing.T, g scrfdGolden) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
		t.Fatalf("create golden directory: %v", err)
	}
	raw, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatalf("encode golden: %v", err)
	}
	if err := os.WriteFile(goldenPath, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
	t.Logf("wrote %s", goldenPath)
}

// newDetector loads the detector, skipping when the model is absent.
func newDetector(t *testing.T, inputSize int, minScore float64) *SCRFD {
	t.Helper()

	rt, path := newRealRuntime(t, detectorModel)

	pool, err := rt.LoadModel(ModelSpec{
		Name: "detector", Path: path, PoolSize: 1, IntraOpThreads: 0,
	})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	d, err := NewSCRFD(pool, SCRFDOptions{InputSize: inputSize, MinScore: minScore, NMSIoU: 0.4})
	if err != nil {
		t.Fatalf("NewSCRFD() returned an unexpected error: %v", err)
	}
	return d
}

// TestSCRFDGolden pins the detector's behaviour on a fixed input.
//
// Regenerate deliberately, never to make a red test go green:
//
//	docker compose run --rm dev go test -tags=models ./internal/biometric/onnx/ -run TestSCRFDGolden -update
func TestSCRFDGolden(t *testing.T) {
	const (
		inputSize = 640
		sceneW    = 480
		sceneH    = 640
		threshold = 0.3
	)

	d := newDetector(t, inputSize, threshold)
	scene := syntheticScene(sceneW, sceneH)

	got := scrfdGolden{
		Model:     detectorModel,
		InputSize: inputSize,
		SceneW:    sceneW,
		SceneH:    sceneH,
		Threshold: threshold,
	}

	// Read the raw feature maps directly. Per-stride maxima fingerprint the
	// model far more tightly than a detection list that may well be empty for
	// a synthetic scene.
	planes, _ := letterbox(scene, inputSize)
	err := d.pool.Use(context.Background(), func(s *Session) error {
		in, err := ort.NewTensor(ort.NewShape(1, 3, int64(inputSize), int64(inputSize)), planes)
		if err != nil {
			return err
		}
		defer func() { _ = in.Destroy() }()

		outs := make([]ort.Value, len(s.Outputs))
		if err := s.Run([]ort.Value{in}, outs); err != nil {
			return err
		}
		defer destroyValues(outs)

		for i, stride := range scrfdStrides {
			scores, err := floatData(outs[i], "scores", stride)
			if err != nil {
				return err
			}
			maxScore := 0.0
			for _, v := range scores {
				if float64(v) > maxScore {
					maxScore = float64(v)
				}
			}
			got.Strides = append(got.Strides, strideFingerprint{
				Stride:   stride,
				Anchors:  len(scores),
				MaxScore: math.Round(maxScore*1e6) / 1e6,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("raw inference failed: %v", err)
	}

	found, err := d.DetectAll(context.Background(), scene)
	if err != nil {
		t.Fatalf("DetectAll() returned an unexpected error: %v", err)
	}
	for _, f := range found {
		got.Detections = append(got.Detections, detectionRecord{
			Score: math.Round(f.Score*1e6) / 1e6,
			Box:   f.Box,
			Keys:  f.Keypoints,
		})
	}

	for _, s := range got.Strides {
		t.Logf("stride %d: %d anchors, max score %.6f", s.Stride, s.Anchors, s.MaxScore)
	}
	t.Logf("%d detections above %.2f", len(got.Detections), threshold)

	if *updateGolden {
		writeGolden(t, got)
		return
	}

	want := loadGolden(t)
	compareGolden(t, got, want)
}

func compareGolden(t *testing.T, got, want scrfdGolden) {
	t.Helper()

	if got.InputSize != want.InputSize || got.SceneW != want.SceneW || got.SceneH != want.SceneH {
		t.Fatalf("the golden file was recorded for a different setup: got %dx%d at input %d, golden %dx%d at input %d",
			got.SceneW, got.SceneH, got.InputSize, want.SceneW, want.SceneH, want.InputSize)
	}

	if len(got.Strides) != len(want.Strides) {
		t.Fatalf("model produced %d feature maps, golden has %d", len(got.Strides), len(want.Strides))
	}
	for i, w := range want.Strides {
		g := got.Strides[i]
		if g.Stride != w.Stride {
			t.Errorf("feature map %d: stride %d, golden %d", i, g.Stride, w.Stride)
		}
		// Anchor counts are structural: a change means a different graph.
		if g.Anchors != w.Anchors {
			t.Errorf("stride %d: %d anchors, golden %d", w.Stride, g.Anchors, w.Anchors)
		}
		if math.Abs(g.MaxScore-w.MaxScore) > 1e-4 {
			t.Errorf("stride %d: max score %.6f, golden %.6f", w.Stride, g.MaxScore, w.MaxScore)
		}
	}

	if len(got.Detections) != len(want.Detections) {
		t.Fatalf("detector found %d faces, golden has %d", len(got.Detections), len(want.Detections))
	}
	for i, w := range want.Detections {
		g := got.Detections[i]
		if math.Abs(g.Score-w.Score) > 1e-4 {
			t.Errorf("detection %d: score %.6f, golden %.6f", i, g.Score, w.Score)
		}
		// Two pixels of slack absorbs floating-point drift across CPUs without
		// hiding a genuine change in the decode.
		for _, c := range []struct {
			name      string
			got, want float64
		}{
			{"min x", g.Box.MinX, w.Box.MinX},
			{"min y", g.Box.MinY, w.Box.MinY},
			{"max x", g.Box.MaxX, w.Box.MaxX},
			{"max y", g.Box.MaxY, w.Box.MaxY},
		} {
			if math.Abs(c.got-c.want) > 2 {
				t.Errorf("detection %d %s = %.2f, golden %.2f", i, c.name, c.got, c.want)
			}
		}
	}
}

// Detect must report ErrNoFaceFound rather than an empty detection, so a caller
// cannot mistake a zero box for a real one.
func TestDetectReportsNoFaceOnEmptyScene(t *testing.T) {
	d := newDetector(t, 640, 0.6)

	flat := image.NewRGBA(image.Rect(0, 0, 320, 320))
	for y := 0; y < 320; y++ {
		for x := 0; x < 320; x++ {
			flat.SetRGBA(x, y, color.RGBA{R: 18, G: 18, B: 18, A: 255})
		}
	}

	_, err := d.Detect(context.Background(), flat)
	if err == nil {
		t.Fatal("Detect() found a face in a blank image, want ErrNoFaceFound")
	}
	if !errors.Is(err, biometric.ErrNoFaceFound) {
		t.Errorf("Detect() error = %v, want ErrNoFaceFound", err)
	}
}

func TestDetectRejectsUnusableImages(t *testing.T) {
	d := newDetector(t, 640, 0.6)

	t.Run("nil image", func(t *testing.T) {
		if _, err := d.Detect(context.Background(), nil); err == nil {
			t.Error("Detect() accepted a nil image, want an error")
		}
	})

	t.Run("empty image", func(t *testing.T) {
		if _, err := d.Detect(context.Background(), image.NewRGBA(image.Rect(0, 0, 0, 0))); err == nil {
			t.Error("Detect() accepted a zero-sized image, want an error")
		}
	})
}

// BenchmarkDetector measures the cost of a detection across models and input
// sizes. It is what the choice of detector is argued from.
//
//	docker compose run --rm dev go test -tags=models ./internal/biometric/onnx/ \
//	  -run '^$' -bench BenchmarkDetector -benchtime 10x
func BenchmarkDetector(b *testing.B) {
	scene := syntheticScene(480, 640)

	for _, model := range []string{"det_500m.onnx", "det_10g.onnx"} {
		for _, size := range []int{640, 480, 320} {
			b.Run(fmt.Sprintf("%s/size=%d", model, size), func(b *testing.B) {
				path := filepath.Join(modelsDir(), model)
				if _, err := os.Stat(path); err != nil {
					b.Skipf("model %s is not present", path)
				}

				rt, err := NewRuntime(libraryPath())
				if err != nil {
					b.Fatalf("NewRuntime(): %v", err)
				}
				defer func() { _ = rt.Close() }()

				pool, err := rt.LoadModel(ModelSpec{
					Name: "detector", Path: path, PoolSize: 1,
				})
				if err != nil {
					b.Fatalf("LoadModel(): %v", err)
				}
				d, err := NewSCRFD(pool, SCRFDOptions{InputSize: size, MinScore: 0.6, NMSIoU: 0.4})
				if err != nil {
					b.Fatalf("NewSCRFD(): %v", err)
				}

				// One warm-up outside the timer: the first inference pays for
				// lazy allocation inside ONNX Runtime.
				_, _ = d.DetectAll(context.Background(), scene)

				b.ResetTimer()
				start := time.Now()
				for i := 0; i < b.N; i++ {
					if _, err := d.DetectAll(context.Background(), scene); err != nil {
						b.Fatalf("DetectAll(): %v", err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(time.Since(start).Milliseconds())/float64(b.N), "ms/frame")
			})
		}
	}
}
