// Package config loads and validates every runtime tunable from the process
// environment.
//
// This package is the single source of truth for thresholds, timeouts, and
// limits. No other package may hard-code them: the biometric thresholds in
// particular start life as estimates from the literature and are expected to
// move once the calibration harness produces measured values, so they must be
// reachable without a code change.
//
// Load reports every problem it finds at once rather than failing on the first
// one, because a misconfigured deployment otherwise costs one restart per
// mistake.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Secret wraps a sensitive value so it cannot be printed by accident. Its
// String, GoString, and LogValue methods all return a placeholder; call Reveal
// at the point of use to obtain the real value.
type Secret string

// String implements fmt.Stringer and deliberately hides the value.
func (s Secret) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer so %#v stays redacted too.
func (s Secret) GoString() string { return `"[REDACTED]"` }

// LogValue implements slog.LogValuer so secrets stay redacted in structured logs.
func (s Secret) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// Reveal returns the underlying value. Call it only where the secret is used.
func (s Secret) Reveal() string { return string(s) }

// PipelineMode selects which biometric pipeline implementation is wired in.
type PipelineMode string

const (
	// PipelineONNX runs the real ONNX Runtime models. Requires model files on disk.
	PipelineONNX PipelineMode = "onnx"

	// PipelineStub runs a deterministic fake. It is the default so that the
	// service and its tests come up without any model files present.
	PipelineStub PipelineMode = "stub"
)

// Config is the fully validated configuration for one process.
type Config struct {
	Server      Server
	Log         Log
	Database    Database
	ObjectStore ObjectStore
	Models      Models
	Imaging     Imaging
	Liveness    Liveness
	Enrollment  Enrollment
	Token       Token
}

// Server holds HTTP transport settings.
type Server struct {
	Addr            string
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration

	// MaxFrameBytes caps the decoded size of a single uploaded frame. Enforced
	// before decoding, not after.
	MaxFrameBytes int64

	APIKeys         []Secret
	RateLimitPerMin int
}

// Log holds logging settings.
type Log struct {
	Level  slog.Level
	Format string // "json" or "text"
}

// Database holds PostgreSQL connection settings.
type Database struct {
	URL            Secret
	MaxConns       int
	MinConns       int
	ConnectTimeout time.Duration
}

// ObjectStore holds S3-compatible (MinIO) settings.
type ObjectStore struct {
	Endpoint  string
	AccessKey Secret
	SecretKey Secret
	Bucket    string
	Region    string
	UseSSL    bool
}

// Models holds inference engine settings and model file names. File names are
// resolved relative to Dir.
type Models struct {
	Mode           PipelineMode
	Dir            string
	PoolSize       int
	IntraOpThreads int // 0 lets ONNX Runtime decide.

	// SharedLibrary is the path to libonnxruntime.so. The binding loads it at
	// runtime rather than linking against it, so the path has to be given.
	SharedLibrary string

	Detector   string
	Landmarker string
	AntiSpoof  string
	Embedder   string
}

// Imaging holds decode limits and the frame quality gate.
type Imaging struct {
	MaxDecodedPixels int
	MinFaceWidthPx   int
	MinLaplacianVar  float64
	MinBrightness    float64
	MaxBrightness    float64

	// PHashMinDistance is the minimum Hamming distance between two frames'
	// perceptual hashes for them to count as distinct. Frames closer than this
	// are treated as a replayed still image.
	PHashMinDistance int
}

// Liveness holds session lifecycle and challenge-satisfaction thresholds.
//
// Every value here is currently an estimate taken from the literature or set by
// judgement. None has been measured against a labelled dataset yet.
type Liveness struct {
	SessionTTL       time.Duration
	ChallengeTimeout time.Duration
	ChallengeCount   int

	MinDetectionScore float64
	MinScore          float64

	EARBlink       float64 // Eye aspect ratio below this counts as closed.
	EAROpen        float64 // Eye aspect ratio above this counts as open again.
	BlinkMinFrames int

	YawTurnDeg   float64
	PitchNodDeg  float64
	MARMouthOpen float64

	// IdentityCosineMin is the minimum cosine similarity between the first
	// frame's embedding and every later key frame. It blocks a face swap in the
	// middle of an otherwise valid session.
	IdentityCosineMin float64
}

