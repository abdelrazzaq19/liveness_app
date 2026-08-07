package onnx

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	ort "github.com/yalue/onnxruntime_go"
)

// ErrPoolClosed is returned by Acquire once the pool has been shut down.
var ErrPoolClosed = errors.New("onnx: session pool is closed")

// runner is what a pooled session must be able to do.
//
// The pool is written against this interface rather than against
// *ort.DynamicAdvancedSession so its concurrency behaviour can be tested with a
// fake, without a model file on disk. Pool correctness is the whole point of
// this type, so it needs to be testable in the default test run.
type runner interface {
	Run(inputs, outputs []ort.Value) error
	Destroy() error
}

// Session is one loaded copy of a model.
//
// A session is checked out of a Pool for the duration of a single inference and
// must not be shared: see Pool for why.
type Session struct {
	run runner

	// Inputs and Outputs describe the graph signature, read once at load time.
	// Callers need the names and shapes to build tensors.
	Inputs  []ort.InputOutputInfo
	Outputs []ort.InputOutputInfo
}

// Run performs one inference.
//
// outputs must have one entry per graph output; individual entries may be nil,
// in which case ONNX Runtime allocates them.
func (s *Session) Run(inputs, outputs []ort.Value) error {
	return s.run.Run(inputs, outputs)
}

// Pool hands out the loaded copies of a single model.
//
// This is not an optimisation. An ONNX Runtime session is not safe for
// concurrent Run calls, and the failure mode is not a Go panic — it is two
// requests writing over each other's tensors inside C, silently producing
// wrong scores. The pool is what makes concurrent use correct at all.
type Pool struct {
	name string

	// free carries the sessions that are available. Its capacity equals the
	// number of sessions, so Release can never block.
	free chan *Session

	// all is fixed after construction and is only read during Close.
	all []*Session

	closed atomic.Bool
}

func newPool(name string, sessions []*Session) *Pool {
	p := &Pool{
		name: name,
		free: make(chan *Session, len(sessions)),
		all:  sessions,
	}
	for _, s := range sessions {
		p.free <- s
	}
	return p
}

// Name returns the model name this pool serves.
func (p *Pool) Name() string { return p.name }

// Size returns how many sessions the pool holds.
func (p *Pool) Size() int { return len(p.all) }

// Available returns how many sessions are currently checked in. It exists for
// tests and diagnostics; do not branch on it, since it is stale the moment it
// returns.
func (p *Pool) Available() int { return len(p.free) }

// Acquire checks out a session, waiting until one is free.
//
// It returns ctx.Err() if the caller gives up first, so a cancelled request
// stops queueing for a session it will never use.
func (p *Pool) Acquire(ctx context.Context) (*Session, error) {
	if p.closed.Load() {
		return nil, fmt.Errorf("acquire %s session: %w", p.name, ErrPoolClosed)
	}
	// An already-cancelled context must not win a session by chance: select
	// picks randomly among ready cases.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire %s session: %w", p.name, err)
	}

	select {
	case s, ok := <-p.free:
		if !ok {
			return nil, fmt.Errorf("acquire %s session: %w", p.name, ErrPoolClosed)
		}
		// Close may have started while we were waiting. Hand the session back
		// so Close can account for it, and report the shutdown.
		if p.closed.Load() {
			p.free <- s
			return nil, fmt.Errorf("acquire %s session: %w", p.name, ErrPoolClosed)
		}
		return s, nil

	case <-ctx.Done():
		return nil, fmt.Errorf("acquire %s session: %w", p.name, ctx.Err())
	}
}

// Release returns a session to the pool. It never blocks.
func (p *Pool) Release(s *Session) {
	if s == nil {
		return
	}
	p.free <- s
}

// Use acquires a session, passes it to fn, and returns it afterwards — even if
// fn panics.
//
// Prefer this over Acquire and Release. A session that is never released is a
// permanent reduction in capacity, and with a small pool that means the service
// wedges rather than degrades.
func (p *Pool) Use(ctx context.Context, fn func(*Session) error) error {
	s, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer p.Release(s)

	return fn(s)
}

// Close waits for every session to come back, then destroys them.
//
// The wait is not politeness. Destroying a session while another goroutine is
// still inside Run frees memory that C code is using, which corrupts the
// process rather than raising an error.
func (p *Pool) Close() error {
	if p.closed.Swap(true) {
		return nil
	}

	for range p.all {
		<-p.free
	}
	close(p.free)

	var errs []error
	for _, s := range p.all {
		if err := s.run.Destroy(); err != nil {
			errs = append(errs, fmt.Errorf("destroy %s session: %w", p.name, err))
		}
	}
	return errors.Join(errs...)
}
