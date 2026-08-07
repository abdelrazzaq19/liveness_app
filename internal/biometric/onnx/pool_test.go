package onnx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

// fakeRunner stands in for a loaded ONNX session.
//
// The pool's job is to make concurrent access correct, and that logic is worth
// testing on every run — not only on the machines that happen to have 187 MB of
// model files on disk.
type fakeRunner struct {
	runs       atomic.Int64
	destroys   atomic.Int64
	destroyErr error

	// inUse counts callers inside Run. A session handed to two goroutines at
	// once is exactly the bug the pool exists to prevent, so the fake detects
	// it rather than trusting the pool.
	inUse      atomic.Int32
	sawOverlap atomic.Bool
}

func (f *fakeRunner) Run(_, _ []ort.Value) error {
	if f.inUse.Add(1) > 1 {
		f.sawOverlap.Store(true)
	}
	f.runs.Add(1)
	f.inUse.Add(-1)
	return nil
}

func (f *fakeRunner) Destroy() error {
	f.destroys.Add(1)
	return f.destroyErr
}

// newFakePool builds a pool of n fake sessions and returns both.
func newFakePool(t *testing.T, n int) (*Pool, []*fakeRunner) {
	t.Helper()

	fakes := make([]*fakeRunner, n)
	sessions := make([]*Session, n)
	for i := range fakes {
		fakes[i] = &fakeRunner{}
		sessions[i] = &Session{run: fakes[i]}
	}
	return newPool("fake", sessions), fakes
}

func TestAcquireAndReleaseRoundTrip(t *testing.T) {
	p, _ := newFakePool(t, 2)

	if got := p.Available(); got != 2 {
		t.Fatalf("Available() = %d on a fresh pool, want 2", got)
	}

	s, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() returned an unexpected error: %v", err)
	}
	if got := p.Available(); got != 1 {
		t.Errorf("Available() = %d after one checkout, want 1", got)
	}

	p.Release(s)
	if got := p.Available(); got != 2 {
		t.Errorf("Available() = %d after release, want 2", got)
	}
}

// An exhausted pool must make callers wait, not hand out a session twice.
func TestAcquireWaitsUntilASessionIsReleased(t *testing.T) {
	p, _ := newFakePool(t, 1)

	held, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() returned an unexpected error: %v", err)
	}

	got := make(chan *Session, 1)
	go func() {
		s, err := p.Acquire(context.Background())
		if err != nil {
			t.Errorf("second Acquire() returned an unexpected error: %v", err)
			close(got)
			return
		}
		got <- s
	}()

	select {
	case <-got:
		t.Fatal("Acquire() succeeded while the only session was checked out")
	case <-time.After(50 * time.Millisecond):
	}

	p.Release(held)

	select {
	case s := <-got:
		if s != held {
			t.Error("the waiting caller got a different session than the one released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire() did not resume after the session was released")
	}
}

// A caller that has given up should stop queueing for a session it will never
// use.
func TestAcquireRespectsContextCancellation(t *testing.T) {
	p, _ := newFakePool(t, 1)

	held, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() returned an unexpected error: %v", err)
	}
	defer p.Release(held)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := p.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Acquire() error = %v, want %v", err, context.DeadlineExceeded)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Acquire() took %s to give up, want it to honour the deadline", elapsed)
	}
}

// select picks randomly among ready cases, so a pool with sessions to spare
// would otherwise sometimes serve an already-cancelled caller.
func TestAcquireFailsFastOnCancelledContext(t *testing.T) {
	p, _ := newFakePool(t, 4)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 50; i++ {
		if _, err := p.Acquire(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire() error = %v on attempt %d, want %v", err, i, context.Canceled)
		}
	}
	if got := p.Available(); got != 4 {
		t.Errorf("Available() = %d after refusing every acquire, want 4", got)
	}
}

// A session that is never returned is a permanent loss of capacity, so Use has
// to survive a panicking caller.
func TestUseReturnsTheSessionAfterAPanic(t *testing.T) {
	p, _ := newFakePool(t, 1)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not propagate to the caller")
			}
		}()
		_ = p.Use(context.Background(), func(*Session) error {
			panic("inference blew up")
		})
	}()

	if got := p.Available(); got != 1 {
		t.Fatalf("Available() = %d after a panicking Use, want 1", got)
	}

	// The pool must still be usable.
	if err := p.Use(context.Background(), func(s *Session) error {
		return s.Run(nil, nil)
	}); err != nil {
		t.Errorf("Use() after a panic returned an unexpected error: %v", err)
	}
}

func TestUseReturnsTheSessionAndTheError(t *testing.T) {
	p, _ := newFakePool(t, 1)
	sentinel := errors.New("inference failed")

	if err := p.Use(context.Background(), func(*Session) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("Use() error = %v, want %v", err, sentinel)
	}
	if got := p.Available(); got != 1 {
		t.Errorf("Available() = %d after a failing Use, want 1", got)
	}
}

