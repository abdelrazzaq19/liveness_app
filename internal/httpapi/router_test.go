package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziad/liveness-verifier/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Server: config.Server{
			Addr:            ":8080",
			RequestTimeout:  2 * time.Second,
			ShutdownTimeout: time.Second,
			APIKeys:         []config.Secret{"key-one", "key-two"},
		},
		Log: config.Log{Level: slog.LevelDebug, Format: "json"},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// bufferLogger returns a logger writing into buf, for asserting on log output.
func bufferLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	h, err := NewRouter(Deps{Config: testConfig(), Logger: discardLogger()})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}
	return h
}

// do issues req against h and returns the recorder.
func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeError reads the standard error envelope out of a response.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response body is not a valid error envelope: %v\nbody: %s", err, rec.Body.String())
	}
	return env
}

func TestNewRouterRejectsIncompleteDeps(t *testing.T) {
	cfgNoKeys := testConfig()
	cfgNoKeys.Server.APIKeys = nil

	tests := []struct {
		name string
		deps Deps
	}{
		{"missing config", Deps{Logger: discardLogger()}},
		{"missing logger", Deps{Config: testConfig()}},
		{"no api keys", Deps{Config: cfgNoKeys, Logger: discardLogger()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewRouter(tt.deps); err == nil {
				t.Error("NewRouter() succeeded, want an error")
			}
		})
	}
}

// The container health check must work without a credential.
func TestHealthzIsPublic(t *testing.T) {
	rec := do(newTestRouter(t), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var got healthStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, rec.Body.String())
	}
	if got.Status != "ok" {
		t.Errorf("status field = %q, want %q", got.Status, "ok")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestUnknownRouteUsesErrorEnvelope(t *testing.T) {
	rec := do(newTestRouter(t), httptest.NewRequest(http.MethodGet, "/does-not-exist", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := decodeError(t, rec).Error.Code; got != CodeNotFound {
		t.Errorf("error code = %q, want %q", got, CodeNotFound)
	}
}

func TestMethodNotAllowedUsesErrorEnvelope(t *testing.T) {
	rec := do(newTestRouter(t), httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if got := decodeError(t, rec).Error.Code; got != CodeMethodNotAllowed {
		t.Errorf("error code = %q, want %q", got, CodeMethodNotAllowed)
	}
}

// Every /v1 route sits behind the API key. A valid key must get past auth,
// which shows up as a 404 rather than a 401 for an unmounted path.
func TestV1RequiresAPIKey(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		setHeader  bool
		wantStatus int
	}{
		{"no header", "", false, http.StatusUnauthorized},
		{"empty header", "", true, http.StatusUnauthorized},
		{"wrong key", "not-a-real-key", true, http.StatusUnauthorized},
		{"key prefix only", "key-", true, http.StatusUnauthorized},
		{"first configured key", "key-one", true, http.StatusNotFound},
		{"second configured key", "key-two", true, http.StatusNotFound},
	}

	h := newTestRouter(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
			if tt.setHeader {
				req.Header.Set(headerAPIKey, tt.key)
			}

			rec := do(h, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus == http.StatusUnauthorized {
				if got := decodeError(t, rec).Error.Code; got != CodeUnauthorized {
					t.Errorf("error code = %q, want %q", got, CodeUnauthorized)
				}
			}
		})
	}
}

func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	rec := do(newTestRouter(t), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	id := rec.Header().Get(headerRequestID)
	if id == "" {
		t.Fatal("response is missing a request ID header")
	}
	if len(id) < 8 {
		t.Errorf("generated request ID %q is suspiciously short", id)
	}
}

// A caller-supplied ID lands in every log line for the request, so it must not
// be able to carry newlines or spaces into the log.
func TestClientRequestIDIsSanitized(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(headerRequestID, "trace-123 FAKE\nlevel=ERROR msg=owned")

	got := do(newTestRouter(t), req).Header().Get(headerRequestID)

	if strings.ContainsAny(got, " \n\r\t=") {
		t.Errorf("sanitized request ID still contains injection characters: %q", got)
	}
	if !strings.HasPrefix(got, "trace-123") {
		t.Errorf("sanitized request ID = %q, want it to keep the safe prefix", got)
	}
}

func TestClientRequestIDIsTruncated(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(headerRequestID, strings.Repeat("a", 500))

	got := do(newTestRouter(t), req).Header().Get(headerRequestID)

	if len(got) > maxClientRequestIDLen {
		t.Errorf("request ID length = %d, want at most %d", len(got), maxClientRequestIDLen)
	}
}

func TestRecovererConvertsPanicToErrorEnvelope(t *testing.T) {
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	h := RequestID(Recoverer(discardLogger())(panicking))

	rec := do(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := decodeError(t, rec).Error.Code; got != CodeInternal {
		t.Errorf("error code = %q, want %q", got, CodeInternal)
	}
	if body := rec.Body.String(); strings.Contains(body, "boom") {
		t.Errorf("panic value leaked into the response body: %s", body)
	}
}

func TestTimeoutCancelsRequestContext(t *testing.T) {
	var cause error
	observer := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		cause = r.Context().Err()
	})
	h := Timeout(10 * time.Millisecond)(observer)

	do(h, httptest.NewRequest(http.MethodGet, "/", nil))

	if !errors.Is(cause, context.DeadlineExceeded) {
		t.Errorf("context error = %v, want %v", cause, context.DeadlineExceeded)
	}
}

// The access log sees every request, so it is the likeliest place for a secret
// to escape. It must record the path and never the query string.
func TestRequestLoggerDoesNotLogQueryString(t *testing.T) {
	var buf bytes.Buffer
	h := RequestID(RequestLogger(bufferLogger(&buf))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	))

	do(h, httptest.NewRequest(http.MethodGet, "/healthz?api_key=SUPERSECRET&subject=alice", nil))

	logged := buf.String()
	for _, leaked := range []string{"SUPERSECRET", "alice", "api_key"} {
		if strings.Contains(logged, leaked) {
			t.Errorf("access log leaked %q:\n%s", leaked, logged)
		}
	}
	if !strings.Contains(logged, "/healthz") {
		t.Errorf("access log does not record the path:\n%s", logged)
	}
}

