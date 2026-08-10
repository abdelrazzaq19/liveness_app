package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// ReadinessCheck reports whether one dependency can do its job.
//
// It is given a context with a deadline already set. A check that ignores it
// and blocks is worse than a check that fails: a readiness probe that hangs
// takes the whole instance out of rotation without ever saying why.
type ReadinessCheck func(ctx context.Context) error

// readinessTimeout bounds the whole probe.
//
// Short on purpose. A load balancer polling every few seconds gets no value
// from a probe that takes longer than its own interval, and a slow dependency
// should read as not ready rather than as a probe that never answered.
const readinessTimeout = 2 * time.Second

// readyStatus is the probe payload.
//
// Deliberately thin. This endpoint is unauthenticated, because a load balancer
// cannot hold a credential, so it says whether the service can serve and which
// named check failed — never why, never a connection string, never a version.
// "database" is enough for the operator reading the log line beside it; the
// detail lives there, behind the same access control as everything else.
type readyStatus struct {
	Status string `json:"status"`

	// Failing names the checks that did not pass, sorted so the output does not
	// change between identical probes.
	Failing []string `json:"failing,omitempty"`
}

// handleReadyz reports whether the service can serve traffic.
//
// Distinct from /healthz, which answers "is this process alive". A process can
// be perfectly alive with an unreachable database, and answering probes with
// 200 in that state is how a broken instance keeps receiving traffic.
func handleReadyz(log *slog.Logger, checks map[string]ReadinessCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()

		var (
			mu      sync.Mutex
			failing []string
			wg      sync.WaitGroup
		)

		// Concurrently, because the probe's budget is the slowest check rather
		// than the sum of them, and a serial probe grows every time somebody
		// adds a dependency.
		for name, check := range checks {
			wg.Add(1)
			go func(name string, check ReadinessCheck) {
				defer wg.Done()
				if err := check(ctx); err != nil {
					mu.Lock()
					failing = append(failing, name)
					mu.Unlock()

					// The reason goes here, where it is behind the same access
					// control as every other log line, and not into the body.
					log.WarnContext(ctx, "readiness check failed",
						slog.String("check", name),
						slog.String("error", err.Error()))
				}
			}(name, check)
		}
		wg.Wait()

		if len(failing) == 0 {
			writeJSON(w, log, http.StatusOK, readyStatus{Status: "ready"})
			return
		}

		sort.Strings(failing)
		writeJSON(w, log, http.StatusServiceUnavailable, readyStatus{
			Status:  "not ready",
			Failing: failing,
		})
	}
}