// Enrollment holds gallery matching and pgvector HNSW index settings.
type Enrollment struct {
	MatchCosineMin     float64
	SearchTopK         int
	HNSWM              int
	HNSWEfConstruction int
	HNSWEfSearch       int
}

// Token holds settings for the single-use token that binds a passed liveness
// session to an enrollment request.
type Token struct {
	Secret Secret
	TTL    time.Duration
}

// Load reads and validates configuration from the process environment.
func Load() (*Config, error) { return load(os.Getenv) }

// load is the testable core of Load, parameterised over the environment lookup.
func load(getenv func(string) string) (*Config, error) {
	l := &loader{getenv: getenv}

	cfg := &Config{
		Server: Server{
			Addr:            l.str("LV_HTTP_ADDR", ":8080"),
			ReadTimeout:     l.duration("LV_HTTP_READ_TIMEOUT", 10*time.Second, time.Second, 5*time.Minute),
			WriteTimeout:    l.duration("LV_HTTP_WRITE_TIMEOUT", 30*time.Second, time.Second, 5*time.Minute),
			IdleTimeout:     l.duration("LV_HTTP_IDLE_TIMEOUT", 60*time.Second, time.Second, 30*time.Minute),
			RequestTimeout:  l.duration("LV_HTTP_REQUEST_TIMEOUT", 20*time.Second, time.Second, 5*time.Minute),
			ShutdownTimeout: l.duration("LV_HTTP_SHUTDOWN_TIMEOUT", 10*time.Second, time.Second, 2*time.Minute),
			MaxFrameBytes:   int64(l.intRange("LV_HTTP_MAX_FRAME_BYTES", 2<<20, 64<<10, 32<<20)),
			APIKeys:         l.secretsRequired("LV_API_KEYS"),
			RateLimitPerMin: l.intRange("LV_RATE_LIMIT_PER_MIN", 600, 1, 100_000),
		},
		Log: Log{
			Level:  l.logLevel("LV_LOG_LEVEL", slog.LevelInfo),
			Format: l.oneOf("LV_LOG_FORMAT", "json", "json", "text"),
		},
		Database: Database{
			URL:            Secret(l.strRequired("LV_DATABASE_URL")),
			MaxConns:       l.intRange("LV_DATABASE_MAX_CONNS", 10, 1, 500),
			MinConns:       l.intRange("LV_DATABASE_MIN_CONNS", 2, 0, 500),
			ConnectTimeout: l.duration("LV_DATABASE_CONNECT_TIMEOUT", 5*time.Second, time.Second, time.Minute),
		},
		ObjectStore: ObjectStore{
			Endpoint:  l.strRequired("LV_OBJSTORE_ENDPOINT"),
			AccessKey: Secret(l.strRequired("LV_OBJSTORE_ACCESS_KEY")),
			SecretKey: Secret(l.strRequired("LV_OBJSTORE_SECRET_KEY")),
			Bucket:    l.str("LV_OBJSTORE_BUCKET", "liveness-artifacts"),
			Region:    l.str("LV_OBJSTORE_REGION", "us-east-1"),
			UseSSL:    l.boolean("LV_OBJSTORE_USE_SSL", false),
		},
		Models: Models{
			Mode:           PipelineMode(l.oneOf("LV_PIPELINE_MODE", string(PipelineStub), string(PipelineStub), string(PipelineONNX))),
			Dir:            l.str("LV_MODELS_DIR", "/models"),
			PoolSize:       l.intRange("LV_MODELS_POOL_SIZE", 2, 1, 64),
			IntraOpThreads: l.intRange("LV_MODELS_INTRA_OP_THREADS", 0, 0, 128),
			SharedLibrary:  l.str("LV_ONNXRUNTIME_LIB", "/usr/local/lib/libonnxruntime.so"),
			Detector:       l.str("LV_MODEL_DETECTOR", "scrfd_10g_bnkps.onnx"),
			Landmarker:     l.str("LV_MODEL_LANDMARKER", "2d106det.onnx"),
			AntiSpoof:      l.str("LV_MODEL_ANTISPOOF", "minifasnet_v2.onnx"),
			Embedder:       l.str("LV_MODEL_EMBEDDER", "w600k_r50.onnx"),
		},
		Imaging: Imaging{
			MaxDecodedPixels: l.intRange("LV_IMG_MAX_DECODED_PIXELS", 16_000_000, 100_000, 200_000_000),
			MinFaceWidthPx:   l.intRange("LV_IMG_MIN_FACE_WIDTH_PX", 112, 32, 4096),
			MinLaplacianVar:  l.float("LV_IMG_MIN_LAPLACIAN_VARIANCE", 80, 0, 100_000),
			MinBrightness:    l.float("LV_IMG_MIN_BRIGHTNESS", 40, 0, 255),
			MaxBrightness:    l.float("LV_IMG_MAX_BRIGHTNESS", 215, 0, 255),
			PHashMinDistance: l.intRange("LV_IMG_PHASH_MIN_DISTANCE", 5, 0, 64),
		},
		Liveness: Liveness{
			SessionTTL:        l.duration("LV_LIVENESS_SESSION_TTL", 90*time.Second, 10*time.Second, 10*time.Minute),
			ChallengeTimeout:  l.duration("LV_LIVENESS_CHALLENGE_TIMEOUT", 20*time.Second, 3*time.Second, 5*time.Minute),
			ChallengeCount:    l.intRange("LV_LIVENESS_CHALLENGE_COUNT", 3, 1, 5),
			MinDetectionScore: l.float("LV_LIVENESS_MIN_DETECTION_SCORE", 0.60, 0, 1),
			MinScore:          l.float("LV_LIVENESS_MIN_SCORE", 0.80, 0, 1),
			EARBlink:          l.float("LV_LIVENESS_EAR_BLINK", 0.21, 0, 1),
			EAROpen:           l.float("LV_LIVENESS_EAR_OPEN", 0.30, 0, 1),
			BlinkMinFrames:    l.intRange("LV_LIVENESS_BLINK_MIN_FRAMES", 2, 1, 30),
			YawTurnDeg:        l.float("LV_LIVENESS_YAW_TURN_DEG", 25, 5, 89),
			PitchNodDeg:       l.float("LV_LIVENESS_PITCH_NOD_DEG", 15, 5, 89),
			MARMouthOpen:      l.float("LV_LIVENESS_MAR_MOUTH_OPEN", 0.55, 0, 5),
			IdentityCosineMin: l.float("LV_LIVENESS_IDENTITY_COSINE_MIN", 0.70, 0, 1),
		},
		Enrollment: Enrollment{
			MatchCosineMin:     l.float("LV_ENROLL_MATCH_COSINE_MIN", 0.42, 0, 1),
			SearchTopK:         l.intRange("LV_ENROLL_SEARCH_TOP_K", 5, 1, 1000),
			HNSWM:              l.intRange("LV_ENROLL_HNSW_M", 16, 2, 100),
			HNSWEfConstruction: l.intRange("LV_ENROLL_HNSW_EF_CONSTRUCTION", 64, 4, 1000),
			HNSWEfSearch:       l.intRange("LV_ENROLL_HNSW_EF_SEARCH", 40, 1, 1000),
		},
		Token: Token{
			Secret: Secret(l.strRequired("LV_TOKEN_SECRET")),
			TTL:    l.duration("LV_TOKEN_TTL", 5*time.Minute, 30*time.Second, time.Hour),
		},
	}

	l.crossValidate(cfg)

	if len(l.errs) > 0 {
		return nil, fmt.Errorf("invalid configuration: %w", errors.Join(l.errs...))
	}
	return cfg, nil
}

