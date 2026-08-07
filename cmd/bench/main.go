// Command bench measures the face detector end to end: decode a frame, run
// detection, report where the time went.
//
// It exists to answer one question with numbers rather than guesses — whether
// the pipeline fits the per-frame latency budget — and to give any change to
// the model, its input size, or its thread count something to be compared
// against.
//
//	bench -images testdata/faces
//	bench -synthetic 20 -size 320
//	bench -model det_10g.onnx -concurrency 4
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/biometric/onnx"
	"github.com/ziad/liveness-verifier/internal/imaging"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "bench: %v\n", err)
		os.Exit(1)
	}
}

// options are the knobs the benchmark exposes.
//
// The defaults mirror internal/config so that a run measures what the service
// would actually do. Changing one here does not change the service.
type options struct {
	imageDir    string
	synthetic   int
	modelsDir   string
	model       string
	library     string
	inputSize   int
	minScore    float64
	nmsIoU      float64
	threads     int
	poolSize    int
	concurrency int
	repeat      int
	warmup      int
	verbose     bool
	full        bool
}

func run(args []string, out, errOut *os.File) error {
	var o options

	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(errOut)

	fs.StringVar(&o.imageDir, "images", "", "directory of .jpg/.jpeg/.png frames to measure")
	fs.IntVar(&o.synthetic, "synthetic", 0, "measure this many generated scenes instead of files")
	fs.StringVar(&o.modelsDir, "models", envOr("LV_MODELS_DIR", "models"), "directory holding the .onnx files")
	fs.StringVar(&o.model, "model", "det_500m.onnx", "detector model file name")
	fs.StringVar(&o.library, "library", envOr("LV_ONNXRUNTIME_LIB", "/usr/local/lib/libonnxruntime.so"), "path to libonnxruntime.so")
	fs.IntVar(&o.inputSize, "size", 640, "detector input size; must be a multiple of 32")
	fs.Float64Var(&o.minScore, "min-score", 0.60, "detection score threshold")
	fs.Float64Var(&o.nmsIoU, "nms-iou", 0.40, "NMS overlap threshold")
	fs.IntVar(&o.threads, "threads", 0, "intra-op threads; 0 lets ONNX Runtime decide")
	fs.IntVar(&o.poolSize, "pool", 0, "session pool size; 0 matches -concurrency")
	fs.IntVar(&o.concurrency, "concurrency", 1, "frames processed at once")
	fs.IntVar(&o.repeat, "repeat", 1, "passes over the input set")
	fs.IntVar(&o.warmup, "warmup", 3, "untimed inferences before measuring")
	fs.BoolVar(&o.verbose, "v", false, "print a line per frame")
	fs.BoolVar(&o.full, "full", false, "measure the whole pipeline stage by stage, not just the detector")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.imageDir == "" && o.synthetic == 0 {
		fs.Usage()
		return errors.New("need either -images or -synthetic")
	}
	if o.concurrency < 1 {
		return fmt.Errorf("-concurrency must be at least 1, got %d", o.concurrency)
	}
	if o.repeat < 1 {
		return fmt.Errorf("-repeat must be at least 1, got %d", o.repeat)
	}
	if o.poolSize == 0 {
		o.poolSize = o.concurrency
	}

	frames, err := loadFrames(o)
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return fmt.Errorf("no usable frames found in %s", o.imageDir)
	}

	if o.full {
		return runFullPipeline(o, frames, out)
	}

	detector, closeRuntime, err := buildDetector(o)
	if err != nil {
		return err
	}
	defer closeRuntime()

	fmt.Fprintf(out, "model        %s (input %d, threads %d, pool %d)\n",
		o.model, o.inputSize, o.threads, o.poolSize)
	fmt.Fprintf(out, "runtime      ONNX Runtime %s on %d cores\n", onnx.Version(), runtime.NumCPU())
	fmt.Fprintf(out, "frames       %d x %d passes, concurrency %d\n\n", len(frames), o.repeat, o.concurrency)

	// The first inferences pay for lazy allocation inside ONNX Runtime, which
	// would otherwise land in the tail percentiles and look like jitter.
	warmUp(detector, frames, o.warmup)

	results := measure(detector, frames, o)
	return report(out, results, o)
}

// frame is one decoded input, held in memory so that decode cost is measured
// separately from disk I/O.
type frame struct {
	name string
	img  image.Image
}

// result is one timed detection.
type result struct {
	frame     string
	elapsed   time.Duration
	detection biometric.Detection
	found     bool
	err       error
}

func loadFrames(o options) ([]frame, error) {
	if o.synthetic > 0 {
		frames := make([]frame, 0, o.synthetic)
		for i := 0; i < o.synthetic; i++ {
			frames = append(frames, frame{
				name: fmt.Sprintf("synthetic-%02d", i),
				img:  syntheticScene(480, 640, i),
			})
		}
		return frames, nil
	}

	entries, err := os.ReadDir(o.imageDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", o.imageDir, err)
	}

	limits := imaging.Limits{MaxBytes: 32 << 20, MaxPixels: 64_000_000}

	var frames []frame
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".jpg", ".jpeg", ".png":
		default:
			continue
		}

		path := filepath.Join(o.imageDir, e.Name())
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}

		img, err := imaging.Decode(f, limits)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}

		frames = append(frames, frame{name: e.Name(), img: img})
	}
	return frames, nil
}

