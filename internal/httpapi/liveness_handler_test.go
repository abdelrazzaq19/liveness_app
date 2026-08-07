package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziad/liveness-verifier/internal/liveness"
)

// fakeLiveness is a scripted stand-in for the domain service.
type fakeLiveness struct {
	session *liveness.Session
	result  liveness.FrameResult
	verdict liveness.Verdict

	startErr, submitErr, completeErr, getErr error

	lastInput liveness.FrameInput
	lastID    liveness.SessionID
}

func (f *fakeLiveness) Start(context.Context) (*liveness.Session, error) {
	return f.session, f.startErr
}

func (f *fakeLiveness) SubmitFrame(_ context.Context, id liveness.SessionID, in liveness.FrameInput) (liveness.FrameResult, error) {
	f.lastID = id
	f.lastInput = in
	return f.result, f.submitErr
}

func (f *fakeLiveness) Complete(_ context.Context, id liveness.SessionID) (liveness.Verdict, error) {
	f.lastID = id
	return f.verdict, f.completeErr
}

func (f *fakeLiveness) Get(_ context.Context, id liveness.SessionID) (*liveness.Session, error) {
	f.lastID = id
	return f.session, f.getErr
}

func newFakeSession(t *testing.T) *liveness.Session {
	t.Helper()

	s, err := liveness.NewSession(time.Now().UTC(), liveness.NewSessionParams{
		ID: "session-under-test", ChallengeCount: 3,
		TTL: 90 * time.Second, ChallengeTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSession() returned an unexpected error: %v", err)
	}
	return s
}

// livenessRouter builds a router with the fake service mounted.
func livenessRouter(t *testing.T, fake *fakeLiveness) http.Handler {
	t.Helper()

	cfg := testConfig()
	cfg.Server.MaxFrameBytes = 64 << 10
	cfg.Imaging.MaxDecodedPixels = 4_000_000

	h, err := NewRouter(Deps{Config: cfg, Logger: discardLogger(), Liveness: fake})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}
	return h
}

func authed(method, path string, body any) *http.Request {
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		raw, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set(headerAPIKey, "key-one")
	return r
}

// pngFrame returns a small PNG as a base64 data URL, which is what a browser
// canvas produces.
func pngFrame(t *testing.T, w, h int) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestStartSession(t *testing.T) {
	fake := &fakeLiveness{session: newFakeSession(t)}
	rec := do(livenessRouter(t, fake), authed(http.MethodPost, "/v1/liveness/sessions", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, http.StatusCreated, rec.Body)
	}

	var got startSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got.SessionID == "" || got.Nonce == "" {
		t.Errorf("response is missing the session id or nonce: %+v", got)
	}
	if len(got.Challenges) != 3 {
		t.Errorf("returned %d challenges, want 3", len(got.Challenges))
	}
}

// The client shows the subject how long each step allows before they start, so
// both figures have to be on the response that opens the session.
func TestStartSessionCarriesTheTiming(t *testing.T) {
	fake := &fakeLiveness{session: newFakeSession(t)}
	rec := do(livenessRouter(t, fake), authed(http.MethodPost, "/v1/liveness/sessions", nil))

	var got startSessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}

	if got.ChallengeSeconds <= 0 {
		t.Errorf("challenge_seconds = %g, want the per-step allowance", got.ChallengeSeconds)
	}
	if got.SecondsRemaining <= 0 || got.SecondsRemaining > got.ChallengeSeconds+1 {
		t.Errorf("seconds_remaining = %g, want it within the %g second allowance",
			got.SecondsRemaining, got.ChallengeSeconds)
	}
}

// The countdown is interpolated between responses, so it has to be on every
// one of them.
func TestFrameResponseCarriesTheCountdown(t *testing.T) {
	fake := &fakeLiveness{
		session: newFakeSession(t),
		result: liveness.FrameResult{
			State: liveness.StateInProgress, Challenge: liveness.ChallengeBlink,
			Remaining: 3, SecondsRemaining: 7.5,
		},
	}

	rec := do(livenessRouter(t, fake), authed(http.MethodPost, "/v1/liveness/sessions/abc/frames",
		submitFrameRequest{Seq: 1, Nonce: "n", Frame: pngFrame(t, 16, 16)}))

	var got submitFrameResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got.SecondsRemaining != 7.5 {
		t.Errorf("seconds_remaining = %g, want 7.5", got.SecondsRemaining)
	}
}

