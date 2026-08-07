//go:build models

// These tests load real .onnx files and therefore need both the ONNX Runtime
// shared library and the models on disk. They are behind a build tag so the
// default `go test ./...` stays runnable on a checkout with an empty models
// directory.
//
//	docker compose --profile setup run --rm modelctl download
//	docker compose run --rm dev go test -tags=models ./internal/biometric/onnx/... -race
package onnx

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

func libraryPath() string {
	if p := os.Getenv("LV_ONNXRUNTIME_LIB"); p != "" {
		return p
	}
	return "/usr/local/lib/libonnxruntime.so"
}

func modelsDir() string {
	if d := os.Getenv("LV_MODELS_DIR"); d != "" {
		return d
	}
	return "/src/models"
}

// newRealRuntime builds a Runtime, skipping the test when the model is absent
// so that -tags=models on a machine without models reports a skip rather than a
// misleading failure.
func newRealRuntime(t *testing.T, model string) (*Runtime, string) {
	t.Helper()

	path := filepath.Join(modelsDir(), model)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("model %s is not present: %v", path, err)
	}

	rt, err := NewRuntime(libraryPath())
	if err != nil {
		t.Fatalf("NewRuntime() returned an unexpected error: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Errorf("Close() returned an unexpected error: %v", err)
		}
	})
	return rt, path
}

// tensorShape turns a graph's declared input shape into something concrete.
// Dynamic dimensions come back as -1; batch becomes 1 and spatial dimensions
// take a value SCRFD accepts.
func tensorShape(declared ort.Shape) ort.Shape {
	out := make(ort.Shape, len(declared))
	for i, d := range declared {
		switch {
		case d > 0:
			out[i] = d
		case i == 0:
			out[i] = 1
		case i == 1:
			out[i] = 3
		default:
			out[i] = 640
		}
	}
	return out
}

func TestVersionReportsLoadedLibrary(t *testing.T) {
	rt, _ := newRealRuntime(t, "det_10g.onnx")
	_ = rt

	if v := Version(); v == "" {
		t.Error("Version() is empty; the shared library did not report itself")
	} else {
		t.Logf("ONNX Runtime %s", v)
	}
}

func TestLoadModelReadsGraphSignature(t *testing.T) {
	rt, path := newRealRuntime(t, "det_10g.onnx")

	pool, err := rt.LoadModel(ModelSpec{
		Name: "detector", Path: path, PoolSize: 2, IntraOpThreads: 1,
	})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	if got := pool.Size(); got != 2 {
		t.Errorf("Size() = %d, want 2", got)
	}
	if got := pool.Available(); got != 2 {
		t.Errorf("Available() = %d, want 2", got)
	}

	s, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() returned an unexpected error: %v", err)
	}
	defer pool.Release(s)

	if len(s.Inputs) == 0 {
		t.Fatal("the graph reports no inputs")
	}
	if len(s.Outputs) == 0 {
		t.Fatal("the graph reports no outputs")
	}
	t.Logf("input %q shape %v", s.Inputs[0].Name, s.Inputs[0].Dimensions)
	for _, o := range s.Outputs {
		t.Logf("output %q shape %v", o.Name, o.Dimensions)
	}
}

// A missing or unreadable model must fail at load time, not on the first
// request.
func TestLoadModelFailsEarly(t *testing.T) {
	rt, _ := newRealRuntime(t, "det_10g.onnx")

	t.Run("missing file", func(t *testing.T) {
		_, err := rt.LoadModel(ModelSpec{
			Name: "absent", Path: filepath.Join(modelsDir(), "no_such_model.onnx"), PoolSize: 1,
		})
		if err == nil {
			t.Fatal("LoadModel() succeeded for a missing file, want an error")
		}
	})

	t.Run("not an onnx graph", func(t *testing.T) {
		junk := filepath.Join(t.TempDir(), "junk.onnx")
		if err := os.WriteFile(junk, []byte("this is not a protobuf"), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		if _, err := rt.LoadModel(ModelSpec{Name: "junk", Path: junk, PoolSize: 1}); err == nil {
			t.Fatal("LoadModel() succeeded for a file that is not a model, want an error")
		}
	})
}

func TestRunRealInference(t *testing.T) {
	rt, path := newRealRuntime(t, "det_10g.onnx")

	pool, err := rt.LoadModel(ModelSpec{Name: "detector", Path: path, PoolSize: 1, IntraOpThreads: 2})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	start := time.Now()
	err = pool.Use(context.Background(), func(s *Session) error {
		shape := tensorShape(s.Inputs[0].Dimensions)

		in, err := ort.NewEmptyTensor[float32](shape)
		if err != nil {
			return err
		}
		defer func() { _ = in.Destroy() }()

		// A nil entry tells ONNX Runtime to allocate that output itself, which
		// is what dynamic output shapes require.
		outs := make([]ort.Value, len(s.Outputs))
		if err := s.Run([]ort.Value{in}, outs); err != nil {
			return err
		}

		for _, o := range outs {
			if o == nil {
				t.Error("an output was left unallocated")
				continue
			}
			t.Logf("produced output shape %v", o.GetShape())
			_ = o.Destroy()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inference failed: %v", err)
	}
	t.Logf("single inference took %s", time.Since(start))
}

// The real thing under concurrency. The counts are far lower than the
// fake-backed test in pool_test.go: this one is about proving that concurrent
// Run calls on distinct sessions are safe, not about exercising the pool.
func TestConcurrentRealInference(t *testing.T) {
	const (
		goroutines = 8
		perRoutine = 3
	)

	rt, path := newRealRuntime(t, "det_10g.onnx")

	pool, err := rt.LoadModel(ModelSpec{Name: "detector", Path: path, PoolSize: 2, IntraOpThreads: 2})
	if err != nil {
		t.Fatalf("LoadModel() returned an unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perRoutine; j++ {
				err := pool.Use(context.Background(), func(s *Session) error {
					in, err := ort.NewEmptyTensor[float32](tensorShape(s.Inputs[0].Dimensions))
					if err != nil {
						return err
					}
					defer func() { _ = in.Destroy() }()

					outs := make([]ort.Value, len(s.Outputs))
					if err := s.Run([]ort.Value{in}, outs); err != nil {
						return err
					}
					for _, o := range outs {
						if o != nil {
							_ = o.Destroy()
						}
					}
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent inference failed: %v", err)
	}

	total := goroutines * perRoutine
	t.Logf("%d inferences across %d goroutines on a pool of 2 took %s (%s each)",
		total, goroutines, time.Since(start), time.Since(start)/time.Duration(total))
}