// crossValidate checks constraints that span more than one variable. These are
// the mistakes that individually-valid values still produce.
func (l *loader) crossValidate(c *Config) {
	// A session that cannot fit its own challenges will always expire, and the
	// failure looks like a model problem rather than a configuration one.
	budget := time.Duration(c.Liveness.ChallengeCount) * c.Liveness.ChallengeTimeout
	if budget > c.Liveness.SessionTTL {
		l.errs = append(l.errs, fmt.Errorf(
			"LV_LIVENESS_SESSION_TTL: %s is shorter than LV_LIVENESS_CHALLENGE_COUNT (%d) x LV_LIVENESS_CHALLENGE_TIMEOUT (%s) = %s",
			c.Liveness.SessionTTL, c.Liveness.ChallengeCount, c.Liveness.ChallengeTimeout, budget))
	}

	// If the connection write deadline fires first, the client sees a dropped
	// connection instead of the timeout response the request timeout intends.
	if c.Server.WriteTimeout <= c.Server.RequestTimeout {
		l.errs = append(l.errs, fmt.Errorf(
			"LV_HTTP_WRITE_TIMEOUT (%s) must be longer than LV_HTTP_REQUEST_TIMEOUT (%s)",
			c.Server.WriteTimeout, c.Server.RequestTimeout))
	}

	// Blink detection needs hysteresis: a single threshold would flap.
	if c.Liveness.EARBlink >= c.Liveness.EAROpen {
		l.errs = append(l.errs, fmt.Errorf(
			"LV_LIVENESS_EAR_BLINK (%.3f) must be lower than LV_LIVENESS_EAR_OPEN (%.3f)",
			c.Liveness.EARBlink, c.Liveness.EAROpen))
	}

	if c.Imaging.MinBrightness >= c.Imaging.MaxBrightness {
		l.errs = append(l.errs, fmt.Errorf(
			"LV_IMG_MIN_BRIGHTNESS (%.1f) must be lower than LV_IMG_MAX_BRIGHTNESS (%.1f)",
			c.Imaging.MinBrightness, c.Imaging.MaxBrightness))
	}

	if c.Database.MinConns > c.Database.MaxConns {
		l.errs = append(l.errs, fmt.Errorf(
			"LV_DATABASE_MIN_CONNS (%d) must not exceed LV_DATABASE_MAX_CONNS (%d)",
			c.Database.MinConns, c.Database.MaxConns))
	}

	// Searching a smaller candidate list than the requested result count
	// silently degrades recall instead of erroring at query time.
	if c.Enrollment.HNSWEfSearch < c.Enrollment.SearchTopK {
		l.errs = append(l.errs, fmt.Errorf(
			"LV_ENROLL_HNSW_EF_SEARCH (%d) must be at least LV_ENROLL_SEARCH_TOP_K (%d)",
			c.Enrollment.HNSWEfSearch, c.Enrollment.SearchTopK))
	}
}