func buildDetector(o options) (*onnx.SCRFD, func(), error) {
	path := filepath.Join(o.modelsDir, o.model)
	if _, err := os.Stat(path); err != nil {
		return nil, nil, fmt.Errorf("model %s: %w\nrun: docker compose --profile setup run --rm modelctl download", path, err)
	}

	rt, err := onnx.NewRuntime(o.library)
	if err != nil {
		return nil, nil, err
	}
	closeRuntime := func() { _ = rt.Close() }

	pool, err := rt.LoadModel(onnx.ModelSpec{
		Name:           "detector",
		Path:           path,
		PoolSize:       o.poolSize,
		IntraOpThreads: o.threads,
	})
	if err != nil {
		closeRuntime()
		return nil, nil, err
	}

	detector, err := onnx.NewSCRFD(pool, onnx.SCRFDOptions{
		InputSize: o.inputSize,
		MinScore:  o.minScore,
		NMSIoU:    o.nmsIoU,
	})
	if err != nil {
		closeRuntime()
		return nil, nil, err
	}
	return detector, closeRuntime, nil
}

func warmUp(d *onnx.SCRFD, frames []frame, rounds int) {
	for i := 0; i < rounds; i++ {
		_, _ = d.Detect(context.Background(), frames[i%len(frames)].img)
	}
}

// measure runs every frame the requested number of times and records how long
// each detection took.
func measure(d *onnx.SCRFD, frames []frame, o options) []result {
	jobs := make(chan frame)
	results := make([]result, 0, len(frames)*o.repeat)

	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < o.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				start := time.Now()
				det, err := d.Detect(context.Background(), f.img)
				elapsed := time.Since(start)

				r := result{frame: f.name, elapsed: elapsed, detection: det}
				switch {
				case err == nil:
					r.found = true
				case errors.Is(err, biometric.ErrNoFaceFound):
					// A frame with no face is a valid outcome, not a failure.
				default:
					r.err = err
				}

				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}
		}()
	}

	for pass := 0; pass < o.repeat; pass++ {
		for _, f := range frames {
			jobs <- f
		}
	}
	close(jobs)
	wg.Wait()

	return results
}

func report(out *os.File, results []result, o options) error {
	var failed, noFace int
	durations := make([]time.Duration, 0, len(results))
	var total time.Duration

	for _, r := range results {
		if r.err != nil {
			failed++
			fmt.Fprintf(out, "ERROR  %-20s %v\n", r.frame, r.err)
			continue
		}
		durations = append(durations, r.elapsed)
		total += r.elapsed

		if !r.found {
			noFace++
		}
		if o.verbose {
			if r.found {
				b := r.detection.Box
				fmt.Fprintf(out, "%-20s %7.1f ms  score %.3f  box [%.0f %.0f %.0f %.0f]  %.0fx%.0f px\n",
					r.frame, ms(r.elapsed), r.detection.Score,
					b.MinX, b.MinY, b.MaxX, b.MaxY, b.Width(), b.Height())
			} else {
				fmt.Fprintf(out, "%-20s %7.1f ms  no face\n", r.frame, ms(r.elapsed))
			}
		}
	}

	if len(durations) == 0 {
		return fmt.Errorf("every frame failed: %d errors", failed)
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	fmt.Fprintf(out, "\nlatency per frame\n")
	for _, p := range []struct {
		label string
		value time.Duration
	}{
		{"min", durations[0]},
		{"p50", percentile(durations, 50)},
		{"p95", percentile(durations, 95)},
		{"p99", percentile(durations, 99)},
		{"max", durations[len(durations)-1]},
		{"mean", total / time.Duration(len(durations))},
	} {
		fmt.Fprintf(out, "  %-5s %8.1f ms\n", p.label, ms(p.value))
	}

	fmt.Fprintf(out, "\nframes       %d measured, %d with a face, %d without, %d failed\n",
		len(durations), len(durations)-noFace, noFace, failed)

	// Throughput is wall-clock over the whole run, so with -concurrency above 1
	// it exceeds what the per-frame latency alone would suggest.
	if total > 0 {
		perWorker := total / time.Duration(o.concurrency)
		fmt.Fprintf(out, "throughput   %.1f frames/s at concurrency %d\n",
			float64(len(durations))/perWorker.Seconds(), o.concurrency)
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d frames failed to process", failed, len(results))
	}
	return nil
}

// percentile returns the nearest-rank percentile of a sorted slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// syntheticScene draws a deterministic face-like scene, varied by seed.
//
// It exists because this repository holds no photographs of people, by policy,
// so there is no corpus to run against on a fresh checkout. Synthetic frames
// measure throughput honestly — the model does the same work either way — but
// they say nothing about detection quality. For that, point -images at a real
// set.
func syntheticScene(w, h, seed int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))

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

	// Shift the face around so repeated frames are not byte-identical, which
	// would let a cache flatter the numbers.
	cx := w/2 + (seed%5-2)*w/40
	cy := h/2 + (seed%3-1)*h/40

	head := color.RGBA{R: 226, G: 190, B: 160, A: 255}
	dark := color.RGBA{R: 45, G: 35, B: 30, A: 255}

	fill(cx, cy, w/5, h/4, head)
	fill(cx-w/13, cy-h/16, w/40, h/50, dark)
	fill(cx+w/13, cy-h/16, w/40, h/50, dark)
	fill(cx, cy+h/12, w/22, h/60, dark)

	return img
}