func TestSubmitFrameDecodesAndForwards(t *testing.T) {
	fake := &fakeLiveness{
		session: newFakeSession(t),
		result: liveness.FrameResult{
			State: liveness.StateInProgress, Challenge: liveness.ChallengeBlink,
			Remaining: 3, Reason: "blink",
		},
	}

	req := authed(http.MethodPost, "/v1/liveness/sessions/abc/frames", submitFrameRequest{
		Seq: 7, Nonce: "the-nonce", Frame: pngFrame(t, 32, 32),
	})
	rec := do(livenessRouter(t, fake), req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, http.StatusOK, rec.Body)
	}

	if fake.lastID != "abc" {
		t.Errorf("session id = %q, want %q", fake.lastID, "abc")
	}
	if fake.lastInput.Seq != 7 || fake.lastInput.Nonce != "the-nonce" {
		t.Errorf("forwarded seq/nonce = %d/%q, want 7/the-nonce", fake.lastInput.Seq, fake.lastInput.Nonce)
	}
	if fake.lastInput.Image == nil {
		t.Error("the frame was not decoded")
	}
	if fake.lastInput.PHash == 0 {
		t.Error("no perceptual hash was computed")
	}

	var got submitFrameResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got.Challenge != string(liveness.ChallengeBlink) || got.Reason != "blink" {
		t.Errorf("response = %+v, want the challenge and reason forwarded", got)
	}
}

// A browser sends a data URL; a machine client is likelier to send bare base64.
// Both have to work.
func TestSubmitFrameAcceptsBareBase64AndDataURLs(t *testing.T) {
	dataURL := pngFrame(t, 16, 16)
	bare := strings.TrimPrefix(dataURL, "data:image/png;base64,")

	for name, payload := range map[string]string{"data url": dataURL, "bare base64": bare} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeLiveness{session: newFakeSession(t)}
			req := authed(http.MethodPost, "/v1/liveness/sessions/abc/frames", submitFrameRequest{
				Seq: 1, Nonce: "n", Frame: payload,
			})

			if rec := do(livenessRouter(t, fake), req); rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d\nbody: %s", rec.Code, http.StatusOK, rec.Body)
			}
		})
	}
}

func TestSubmitFrameRejectsBadRequests(t *testing.T) {
	frame := pngFrame(t, 16, 16)

	tests := []struct {
		name       string
		body       any
		wantStatus int
	}{
		{"missing seq", submitFrameRequest{Nonce: "n", Frame: frame}, http.StatusBadRequest},
		{"negative seq", submitFrameRequest{Seq: -1, Nonce: "n", Frame: frame}, http.StatusBadRequest},
		{"missing nonce", submitFrameRequest{Seq: 1, Frame: frame}, http.StatusBadRequest},
		{"empty frame", submitFrameRequest{Seq: 1, Nonce: "n"}, http.StatusBadRequest},
		{"not base64", submitFrameRequest{Seq: 1, Nonce: "n", Frame: "!!!not base64!!!"}, http.StatusBadRequest},
		{"not an image", submitFrameRequest{
			Seq: 1, Nonce: "n",
			Frame: base64.StdEncoding.EncodeToString([]byte("this is plain text")),
		}, http.StatusUnsupportedMediaType},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeLiveness{session: newFakeSession(t)}
			rec := do(livenessRouter(t, fake), authed(http.MethodPost, "/v1/liveness/sessions/abc/frames", tt.body))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d\nbody: %s", rec.Code, tt.wantStatus, rec.Body)
			}
		})
	}
}

// The limit is enforced while the body arrives, not after it has all been
// buffered.
func TestSubmitFrameRejectsAnOversizedBody(t *testing.T) {
	fake := &fakeLiveness{session: newFakeSession(t)}

	huge := strings.Repeat("A", 512<<10) // well past the 64 KiB frame limit
	rec := do(livenessRouter(t, fake), authed(http.MethodPost, "/v1/liveness/sessions/abc/frames",
		submitFrameRequest{Seq: 1, Nonce: "n", Frame: huge}))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d\nbody: %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body)
	}
}

