// Package onnx adapts the ONNX Runtime C library to the ports declared in
// internal/biometric.
//
// It is the only package in this repository permitted to import
// onnxruntime_go. Everything upstream of it works against the interfaces in
// internal/biometric, so swapping the inference engine — or substituting the
// deterministic stub — never reaches beyond this directory.
package onnx

import (
	"errors"
	"fmt"
	"os"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// The ONNX Runtime environment is global to the process: the C library allows
// exactly one, and creating a second is an error rather than a second
// environment. Reference counting lets several Runtimes exist in one process —
// which tests need — while still initialising and destroying it exactly once.
var (
	envMu   sync.Mutex
	envRefs int
)

// Runtime owns the ONNX Runtime environment and every session pool created
// through it.
//
// Construct one at start-up, load every model into it, and Close it on
// shutdown. There is no lazy loading: see LoadModel.
type Runtime struct {
	mu     sync.Mutex
	pools  []*Pool
	closed bool
}

// ModelSpec describes one model to load.
type ModelSpec struct {
	// Name identifies the model in errors and logs, e.g. "detector".
	Name string

	// Path is the .onnx file to load.
	Path string

	// PoolSize is how many copies to load. Each copy costs the model's memory
	// again, so this bounds concurrency rather than chasing throughput.
	PoolSize int

	// IntraOpThreads caps the threads ONNX Runtime uses inside a single
	// operator. Zero lets it decide.
	//
	// With a pool of N sessions the defaults oversubscribe badly: every session
	// sizes its thread pool for the whole machine, so N concurrent inferences
	// spawn N times the cores and spend the difference on context switching.
	IntraOpThreads int
}

func (s ModelSpec) validate() error {
	var problems []error
	if s.Name == "" {
		problems = append(problems, errors.New("name is required"))
	}
	if s.Path == "" {
		problems = append(problems, errors.New("path is required"))
	}
	if s.PoolSize < 1 {
		problems = append(problems, fmt.Errorf("pool size must be at least 1, got %d", s.PoolSize))
	}
	if s.IntraOpThreads < 0 {
		problems = append(problems, fmt.Errorf("intra-op threads cannot be negative, got %d", s.IntraOpThreads))
	}
	return errors.Join(problems...)
}

// NewRuntime initialises the ONNX Runtime environment.
//
// libraryPath points at libonnxruntime.so. The binding loads it with dlopen
// rather than linking against it, so the path has to be supplied even when the
// dynamic linker already knows about the library.
func NewRuntime(libraryPath string) (*Runtime, error) {
	if libraryPath == "" {
		return nil, errors.New("onnx: shared library path is required")
	}

	envMu.Lock()
	defer envMu.Unlock()

	if envRefs == 0 {
		ort.SetSharedLibraryPath(libraryPath)
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("onnx: initialise environment using %s: %w", libraryPath, err)
		}
	}
	envRefs++

	return &Runtime{}, nil
}

// Version reports the ONNX Runtime version that was actually loaded, which is
// worth logging: a mismatch with the version the models were exported against
// produces confusing failures much later.
func Version() string { return ort.GetVersion() }

// LoadModel reads a model and returns a pool of sessions for it.
//
// Everything that can fail about a model fails here, at start-up: a missing
// file, an unreadable graph, a session that will not construct. Deferring any
// of it to the first request would turn a deployment mistake into a
// user-visible error at the worst possible moment.
func (r *Runtime) LoadModel(spec ModelSpec) (*Pool, error) {
	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("onnx: model spec: %w", err)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("onnx: runtime is closed")
	}
	r.mu.Unlock()

	if _, err := os.Stat(spec.Path); err != nil {
		return nil, fmt.Errorf("onnx: model %q: %w", spec.Name, err)
	}

	inputs, outputs, err := ort.GetInputOutputInfo(spec.Path)
	if err != nil {
		return nil, fmt.Errorf("onnx: model %q: read graph signature from %s: %w", spec.Name, spec.Path, err)
	}
	if len(inputs) == 0 || len(outputs) == 0 {
		return nil, fmt.Errorf("onnx: model %q: graph declares %d inputs and %d outputs, need at least one of each",
			spec.Name, len(inputs), len(outputs))
	}

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("onnx: model %q: create session options: %w", spec.Name, err)
	}
	// ONNX Runtime copies the options into each session, so releasing them once
	// every session exists is safe.
	defer func() { _ = opts.Destroy() }()

	if spec.IntraOpThreads > 0 {
		if err := opts.SetIntraOpNumThreads(spec.IntraOpThreads); err != nil {
			return nil, fmt.Errorf("onnx: model %q: set intra-op threads: %w", spec.Name, err)
		}
	}

	inNames := infoNames(inputs)
	outNames := infoNames(outputs)

	sessions := make([]*Session, 0, spec.PoolSize)
	for i := 0; i < spec.PoolSize; i++ {
		inner, err := ort.NewDynamicAdvancedSession(spec.Path, inNames, outNames, opts)
		if err != nil {
			// Do not leave half a pool loaded: the memory would stay held for
			// the life of the process with nothing able to reach it.
			destroyAll(sessions)
			return nil, fmt.Errorf("onnx: model %q: create session %d of %d: %w",
				spec.Name, i+1, spec.PoolSize, err)
		}
		sessions = append(sessions, &Session{run: inner, Inputs: inputs, Outputs: outputs})
	}

	pool := newPool(spec.Name, sessions)

	r.mu.Lock()
	closed := r.closed
	if !closed {
		r.pools = append(r.pools, pool)
	}
	r.mu.Unlock()

	if closed {
		// Close ran while we were loading. Release what we just built rather
		// than handing back a pool nobody is going to close.
		_ = pool.Close()
		return nil, errors.New("onnx: runtime is closed")
	}

	return pool, nil
}

// Close shuts down every pool and, once the last Runtime is gone, the
// environment.
//
// Each pool waits for its in-flight inferences first, so Close can block for as
// long as the slowest one takes.
func (r *Runtime) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	pools := r.pools
	r.pools = nil
	r.mu.Unlock()

	var errs []error
	for _, p := range pools {
		if err := p.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	envMu.Lock()
	defer envMu.Unlock()

	envRefs--
	if envRefs == 0 {
		if err := ort.DestroyEnvironment(); err != nil {
			errs = append(errs, fmt.Errorf("onnx: destroy environment: %w", err))
		}
	}

	return errors.Join(errs...)
}

func infoNames(info []ort.InputOutputInfo) []string {
	names := make([]string, len(info))
	for i, n := range info {
		names[i] = n.Name
	}
	return names
}

func destroyAll(sessions []*Session) {
	for _, s := range sessions {
		_ = s.run.Destroy()
	}
}
