// Package httpapi is the transport layer. It translates HTTP requests into
// domain calls and domain results back into HTTP responses, and holds no
// business logic of its own.
//
// Nothing under internal/liveness or internal/enrollment may import this
// package, and this package may not reach past the services it is given.
package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ziad/liveness-verifier/internal/config"
	"github.com/ziad/liveness-verifier/internal/imaging"
)

// Deps carries everything the router needs. Wiring happens in cmd/server; this
// package constructs none of its own dependencies.
type Deps struct {
	Config *config.Config
	Logger *slog.Logger

	// Liveness serves the verification endpoints. A nil value leaves them
	// unmounted, which is what the health-only configuration in the earliest
	// tests relies on.
	Liveness LivenessService
}

func (d Deps) validate() error {
	if d.Config == nil {
		return errors.New("httpapi: Config is required")
	}
	if d.Logger == nil {
		return errors.New("httpapi: Logger is required")
	}
	if len(d.Config.Server.APIKeys) == 0 {
		return errors.New("httpapi: at least one API key is required")
	}
	return nil
}

// NewRouter builds the HTTP handler for the service.
func NewRouter(d Deps) (http.Handler, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	log := d.Logger
	r := chi.NewRouter()

	// Order matters. RequestID runs first so every later line can reference it;
	// Recoverer sits inside the logger so a panic is still logged as a request.
	r.Use(RequestID)
	r.Use(RequestLogger(log))
	r.Use(Recoverer(log))
	r.Use(Timeout(d.Config.Server.RequestTimeout))

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		respond(w, req, log, Fail(http.StatusNotFound, CodeNotFound, "no such endpoint"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		respond(w, req, log, Fail(http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"method not allowed for this endpoint"))
	})

	// Unauthenticated: container and load-balancer probes must not need a key.
	r.Get("/healthz", handleHealthz(log, string(d.Config.Models.Mode)))

	if err := mountDemo(r); err != nil {
		return nil, fmt.Errorf("httpapi: mount demo: %w", err)
	}

	r.Route("/v1", func(r chi.Router) {
		r.Use(APIKeyAuth(d.Config.Server.APIKeys, log))

		if d.Liveness != nil {
			h := &livenessHandler{
				svc: d.Liveness,
				log: log,
				limits: imaging.Limits{
					MaxBytes:  d.Config.Server.MaxFrameBytes,
					MaxPixels: d.Config.Imaging.MaxDecodedPixels,
				},
				// Base64 inflates by a third; the rest is room for the JSON
				// around it.
				maxBodyBytes: d.Config.Server.MaxFrameBytes*4/3 + 4096,
			}
			r.Route("/liveness", h.routes)
		}

		// Face endpoints are mounted here by later tasks.

		// A chi sub-router builds its middleware chain lazily, and only once at
		// least one route is registered on it. Without this catch-all the
		// sub-router would answer straight from its NotFound handler and skip
		// APIKeyAuth entirely, leaving /v1 unauthenticated.
		//
		// It also keeps that guarantee afterwards: any path under /v1 needs a
		// key before it can learn whether the path exists. More specific
		// patterns registered later win over this wildcard.
		r.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			respond(w, req, log, Fail(http.StatusNotFound, CodeNotFound, "no such endpoint"))
		}))
	})

	return r, nil
}

// healthStatus is the liveness probe payload. It answers "is this process
// running", not "can it serve traffic" — that is what /readyz will be for.
type healthStatus struct {
	Status string `json:"status"`

	// Pipeline says whether real models are in use. The demo page shows it,
	// because a session that passed against the stub proves only that the
	// wiring works and it would be easy to forget which one is running.
	Pipeline string `json:"pipeline,omitempty"`
}

func handleHealthz(log *slog.Logger, pipeline string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, log, http.StatusOK, healthStatus{Status: "ok", Pipeline: pipeline})
	}
}