// Every replay and spoof defence must look identical from outside. A client
// that learns which one fired learns which one to work around.
func TestDomainErrorsMapToStatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"unknown session", liveness.ErrSessionNotFound, http.StatusNotFound},
		{"expired", liveness.ErrSessionExpired, http.StatusGone},
		{"already finished", liveness.ErrSessionFinished, http.StatusConflict},
		{"concurrent write", liveness.ErrVersionConflict, http.StatusConflict},
		{"replayed sequence", liveness.ErrSequenceReplay, http.StatusUnprocessableEntity},
		{"wrong nonce", liveness.ErrNonceMismatch, http.StatusUnprocessableEntity},
		{"still image", liveness.ErrStaticReplay, http.StatusUnprocessableEntity},
		{"spoof", liveness.ErrSpoofDetected, http.StatusUnprocessableEntity},
		{"identity changed", liveness.ErrIdentityChanged, http.StatusUnprocessableEntity},
	}

	frame := pngFrame(t, 16, 16)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeLiveness{session: newFakeSession(t), submitErr: tt.err}
			rec := do(livenessRouter(t, fake), authed(http.MethodPost, "/v1/liveness/sessions/abc/frames",
				submitFrameRequest{Seq: 1, Nonce: "n", Frame: frame}))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, tt.wantStatus, rec.Body)
			}

			// The internal reason must not travel back.
			body := rec.Body.String()
			for _, leak := range []string{"spoof", "identity", "nonce", "sequence", "replay"} {
				if strings.Contains(strings.ToLower(body), leak) {
					t.Errorf("the response names the defence that fired (%q): %s", leak, body)
				}
			}
		})
	}
}

func TestCompleteReturnsTheVerdict(t *testing.T) {
	fake := &fakeLiveness{verdict: liveness.Verdict{
		SessionID: "abc", State: liveness.StatePassed, Passed: true,
	}}

	rec := do(livenessRouter(t, fake), authed(http.MethodPost, "/v1/liveness/sessions/abc/complete", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, http.StatusOK, rec.Body)
	}

	var got completeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if !got.Passed || got.State != string(liveness.StatePassed) {
		t.Errorf("verdict = %+v, want a pass", got)
	}
}

func TestCompleteRefusesAnUnfinishedSession(t *testing.T) {
	fake := &fakeLiveness{completeErr: liveness.ErrChallengesIncomplete}

	rec := do(livenessRouter(t, fake), authed(http.MethodPost, "/v1/liveness/sessions/abc/complete", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

// The status endpoint must not become a way to read biometric data back out.
func TestStatusExposesNothingBiometric(t *testing.T) {
	session := newFakeSession(t)
	session.RecentHashes = []uint64{0xDEADBEEF}
	session.FailureReason = "frame flagged as a spoof"

	fake := &fakeLiveness{session: session}
	rec := do(livenessRouter(t, fake), authed(http.MethodGet, "/v1/liveness/sessions/abc", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, http.StatusOK, rec.Body)
	}

	body := rec.Body.String()
	for _, forbidden := range []string{"embedding", "recent_hashes", "deadbeef", "landmark", "failure_reason", "spoof"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("the status response contains %q: %s", forbidden, body)
		}
	}

	var got sessionStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got.SessionID == "" || got.State == "" {
		t.Errorf("status is missing the basics: %+v", got)
	}
}

func TestLivenessEndpointsRequireAnAPIKey(t *testing.T) {
	fake := &fakeLiveness{session: newFakeSession(t)}
	h := livenessRouter(t, fake)

	paths := []struct{ method, path string }{
		{http.MethodPost, "/v1/liveness/sessions"},
		{http.MethodGet, "/v1/liveness/sessions/abc"},
		{http.MethodPost, "/v1/liveness/sessions/abc/frames"},
		{http.MethodPost, "/v1/liveness/sessions/abc/complete"},
	}

	for _, p := range paths {
		t.Run(fmt.Sprintf("%s %s", p.method, p.path), func(t *testing.T) {
			rec := do(h, httptest.NewRequest(p.method, p.path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}
