package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	lastNonce string
}

func (f *fakeLiveness) Start(context.Context) (*liveness.Session, error) {
	return f.session, f.startErr
}

func (f *fakeLiveness) SubmitFrame(_ context.Context, id liveness.SessionID, in liveness.FrameInput) (liveness.FrameResult, error) {
	f.lastID = id
	f.lastInput = in
	return f.result, f.submitErr
}

func (f *fakeLiveness) Complete(_ context.Context, id liveness.SessionID, nonce string) (liveness.Verdict, error) {
	f.lastNonce = nonce
	f.lastID = id
	return f.verdict, f.completeErr
}

func (f *fakeLiveness) Get(_ context.Context, id liveness.SessionID, nonce string) (*liveness.Session, error) {
	f.lastNonce = nonce
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

// A wrong nonce is different in kind from the defences above, and is answered
// differently on purpose.
//
// It is a failure to authorise, not a failed verification: 403 rather than 422,
// and the message says plainly what is wrong. That leaks nothing — the caller
// already knows whether they sent a nonce — while an opaque "verification
// failed" would send an honest integrator hunting through their camera code for
// a bug in their header.
func TestAWrongNonceIsAnAuthorisationFailure(t *testing.T) {
	fake := &fakeLiveness{session: newFakeSession(t), getErr: liveness.ErrWrongNonce}

	req := httptest.NewRequest(http.MethodGet, "/v1/liveness/sessions/abc", nil)
	req.Header.Set(headerSessionNonce, "wrong")

	rec := do(livenessRouter(t, fake), req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, http.StatusForbidden, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "nonce") {
		t.Errorf("the response does not say what was wrong: %s", rec.Body)
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

// Creating a session is the integrator's call: it allocates a row and a slot of
// inference work, so it is the one that has to be attributable.
func TestCreatingASessionNeedsTheOperatorKey(t *testing.T) {
	fake := &fakeLiveness{session: newFakeSession(t)}

	rec := do(livenessRouter(t, fake), httptest.NewRequest(http.MethodPost, "/v1/liveness/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// The whole point of the split: a subject's browser operates the session it was
// handed without ever holding an operator credential.
func TestSessionScopedEndpointsNeedNoAPIKey(t *testing.T) {
	fake := &fakeLiveness{session: newFakeSession(t)}
	h := livenessRouter(t, fake)

	frame := pngFrame(t, 16, 16)

	calls := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"status", http.MethodGet, "/v1/liveness/sessions/abc", nil},
		{"complete", http.MethodPost, "/v1/liveness/sessions/abc/complete", nil},
		{
			"frames", http.MethodPost, "/v1/liveness/sessions/abc/frames",
			submitFrameRequest{Seq: 1, Nonce: "n", Frame: frame},
		},
	}

	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			var req *http.Request
			if c.body == nil {
				req = httptest.NewRequest(c.method, c.path, nil)
			} else {
				raw, _ := json.Marshal(c.body)
				req = httptest.NewRequest(c.method, c.path, bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
			}
			// No X-API-Key. The session nonce is the authorisation.
			req.Header.Set(headerSessionNonce, "the-nonce")

			rec := do(h, req)
			if rec.Code == http.StatusUnauthorized {
				t.Errorf("status = %d; a subject's browser was asked for an operator key\nbody: %s",
					rec.Code, rec.Body)
			}
		})
	}
}

// The nonce has to reach the service on the calls that have no body to carry
// it, or they would be authorising on the session id alone.
func TestNonceHeaderReachesTheService(t *testing.T) {
	for _, c := range []struct {
		name   string
		method string
		path   string
	}{
		{"status", http.MethodGet, "/v1/liveness/sessions/abc"},
		{"complete", http.MethodPost, "/v1/liveness/sessions/abc/complete"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeLiveness{session: newFakeSession(t)}

			req := httptest.NewRequest(c.method, c.path, nil)
			req.Header.Set(headerSessionNonce, "carried-through")
			do(livenessRouter(t, fake), req)

			if fake.lastNonce != "carried-through" {
				t.Errorf("the service received nonce %q, want the one from the header", fake.lastNonce)
			}
		})
	}
}

// With anonymous sessions enabled the demo can open one, and the operator key
// stops being required for that call too.
func TestAnonymousSessionsCanBeEnabled(t *testing.T) {
	fake := &fakeLiveness{session: newFakeSession(t)}

	cfg := testConfig()
	cfg.Server.AllowAnonymousSessions = true

	h, err := NewRouter(Deps{Config: cfg, Logger: discardLogger(), Liveness: fake})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}

	rec := do(h, httptest.NewRequest(http.MethodPost, "/v1/liveness/sessions", nil))
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d\nbody: %s", rec.Code, http.StatusCreated, rec.Body)
	}
}

// The anonymous setting must reach exactly one route.
//
// Scoped to a group instead, it would quietly open every endpoint anyone added
// to that group later — and the enrollment endpoints are going in next door.
func TestAnonymousSessionsDoNotOpenAnythingElse(t *testing.T) {
	for _, anonymous := range []bool{false, true} {
		name := "anonymous sessions off"
		if anonymous {
			name = "anonymous sessions on"
		}

		t.Run(name, func(t *testing.T) {
			fake := &fakeLiveness{session: newFakeSession(t)}

			cfg := testConfig()
			cfg.Server.AllowAnonymousSessions = anonymous

			h, err := NewRouter(Deps{Config: cfg, Logger: discardLogger(), Liveness: fake})
			if err != nil {
				t.Fatalf("NewRouter() returned an unexpected error: %v", err)
			}

			// A path that is not yet mounted stands in for the enrollment
			// endpoints. It must need the key either way.
			rec := do(h, httptest.NewRequest(http.MethodGet, "/v1/faces/search", nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d; the anonymous setting reached beyond session creation",
					rec.Code, http.StatusUnauthorized)
			}
		})
	}
}
