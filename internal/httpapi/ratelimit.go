package httpapi

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimitSweepInterval is how often exhausted buckets are discarded.
//
// Without a sweep the map grows one entry per client address seen, for the life
// of the process. That is not a leak anyone notices in testing and is one that
// eventually matters.
const rateLimitSweepInterval = 5 * time.Minute

// rateLimitFallbackPerMinute is used when the configured budget is unusable.
//
// Generous enough for one honest session at six frames a second, tight enough
// that an unconfigured deployment is not wide open.
const rateLimitFallbackPerMinute = 600

// bucket is one client's allowance.
type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// limiter hands out requests at a fixed average rate, per client.
//
// A token bucket rather than a fixed window: a fixed window lets a client spend
// its whole minute's allowance in the first instant and then again immediately
// after the boundary, which is twice the rate it was meant to permit.
type limiter struct {
	ratePerSecond float64
	burst         float64

	mu       sync.Mutex
	buckets  map[string]*bucket
	lastSwep time.Time
}

func newLimiter(perMinute int) *limiter {
	rate := float64(perMinute) / 60

	return &limiter{
		ratePerSecond: rate,
		// A minute's worth of burst. Frames arrive at six a second and the
		// budget is set per minute, so anything tighter would reject an
		// ordinary session for being ordinary.
		burst:    float64(perMinute),
		buckets:  make(map[string]*bucket),
		lastSwep: time.Now(),
	}
}

// allow reports whether the client may proceed, and spends a token if so.
func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = b
	}

	b.tokens += now.Sub(b.lastSeen).Seconds() * l.ratePerSecond
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have been idle long enough to have refilled anyway.
func (l *limiter) sweep(now time.Time) {
	if now.Sub(l.lastSwep) < rateLimitSweepInterval {
		return
	}
	l.lastSwep = now

	idle := time.Duration(l.burst/l.ratePerSecond) * time.Second
	if idle < time.Minute {
		idle = time.Minute
	}

	for key, b := range l.buckets {
		if now.Sub(b.lastSeen) > idle {
			delete(l.buckets, key)
		}
	}
}

// RateLimit caps how fast one client may call.
//
// It matters most where no API key is required: an endpoint that creates a
// database row and a slot of inference work, reachable by anyone who can reach
// the port, has nothing else in front of it.
func RateLimit(perMinute int, log *slog.Logger) func(http.Handler) http.Handler {
	// A non-positive budget has two bad readings — reject everything, or let
	// everything through — and both are silent. Configuration already refuses
	// it, so reaching here means something constructed the middleware directly;
	// fall back to something conservative and say so loudly.
	if perMinute <= 0 {
		log.Error("rate limit was configured as non-positive; falling back to a conservative default",
			slog.Int("configured", perMinute),
			slog.Int("using", rateLimitFallbackPerMinute),
		)
		perMinute = rateLimitFallbackPerMinute
	}

	l := newLimiter(perMinute)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l.allow(clientKey(r), time.Now()) {
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Retry-After", "60")
			respond(w, r, log, Fail(http.StatusTooManyRequests, CodeRateLimited,
				"too many requests; slow down"))
		})
	}
}

// clientKey identifies the caller for rate-limiting purposes.
//
// The remote address only. Forwarded headers are not consulted: they are
// trivially forged, and trusting them turns the limiter into a way to exhaust
// somebody else's allowance. Behind a proxy this needs the proxy to be
// configured to set the real address, not a header this code invents trust in.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