// Internal error text names tables, hosts, and file paths. It belongs in the
// log, never in the response.
func TestRespondHidesInternalErrorDetail(t *testing.T) {
	var buf bytes.Buffer
	log := bufferLogger(&buf)

	const secret = "dial tcp 10.0.0.7:5432: connection refused"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, r, log, errors.New(secret))
	})

	rec := do(RequestID(handler), httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if body := rec.Body.String(); strings.Contains(body, secret) {
		t.Errorf("internal error detail leaked to the client: %s", body)
	}
	if !strings.Contains(buf.String(), secret) {
		t.Errorf("internal error detail was not logged:\n%s", buf.String())
	}

	env := decodeError(t, rec)
	if env.Error.Code != CodeInternal {
		t.Errorf("error code = %q, want %q", env.Error.Code, CodeInternal)
	}
	if env.Error.RequestID == "" {
		t.Error("error envelope is missing the request ID")
	}
}

// An *APIError keeps its own status and code all the way to the client.
func TestRespondPreservesAPIErrorStatus(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, r, discardLogger(),
			FailWith(http.StatusGone, CodeGone, "session expired", errors.New("ttl elapsed")))
	})

	rec := do(RequestID(handler), httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusGone {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusGone)
	}
	env := decodeError(t, rec)
	if env.Error.Code != CodeGone {
		t.Errorf("error code = %q, want %q", env.Error.Code, CodeGone)
	}
	if env.Error.Message != "session expired" {
		t.Errorf("message = %q, want %q", env.Error.Message, "session expired")
	}
	if strings.Contains(rec.Body.String(), "ttl elapsed") {
		t.Errorf("wrapped cause leaked to the client: %s", rec.Body.String())
	}
}
