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

	// Enrollment serves the gallery endpoints. Nil leaves them unmounted, which
	// is what a deployment that only verifies and never enrols wants.
	Enrollment EnrollmentService

	// Tokens mints the single-use token a passed session earns. Nil means no
	// token is issued, and enrollment is then unreachable by design.
	Tokens TokenIssuer

	// Ready holds the checks /readyz runs, keyed by the name reported when one
	// fails. An empty map makes the service always ready, which is honest only
	// for a deployment with no dependencies.
	Ready map[string]ReadinessCheck
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
	//
	// Two probes, because they answer different questions. /healthz says the
	// process is alive; /readyz says it can serve. A process is often perfectly
	// alive with an unreachable database, and answering probes with 200 in that
	// state is how a broken instance keeps receiving traffic.
	r.Get("/healthz", handleHealthz(log, string(d.Config.Models.Mode)))
	r.Get("/readyz", handleReadyz(log, d.Ready))

	if err := mountDemo(r); err != nil {
		return nil, fmt.Errorf("httpapi: mount demo: %w", err)
	}

	var h *livenessHandler
	if d.Liveness != nil {
		h = &livenessHandler{
			svc: d.Liveness,
			log: log,
			limits: imaging.Limits{
				MaxBytes:  d.Config.Server.MaxFrameBytes,
				MaxPixels: d.Config.Imaging.MaxDecodedPixels,
			},
			// Base64 inflates by a third; the rest is room for the JSON around
			// it.
			maxBodyBytes: d.Config.Server.MaxFrameBytes*4/3 + 4096,
			tokens:       d.Tokens,
		}
	}

	var f *facesHandler
	if d.Enrollment != nil {
		f = &facesHandler{
			svc: d.Enrollment,
			log: log,
			limits: imaging.Limits{
				MaxBytes:  d.Config.Server.MaxFrameBytes,
				MaxPixels: d.Config.Imaging.MaxDecodedPixels,
			},
			maxBodyBytes: d.Config.Server.MaxFrameBytes*4/3 + 4096,
		}
	}

	r.Route("/v1", func(r chi.Router) {
		// Session-scoped endpoints carry no API key.
		//
		// They are authorised by the session's own nonce, which the service
		// checks in constant time before touching anything. An API key is an
		// operator's credential; the person being verified is a subject, and
		// putting an operator credential in their browser is how it ends up
		// somewhere it should not be.
		//
		// The id and the nonce are 128 random bits each, they expire with the
		// session, and holding both is the capability.
		if h != nil {
			r.Group(func(r chi.Router) {
				r.Use(RateLimit(d.Config.Server.RateLimitPerMin, log))

				r.Post("/liveness/sessions/{id}/frames", h.submitFrame)
				r.Post("/liveness/sessions/{id}/complete", h.complete)
				r.Get("/liveness/sessions/{id}", h.status)
			})
		}

		// Creating a session is the integrator's call: it allocates a database
		// row and a slot of inference work, so it is the one that has to be
		// attributable and rate-limited.
		//
		// This is the only route the anonymous setting reaches. Scoping it to
		// one route rather than to a group is deliberate: a group would quietly
		// open every endpoint anyone added to it later, and the enrollment
		// endpoints are going in next door.
		if h != nil {
			r.Group(func(r chi.Router) {
				r.Use(RateLimit(d.Config.Server.RateLimitPerMin, log))
				if !d.Config.Server.AllowAnonymousSessions {
					r.Use(APIKeyAuth(d.Config.Server.APIKeys, log))
				}
				r.Post("/liveness/sessions", h.start)
			})
		}

		// Everything else is an operator call and always needs the key.
		r.Group(func(r chi.Router) {
			r.Use(APIKeyAuth(d.Config.Server.APIKeys, log))

			// The gallery is operator territory throughout. Even enrolling,
			// which a subject's liveness token authorises, is a call an
			// integrator makes on their behalf: the token proves the capture was
			// live, the key proves who is allowed to write to the gallery, and
			// neither substitutes for the other.
			if f != nil {
				r.Post("/faces", f.enroll)
				r.Post("/faces/search", f.search)

				// No subject in the path. See facesHandler.deleteSubject.
				r.Delete("/faces", f.deleteSubject)
			}

			// A chi sub-router builds its middleware chain lazily, and only
			// once at least one route is registered on it. Without this
			// catch-all an unmounted /v1 would answer straight from its
			// NotFound handler and skip the key check entirely.
			//
			// It also keeps that guarantee afterwards: any path under /v1 that
			// is not explicitly session-scoped needs a key before it can learn
			// whether the path exists.
			r.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				respond(w, req, log, Fail(http.StatusNotFound, CodeNotFound, "no such endpoint"))
			}))
		})
	})

	return r, nil
}

// healthStatus is the liveness probe payload. It answers "is this process
// running", not "can it serve traffic" — that is what /readyz answers.
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
