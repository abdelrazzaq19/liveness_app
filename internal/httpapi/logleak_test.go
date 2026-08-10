package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ziad/liveness-verifier/internal/liveness"
)

// The rule this guards is easy to break by accident and impossible to undo: a
// frame or an embedding written to a log lives wherever logs are shipped, for
// as long as they are kept, outside every control that protects the database.
//
// It is checked by driving real requests through the router with the logger at
// debug — the noisiest setting anybody would run — and reading everything that
// came out.
func TestLogsNeverCarryBiometricData(t *testing.T) {
	var buf bytes.Buffer

	// Debug, not info: the point is to catch the line somebody added "just for
	// debugging", which is exactly the line that would not appear at info.
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	fake := &fakeLiveness{session: newFakeSession(t)}
	fake.result = liveness.FrameResult{State: liveness.StateInProgress, Challenge: liveness.ChallengeBlink}

	h, err := NewRouter(Deps{Config: testConfig(), Logger: log, Liveness: fake, Enrollment: &fakeEnrollment{}})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}

	frame := jpegPayload(t)

	// Every path that has an image or a template anywhere near it.
	do(h, jsonRequest(t, http.MethodPost, "/v1/liveness/sessions", nil))
	do(h, frameRequest(t, fake.session.ID.String(), fake.session.Nonce, frame))
	do(h, jsonRequest(t, http.MethodPost, "/v1/faces",
		enrollRequest{Token: "tok", SubjectID: "subject-1", Image: frame}))
	do(h, jsonRequest(t, http.MethodPost, "/v1/faces/search", searchRequest{Image: frame}))

	logs := buf.String()
	if logs == "" {
		t.Fatal("nothing was logged; the guard would pass vacuously")
	}

	// The frame itself, or any recognisable slice of it.
	raw := strings.TrimPrefix(frame, "data:image/jpeg;base64,")
	for _, n := range []int{len(raw), 200, 64} {
		if n > len(raw) {
			continue
		}
		if strings.Contains(logs, raw[:n]) {
			t.Errorf("a %d character run of the submitted frame appears in the logs", n)
		}
	}

	// A JPEG's own header, in case somebody logged the decoded bytes.
	if strings.Contains(logs, "\xff\xd8\xff") {
		t.Error("raw JPEG bytes appear in the logs")
	}

	// A long run of comma-separated floats is what a 512-dimensional embedding
	// looks like once something formats it.
	vectorish := regexp.MustCompile(`(-?\d+\.\d+\s*,\s*){8,}`)
	if m := vectorish.FindString(logs); m != "" {
		t.Errorf("something vector-shaped appears in the logs: %.120s", m)
	}

	// And the field names that would carry one.
	banned := []string{
		`"embedding"`, `"frame"`, `"image"`, `"landmarks"`,
		`"keypoints"`, `"crop"`, `"pixels"`, `"descriptor"`,
	}
	for _, key := range banned {
		if strings.Contains(logs, key) {
			t.Errorf("log field %s would carry biometric data", key)
		}
	}
}

// A credential must not reach the logs either. Unlike a frame it is short, so
// it would be easy to miss in review and easy to grep for afterwards — by
// somebody who should not have it.
func TestLogsNeverCarryCredentials(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	fake := &fakeLiveness{session: newFakeSession(t)}
	h, err := NewRouter(Deps{Config: testConfig(), Logger: log, Liveness: fake, Enrollment: &fakeEnrollment{}})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}

	do(h, jsonRequest(t, http.MethodPost, "/v1/faces",
		enrollRequest{Token: "the-liveness-token", SubjectID: "s", Image: jpegPayload(t)}))

	// A wrong key, to exercise the rejection path where a naive implementation
	// would log what it received.
	bad := jsonRequest(t, http.MethodPost, "/v1/faces/search", searchRequest{Image: jpegPayload(t)})
	bad.Header.Set("X-API-Key", "a-key-that-should-never-be-logged")
	do(h, bad)

	logs := buf.String()
	for _, secret := range []string{
		testAPIKey,
		"a-key-that-should-never-be-logged",
		"the-liveness-token",
		fake.session.Nonce,
	} {
		if secret != "" && strings.Contains(logs, secret) {
			t.Errorf("the credential %q appears in the logs", secret)
		}
	}
}

// frameRequest builds a frame submission, which needs its own shape.
func frameRequest(t *testing.T, id, nonce, frame string) *http.Request {
	t.Helper()

	body := submitFrameRequest{Seq: 1, Nonce: nonce, Frame: frame}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("could not encode the frame: %v", err)
	}

	req := jsonRequest(t, http.MethodPost, "/v1/liveness/sessions/"+id+"/frames", nil)
	req.Body = http.NoBody
	req2 := req.Clone(context.Background())
	req2.Body = io.NopCloser(bytes.NewReader(raw))
	req2.ContentLength = int64(len(raw))
	req2.Header.Set(headerSessionNonce, nonce)
	return req2
}

// A readiness probe reports which check failed and nothing else. It is
// unauthenticated, because a load balancer cannot hold a credential, so
// anything it says is said to everyone.
func TestReadinessSaysWhichCheckFailedAndNoMore(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))

	h, err := NewRouter(Deps{
		Config: testConfig(), Logger: log,
		Ready: map[string]ReadinessCheck{
			"database": func(context.Context) error {
				return errPointingAtInternals{}
			},
			"schema": func(context.Context) error { return nil },
		},
	})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}

	rec := do(h, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d when a check fails", rec.Code, http.StatusServiceUnavailable)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "database") {
		t.Errorf("the probe does not name the failing check: %s", body)
	}
	if strings.Contains(body, "password") || strings.Contains(body, "postgres://") {
		t.Errorf("the probe leaked connection detail: %s", body)
	}
	if strings.Contains(body, "schema") {
		t.Errorf("the probe named a check that passed: %s", body)
	}
}

func TestReadinessIsReadyWhenEverythingPasses(t *testing.T) {
	h, err := NewRouter(Deps{
		Config: testConfig(), Logger: discardLogger(),
		Ready: map[string]ReadinessCheck{
			"database": func(context.Context) error { return nil },
		},
	})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}

	rec := do(h, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// A check that blocks must not hold the probe open. A readiness endpoint that
// hangs takes the instance out of rotation without ever saying why.
func TestReadinessDoesNotHangOnASlowCheck(t *testing.T) {
	h, err := NewRouter(Deps{
		Config: testConfig(), Logger: discardLogger(),
		Ready: map[string]ReadinessCheck{
			"slow": func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
		},
	})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}

	done := make(chan int, 1)
	go func() { done <- do(h, httptest.NewRequest(http.MethodGet, "/readyz", nil)).Code }()

	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", code, http.StatusServiceUnavailable)
		}
	case <-time.After(readinessTimeout + 3*time.Second):
		t.Fatal("the probe never answered; a slow dependency can hang readiness")
	}
}

// errPointingAtInternals carries the sort of detail a driver error would.
type errPointingAtInternals struct{}

func (errPointingAtInternals) Error() string {
	return "dial postgres://liveness:password@postgres:5432: connection refused"
}