// loader reads environment variables and accumulates every failure so that
// load can report them together.
type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) fail(key, format string, args ...any) {
	l.errs = append(l.errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
}

// raw returns the trimmed value and whether it was set to something non-empty.
func (l *loader) raw(key string) (string, bool) {
	v := strings.TrimSpace(l.getenv(key))
	return v, v != ""
}

func (l *loader) str(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

func (l *loader) strRequired(key string) string {
	v, ok := l.raw(key)
	if !ok {
		l.fail(key, "is required but not set")
		return ""
	}
	return v
}

// secretsRequired parses a comma-separated list of secrets and requires at
// least one entry.
func (l *loader) secretsRequired(key string) []Secret {
	v, ok := l.raw(key)
	if !ok {
		l.fail(key, "is required but not set (comma-separated list)")
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]Secret, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, Secret(p))
		}
	}
	if len(out) == 0 {
		l.fail(key, "contains no non-empty entries")
	}
	return out
}

func (l *loader) intRange(key string, def, minVal, maxVal int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail(key, "must be an integer, got %q", v)
		return def
	}
	if n < minVal || n > maxVal {
		l.fail(key, "must be between %d and %d, got %d", minVal, maxVal, n)
		return def
	}
	return n
}

func (l *loader) float(key string, def, minVal, maxVal float64) float64 {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.fail(key, "must be a number, got %q", v)
		return def
	}
	if f < minVal || f > maxVal {
		l.fail(key, "must be between %g and %g, got %g", minVal, maxVal, f)
		return def
	}
	return f
}

func (l *loader) duration(key string, def, minVal, maxVal time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, "must be a Go duration such as 90s or 5m, got %q", v)
		return def
	}
	if d < minVal || d > maxVal {
		l.fail(key, "must be between %s and %s, got %s", minVal, maxVal, d)
		return def
	}
	return d
}

func (l *loader) boolean(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(key, "must be a boolean such as true or false, got %q", v)
		return def
	}
	return b
}

func (l *loader) oneOf(key, def string, allowed ...string) string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.fail(key, "must be one of %s, got %q", strings.Join(allowed, "|"), v)
	return def
}

func (l *loader) logLevel(key string, def slog.Level) slog.Level {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(v)); err != nil {
		l.fail(key, "must be one of debug|info|warn|error, got %q", v)
		return def
	}
	return lvl
}
