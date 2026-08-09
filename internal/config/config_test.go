package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// requiredEnv is the minimum set of variables that must be present for a
// successful load. Everything else has a default.
func requiredEnv() map[string]string {
	return map[string]string{
		"LV_API_KEYS":            "key-one,key-two",
		"LV_DATABASE_URL":        "postgres://liveness:pw@postgres:5432/liveness?sslmode=disable",
		"LV_OBJSTORE_ENDPOINT":   "minio:9000",
		"LV_OBJSTORE_ACCESS_KEY": "minioadmin",
		"LV_OBJSTORE_SECRET_KEY": "minioadmin-secret",
		"LV_TOKEN_SECRET":        "token-signing-secret",
	}
}

// envFrom turns a map into the lookup function load expects.
func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// withEnv returns the required environment plus the given overrides.
func withEnv(overrides map[string]string) func(string) string {
	m := requiredEnv()
	for k, v := range overrides {
		m[k] = v
	}
	return envFrom(m)
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := load(withEnv(nil))
	if err != nil {
		t.Fatalf("load() returned an unexpected error: %v", err)
	}

	// Spot-check one default per section, including every threshold group, so a
	// dropped or renamed variable shows up here.
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Server.Addr", cfg.Server.Addr, ":8080"},
		{"Server.MaxFrameBytes", cfg.Server.MaxFrameBytes, int64(2 << 20)},
		{"Server.ShutdownTimeout", cfg.Server.ShutdownTimeout, 10 * time.Second},
		{"Log.Level", cfg.Log.Level, slog.LevelInfo},
		{"Log.Format", cfg.Log.Format, "json"},
		{"Database.MaxConns", cfg.Database.MaxConns, 10},
		{"ObjectStore.Bucket", cfg.ObjectStore.Bucket, "liveness-artifacts"},
		{"ObjectStore.UseSSL", cfg.ObjectStore.UseSSL, false},
		{"Models.Mode", cfg.Models.Mode, PipelineStub},
		{"Models.Dir", cfg.Models.Dir, "/models"},
		{"Imaging.MinFaceWidthPx", cfg.Imaging.MinFaceWidthPx, 112},
		{"Imaging.PHashMinDistance", cfg.Imaging.PHashMinDistance, 5},
		{"Liveness.SessionTTL", cfg.Liveness.SessionTTL, 90 * time.Second},
		{"Liveness.ChallengeTimeout", cfg.Liveness.ChallengeTimeout, 5 * time.Second},
		{"Liveness.ChallengeCount", cfg.Liveness.ChallengeCount, 3},
		{"Liveness.EARBlink", cfg.Liveness.EARBlink, 0.21},
		{"Liveness.IdentityCosineMin", cfg.Liveness.IdentityCosineMin, 0.70},
		{"Enrollment.MatchCosineMin", cfg.Enrollment.MatchCosineMin, 0.42},
		{"Enrollment.HNSWM", cfg.Enrollment.HNSWM, 16},
		{"Token.TTL", cfg.Token.TTL, 5 * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// The default pipeline must be the stub: the service and its tests are expected
// to start with an empty models directory.
func TestDefaultPipelineModeIsStub(t *testing.T) {
	cfg, err := load(withEnv(nil))
	if err != nil {
		t.Fatalf("load() returned an unexpected error: %v", err)
	}
	if cfg.Models.Mode != PipelineStub {
		t.Errorf("Models.Mode = %q, want %q", cfg.Models.Mode, PipelineStub)
	}
}

func TestLoadParsesAPIKeyList(t *testing.T) {
	cfg, err := load(withEnv(map[string]string{"LV_API_KEYS": " alpha , beta ,, gamma "}))
	if err != nil {
		t.Fatalf("load() returned an unexpected error: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if len(cfg.Server.APIKeys) != len(want) {
		t.Fatalf("got %d API keys, want %d", len(cfg.Server.APIKeys), len(want))
	}
	for i, w := range want {
		if got := cfg.Server.APIKeys[i].Reveal(); got != w {
			t.Errorf("APIKeys[%d] = %q, want %q", i, got, w)
		}
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	cfg, err := load(withEnv(map[string]string{
		"LV_HTTP_ADDR":                  "127.0.0.1:9999",
		"LV_LOG_LEVEL":                  "debug",
		"LV_PIPELINE_MODE":              "onnx",
		"LV_LIVENESS_SESSION_TTL":       "120s",
		"LV_LIVENESS_EAR_BLINK":         "0.18",
		"LV_ENROLL_MATCH_COSINE_MIN":    "0.5",
		"LV_IMG_MIN_LAPLACIAN_VARIANCE": "120.5",
	}))
	if err != nil {
		t.Fatalf("load() returned an unexpected error: %v", err)
	}

	if cfg.Server.Addr != "127.0.0.1:9999" {
		t.Errorf("Server.Addr = %q, want %q", cfg.Server.Addr, "127.0.0.1:9999")
	}
	if cfg.Log.Level != slog.LevelDebug {
		t.Errorf("Log.Level = %v, want %v", cfg.Log.Level, slog.LevelDebug)
	}
	if cfg.Models.Mode != PipelineONNX {
		t.Errorf("Models.Mode = %q, want %q", cfg.Models.Mode, PipelineONNX)
	}
	if cfg.Liveness.SessionTTL != 120*time.Second {
		t.Errorf("Liveness.SessionTTL = %v, want %v", cfg.Liveness.SessionTTL, 120*time.Second)
	}
	if cfg.Liveness.EARBlink != 0.18 {
		t.Errorf("Liveness.EARBlink = %v, want %v", cfg.Liveness.EARBlink, 0.18)
	}
	if cfg.Enrollment.MatchCosineMin != 0.5 {
		t.Errorf("Enrollment.MatchCosineMin = %v, want %v", cfg.Enrollment.MatchCosineMin, 0.5)
	}
	if cfg.Imaging.MinLaplacianVar != 120.5 {
		t.Errorf("Imaging.MinLaplacianVar = %v, want %v", cfg.Imaging.MinLaplacianVar, 120.5)
	}
}

// A missing required variable must name itself in the error, and all missing
// variables must be reported in a single pass.
func TestLoadReportsAllMissingRequired(t *testing.T) {
	_, err := load(envFrom(map[string]string{}))
	if err == nil {
		t.Fatal("load() succeeded with an empty environment, want an error")
	}

	msg := err.Error()
	for _, key := range []string{
		"LV_API_KEYS",
		"LV_DATABASE_URL",
		"LV_OBJSTORE_ENDPOINT",
		"LV_OBJSTORE_ACCESS_KEY",
		"LV_OBJSTORE_SECRET_KEY",
		"LV_TOKEN_SECRET",
	} {
		if !strings.Contains(msg, key) {
			t.Errorf("error does not mention missing variable %s\nfull error:\n%s", key, msg)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name     string
		override map[string]string
		wantKey  string
	}{
		{"non-numeric int", map[string]string{"LV_MODELS_POOL_SIZE": "many"}, "LV_MODELS_POOL_SIZE"},
		{"int below range", map[string]string{"LV_MODELS_POOL_SIZE": "0"}, "LV_MODELS_POOL_SIZE"},
		{"int above range", map[string]string{"LV_LIVENESS_CHALLENGE_COUNT": "9"}, "LV_LIVENESS_CHALLENGE_COUNT"},
		{"non-numeric float", map[string]string{"LV_LIVENESS_MIN_SCORE": "high"}, "LV_LIVENESS_MIN_SCORE"},
		{"float above range", map[string]string{"LV_LIVENESS_MIN_SCORE": "1.5"}, "LV_LIVENESS_MIN_SCORE"},
		{"bad duration", map[string]string{"LV_LIVENESS_SESSION_TTL": "90 seconds"}, "LV_LIVENESS_SESSION_TTL"},
		{"duration below range", map[string]string{"LV_LIVENESS_SESSION_TTL": "1s"}, "LV_LIVENESS_SESSION_TTL"},
		{"bad bool", map[string]string{"LV_OBJSTORE_USE_SSL": "yes-please"}, "LV_OBJSTORE_USE_SSL"},
		{"bad enum", map[string]string{"LV_PIPELINE_MODE": "tensorflow"}, "LV_PIPELINE_MODE"},
		{"bad log format", map[string]string{"LV_LOG_FORMAT": "xml"}, "LV_LOG_FORMAT"},
		{"bad log level", map[string]string{"LV_LOG_LEVEL": "verbose"}, "LV_LOG_LEVEL"},
		{"empty api key list", map[string]string{"LV_API_KEYS": " , , "}, "LV_API_KEYS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(withEnv(tt.override))
			if err == nil {
				t.Fatalf("load() succeeded, want an error mentioning %s", tt.wantKey)
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("error does not mention %s\nfull error:\n%s", tt.wantKey, err)
			}
		})
	}
}

// Every problem should surface in one run so a misconfigured deployment does
// not cost one restart per mistake.
func TestLoadReportsMultipleErrorsAtOnce(t *testing.T) {
	_, err := load(withEnv(map[string]string{
		"LV_MODELS_POOL_SIZE": "0",
		"LV_PIPELINE_MODE":    "tensorflow",
		"LV_LOG_FORMAT":       "xml",
	}))
	if err == nil {
		t.Fatal("load() succeeded, want an error")
	}

	msg := err.Error()
	for _, key := range []string{"LV_MODELS_POOL_SIZE", "LV_PIPELINE_MODE", "LV_LOG_FORMAT"} {
		if !strings.Contains(msg, key) {
			t.Errorf("error does not mention %s\nfull error:\n%s", key, msg)
		}
	}
}

func TestCrossValidation(t *testing.T) {
	tests := []struct {
		name     string
		override map[string]string
		wantHint string
	}{
		{
			name: "challenge budget exceeds session TTL",
			override: map[string]string{
				"LV_LIVENESS_SESSION_TTL":       "30s",
				"LV_LIVENESS_CHALLENGE_COUNT":   "3",
				"LV_LIVENESS_CHALLENGE_TIMEOUT": "20s",
			},
			wantHint: "LV_LIVENESS_SESSION_TTL",
		},
		{
			name: "write timeout not longer than request timeout",
			override: map[string]string{
				"LV_HTTP_WRITE_TIMEOUT":   "10s",
				"LV_HTTP_REQUEST_TIMEOUT": "20s",
			},
			wantHint: "LV_HTTP_WRITE_TIMEOUT",
		},
		{
			name: "blink threshold not below open threshold",
			override: map[string]string{
				"LV_LIVENESS_EAR_BLINK": "0.35",
				"LV_LIVENESS_EAR_OPEN":  "0.30",
			},
			wantHint: "LV_LIVENESS_EAR_BLINK",
		},
		{
			name: "min brightness not below max brightness",
			override: map[string]string{
				"LV_IMG_MIN_BRIGHTNESS": "200",
				"LV_IMG_MAX_BRIGHTNESS": "100",
			},
			wantHint: "LV_IMG_MIN_BRIGHTNESS",
		},
		{
			name: "min conns exceeds max conns",
			override: map[string]string{
				"LV_DATABASE_MIN_CONNS": "20",
				"LV_DATABASE_MAX_CONNS": "5",
			},
			wantHint: "LV_DATABASE_MIN_CONNS",
		},
		{
			name: "ef_search smaller than top-k degrades recall",
			override: map[string]string{
				"LV_ENROLL_HNSW_EF_SEARCH": "3",
				"LV_ENROLL_SEARCH_TOP_K":   "10",
			},
			wantHint: "LV_ENROLL_HNSW_EF_SEARCH",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(withEnv(tt.override))
			if err == nil {
				t.Fatalf("load() succeeded, want a cross-validation error mentioning %s", tt.wantHint)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error does not mention %s\nfull error:\n%s", tt.wantHint, err)
			}
		})
	}
}

// A valid configuration must not trip any cross-field check.
func TestCrossValidationAcceptsDefaults(t *testing.T) {
	if _, err := load(withEnv(nil)); err != nil {
		t.Fatalf("default configuration failed cross-validation: %v", err)
	}
}

// Secrets must never render their value, whatever the format verb.
func TestSecretIsRedactedInEveryRendering(t *testing.T) {
	const value = "super-secret-value"
	s := Secret(value)

	renderings := map[string]string{
		"%v":       fmt.Sprintf("%v", s),
		"Sprint":   fmt.Sprint(s),
		"%#v":      fmt.Sprintf("%#v", s),
		"String()": s.String(),
		"LogValue": s.LogValue().String(),
	}
	for verb, out := range renderings {
		if strings.Contains(out, value) {
			t.Errorf("%s leaked the secret: %s", verb, out)
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("%s = %q, want it to contain REDACTED", verb, out)
		}
	}

	if s.Reveal() != value {
		t.Errorf("Reveal() = %q, want %q", s.Reveal(), value)
	}
}

// Struct-level formatting must not leak nested secrets either.
func TestConfigFormattingDoesNotLeakSecrets(t *testing.T) {
	cfg, err := load(withEnv(nil))
	if err != nil {
		t.Fatalf("load() returned an unexpected error: %v", err)
	}

	dump := fmt.Sprintf("%+v", *cfg)
	for _, leaked := range []string{"minioadmin-secret", "token-signing-secret", "key-one"} {
		if strings.Contains(dump, leaked) {
			t.Errorf("formatted config leaked %q:\n%s", leaked, dump)
		}
	}
}