func TestCloseDestroysEverySession(t *testing.T) {
	p, fakes := newFakePool(t, 3)

	if err := p.Close(); err != nil {
		t.Fatalf("Close() returned an unexpected error: %v", err)
	}
	for i, f := range fakes {
		if got := f.destroys.Load(); got != 1 {
			t.Errorf("session %d was destroyed %d times, want 1", i, got)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	p, fakes := newFakePool(t, 2)

	for i := 0; i < 3; i++ {
		if err := p.Close(); err != nil {
			t.Fatalf("Close() call %d returned an unexpected error: %v", i+1, err)
		}
	}
	for i, f := range fakes {
		if got := f.destroys.Load(); got != 1 {
			t.Errorf("session %d was destroyed %d times across three Close calls, want 1", i, got)
		}
	}
}

func TestCloseReportsDestroyFailures(t *testing.T) {
	p, fakes := newFakePool(t, 2)
	fakes[1].destroyErr = errors.New("the C library objected")

	err := p.Close()
	if err == nil {
		t.Fatal("Close() succeeded despite a failing Destroy, want an error")
	}
	// Every session must still be destroyed, not just the ones before the
	// failure.
	for i, f := range fakes {
		if got := f.destroys.Load(); got != 1 {
			t.Errorf("session %d was destroyed %d times, want 1", i, got)
		}
	}
}

func TestAcquireAfterCloseFails(t *testing.T) {
	p, _ := newFakePool(t, 1)
	if err := p.Close(); err != nil {
		t.Fatalf("Close() returned an unexpected error: %v", err)
	}

	if _, err := p.Acquire(context.Background()); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("Acquire() error = %v, want %v", err, ErrPoolClosed)
	}
	if err := p.Use(context.Background(), func(*Session) error { return nil }); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("Use() error = %v, want %v", err, ErrPoolClosed)
	}
}

// Close must not free a session another goroutine is still running, because in
// C that corrupts the process instead of raising an error.
func TestCloseWaitsForInFlightWork(t *testing.T) {
	p, fakes := newFakePool(t, 1)

	release := make(chan struct{})
	entered := make(chan struct{})

	go func() {
		_ = p.Use(context.Background(), func(*Session) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()

	select {
	case <-closed:
		t.Fatal("Close() returned while an inference was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	if got := fakes[0].destroys.Load(); got != 0 {
		t.Fatalf("the session was destroyed %d times while still in use, want 0", got)
	}

	close(release)

	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close() returned an unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not return after the inference finished")
	}
	if got := fakes[0].destroys.Load(); got != 1 {
		t.Errorf("session destroyed %d times, want 1", got)
	}
}

// The load the pool exists to survive: far more callers than sessions, all
// running at once. Run with -race; the real failure this guards against is
// memory corruption inside C, and overlap detection is the closest a Go test
// can get to catching it.
func TestConcurrentUseIsSafe(t *testing.T) {
	const (
		goroutines = 100
		perRoutine = 50
		poolSize   = 4
	)

	p, fakes := newFakePool(t, poolSize)
	t.Cleanup(func() { _ = p.Close() })

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perRoutine; j++ {
				err := p.Use(context.Background(), func(s *Session) error {
					return s.Run(nil, nil)
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
		t.Fatalf("Use() returned an unexpected error under load: %v", err)
	}

	var total int64
	for i, f := range fakes {
		if f.sawOverlap.Load() {
			t.Errorf("session %d was run by two goroutines at once", i)
		}
		total += f.runs.Load()
	}
	if want := int64(goroutines * perRoutine); total != want {
		t.Errorf("sessions ran %d inferences in total, want %d", total, want)
	}
	if got := p.Available(); got != poolSize {
		t.Errorf("Available() = %d after the load test, want %d", got, poolSize)
	}
}

func TestModelSpecValidation(t *testing.T) {
	tests := []struct {
		name     string
		spec     ModelSpec
		wantHint string
	}{
		{"missing name", ModelSpec{Path: "m.onnx", PoolSize: 1}, "name"},
		{"missing path", ModelSpec{Name: "m", PoolSize: 1}, "path"},
		{"zero pool size", ModelSpec{Name: "m", Path: "m.onnx"}, "pool size"},
		{"negative threads", ModelSpec{Name: "m", Path: "m.onnx", PoolSize: 1, IntraOpThreads: -1}, "threads"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.validate()
			if err == nil {
				t.Fatalf("validate() succeeded, want an error mentioning %q", tt.wantHint)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error does not mention %q: %v", tt.wantHint, err)
			}
		})
	}

	valid := ModelSpec{Name: "m", Path: "m.onnx", PoolSize: 2, IntraOpThreads: 0}
	if err := valid.validate(); err != nil {
		t.Errorf("validate() rejected a valid spec: %v", err)
	}
}
