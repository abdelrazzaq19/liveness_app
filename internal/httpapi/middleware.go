package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/ziad/liveness-verifier/internal/config"
)

// headerRequestID is both read (for trace propagation) and written on every
// response.
const headerRequestID = "X-Request-Id"

// headerAPIKey carries the caller's credential.
const headerAPIKey = "X-API-Key"

// maxClientRequestIDLen bounds an ID supplied by the caller. Without a bound a
// client could push arbitrarily large strings into every log line.
const maxClientRequestIDLen = 64

type contextKey int

const contextKeyRequestID contextKey = iota

// RequestIDFrom returns the identifier assigned to this request, or an empty
// string if the RequestID middleware did not run.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(contextKeyRequestID).(string)
	return id
}

// RequestID attaches an identifier to the request context and echoes it back in
// the response header.
//
// An inbound X-Request-Id is honoured so a caller can correlate across
// services, but it is sanitised first: the value ends up in every log line for
// this request, which makes it an injection vector if taken verbatim.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(headerRequestID))
		if id == "" {
			id = newRequestID()
		}

		w.Header().Set(headerRequestID, id)
		ctx := context.WithValue(r.Context(), contextKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sanitizeRequestID keeps only characters that are safe to embed in a log line,
// and returns "" if nothing usable is left.
func sanitizeRequestID(raw string) string {
	if len(raw) > maxClientRequestIDLen {
		raw = raw[:maxClientRequestIDLen]
	}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		}
	}
	return string(out)
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not recoverable in any useful way here, and a
		// request without an ID is still a request worth serving.
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// statusWriter records the status code and byte count for the access log.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer for flushing
// and deadline control.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// RequestLogger writes one structured line per completed request.
//
// It deliberately records only r.URL.Path and never r.URL.RawQuery or the
// body: frames and identifiers must not reach the log, and this is the one
// middleware that sees every request.
func RequestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}

			next.ServeHTTP(sw, r)

			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}

			level := slog.LevelInfo
			switch {
			case status >= http.StatusInternalServerError:
				level = slog.LevelError
			case status >= http.StatusBadRequest:
				level = slog.LevelWarn
			}

			log.LogAttrs(r.Context(), level, "request completed",
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", sw.bytes),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

// Recoverer turns a panic into a 500 instead of taking the process down.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// ErrAbortHandler is the documented way to abandon a response;
				// swallowing it would break that contract.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				log.LogAttrs(r.Context(), slog.LevelError, "handler panicked",
					slog.String("request_id", RequestIDFrom(r.Context())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)

				respond(w, r, log, Fail(http.StatusInternalServerError, CodeInternal,
					"an unexpected error occurred"))
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds how long a request may run by cancelling its context.
//
// Cancellation rather than a hard response cut-off is deliberate: the context
// reaches the database and the inference pipeline, so the work actually stops
// instead of continuing to burn a CPU core behind an abandoned response.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// APIKeyAuth rejects requests whose X-API-Key header does not match a
// configured key.
//
// Comparison is constant-time and every key is checked even after a match, so
// response timing does not reveal how much of a guessed key was correct.
func APIKeyAuth(keys []config.Secret, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented := []byte(r.Header.Get(headerAPIKey))

			var matched int
			for _, k := range keys {
				matched |= subtle.ConstantTimeCompare(presented, []byte(k.Reveal()))
			}

			if matched != 1 {
				respond(w, r, log, Fail(http.StatusUnauthorized, CodeUnauthorized,
					"a valid "+headerAPIKey+" header is required"))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
