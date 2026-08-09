package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ziad/liveness-verifier/internal/enrollment"
)

// testAPIKey is one of the keys testConfig accepts.
const testAPIKey = "key-one"

// base64Of is the encoding a browser produces from canvas.toDataURL.
func base64Of(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// fakeEnrollment answers however a test needs.
type fakeEnrollment struct {
	enrollResult enrollment.EnrollResult
	enrollErr    error
	lastEnroll   enrollment.EnrollInput

	searchResult enrollment.SearchResult
	searchErr    error

	deleted    int
	deleteErr  error
	lastDelete string
}

func (f *fakeEnrollment) Enroll(_ context.Context, in enrollment.EnrollInput) (enrollment.EnrollResult, error) {
	f.lastEnroll = in
	return f.enrollResult, f.enrollErr
}

func (f *fakeEnrollment) Search(context.Context, image.Image) (enrollment.SearchResult, error) {
	return f.searchResult, f.searchErr
}

func (f *fakeEnrollment) DeleteSubject(_ context.Context, subject string) (int, error) {
	f.lastDelete = subject
	return f.deleted, f.deleteErr
}

// facesRouter builds a router with the gallery mounted.
func facesRouter(t *testing.T, fake *fakeEnrollment) http.Handler {
	t.Helper()

	h, err := NewRouter(Deps{Config: testConfig(), Logger: discardLogger(), Enrollment: fake})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}
	return h
}

// jpegPayload returns a small valid JPEG, base64 encoded the way a client sends
// one.
func jpegPayload(t *testing.T) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("could not build a test image: %v", err)
	}
	return "data:image/jpeg;base64," + base64Of(buf.Bytes())
}

func jsonRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("could not encode the request: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testAPIKey)
	return req
}

// Every gallery endpoint is operator territory. The liveness token authorises
// the capture; it does not authorise writing to the gallery.
func TestGalleryEndpointsNeedTheOperatorKey(t *testing.T) {
	fake := &fakeEnrollment{}
	router := facesRouter(t, fake)

	calls := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/v1/faces", enrollRequest{Token: "t", SubjectID: "s", Image: jpegPayload(t)}},
		{http.MethodPost, "/v1/faces/search", searchRequest{Image: jpegPayload(t)}},
		{http.MethodDelete, "/v1/faces", deleteSubjectRequest{SubjectID: "s"}},
	}

	for _, c := range calls {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			req := jsonRequest(t, c.method, c.path, c.body)
			req.Header.Del("X-API-Key")

			if rec := do(router, req); rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d without an API key", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestEnrollPassesTheRequestThrough(t *testing.T) {
	fake := &fakeEnrollment{enrollResult: enrollment.EnrollResult{
		FaceID: "face-1", SubjectID: "subject-1", SessionID: "session-1",
	}}

	rec := do(facesRouter(t, fake), jsonRequest(t, http.MethodPost, "/v1/faces",
		enrollRequest{Token: "the-token", SubjectID: "subject-1", Image: jpegPayload(t)}))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body)
	}
	if fake.lastEnroll.Token != "the-token" || fake.lastEnroll.SubjectID != "subject-1" {
		t.Errorf("the service received %+v, want the token and subject from the request", fake.lastEnroll)
	}
	if fake.lastEnroll.Image == nil {
		t.Error("the service received no image")
	}

	var got enrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if got.FaceID != "face-1" || got.SessionID != "session-1" {
		t.Errorf("response = %+v, want the face and session ids", got)
	}
}

func TestEnrollRejectsIncompleteRequests(t *testing.T) {
	tests := []struct {
		name string
		body enrollRequest
	}{
		{"no token", enrollRequest{SubjectID: "s", Image: "aGVsbG8="}},
		{"no subject", enrollRequest{Token: "t", Image: "aGVsbG8="}},
		{"no image", enrollRequest{Token: "t", SubjectID: "s"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(facesRouter(t, &fakeEnrollment{}),
				jsonRequest(t, http.MethodPost, "/v1/faces", tt.body))

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestEnrollmentErrorsMapToStatuses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"invalid token", enrollment.ErrTokenInvalid, http.StatusForbidden},
		{"identity mismatch", enrollment.ErrIdentityMismatch, http.StatusUnprocessableEntity},
		{"session gone", enrollment.ErrSessionUnavailable, http.StatusGone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeEnrollment{enrollErr: tt.err}

			rec := do(facesRouter(t, fake), jsonRequest(t, http.MethodPost, "/v1/faces",
				enrollRequest{Token: "t", SubjectID: "s", Image: jpegPayload(t)}))

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// The response must not repeat back how close a rejected face came. A caller
// who could read that would walk an image towards the threshold one request at
// a time.
func TestARejectedEnrollmentRevealsNoSimilarity(t *testing.T) {
	fake := &fakeEnrollment{enrollErr: enrollment.ErrIdentityMismatch}

	rec := do(facesRouter(t, fake), jsonRequest(t, http.MethodPost, "/v1/faces",
		enrollRequest{Token: "t", SubjectID: "s", Image: jpegPayload(t)}))

	body := rec.Body.String()
	for _, leak := range []string{"similarity", "cosine", "0.", "threshold"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("the refusal mentions %q, which measures how close the attempt got: %s", leak, body)
		}
	}
}

func TestSearchReportsCandidates(t *testing.T) {
	fake := &fakeEnrollment{searchResult: enrollment.SearchResult{
		Matched: true,
		Best:    enrollment.Match{FaceID: "face-1", SubjectID: "subject-1", Score: 0.91},
		Candidates: []enrollment.Match{
			{FaceID: "face-1", SubjectID: "subject-1", Score: 0.91},
			{FaceID: "face-2", SubjectID: "subject-2", Score: 0.31},
		},
	}}

	rec := do(facesRouter(t, fake), jsonRequest(t, http.MethodPost, "/v1/faces/search",
		searchRequest{Image: jpegPayload(t)}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body)
	}

	var got searchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if !got.Matched || got.Best == nil || got.Best.SubjectID != "subject-1" {
		t.Errorf("response = %+v, want a match on subject-1", got)
	}
	if len(got.Candidates) != 2 {
		t.Errorf("returned %d candidates, want 2", len(got.Candidates))
	}
}

func TestAnEmptyGalleryIsNotFound(t *testing.T) {
	fake := &fakeEnrollment{searchErr: enrollment.ErrNoMatch}

	rec := do(facesRouter(t, fake), jsonRequest(t, http.MethodPost, "/v1/faces/search",
		searchRequest{Image: jpegPayload(t)}))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// The subject id travels in the body, never in the path.
//
// A path segment reads as the RESTful choice and is the wrong one: a subject id
// is chosen by the integrator and is routinely a national identity or account
// number, and paths land in access logs, proxy logs, and browser history.
func TestTheSubjectNeverAppearsInTheURL(t *testing.T) {
	fake := &fakeEnrollment{deleted: 2}
	router := facesRouter(t, fake)

	const subject = "national-id-3175012509870001"

	rec := do(router, jsonRequest(t, http.MethodDelete, "/v1/faces",
		deleteSubjectRequest{SubjectID: subject}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body)
	}
	if fake.lastDelete != subject {
		t.Errorf("the service received %q, want %q", fake.lastDelete, subject)
	}

	// And the route that would have put it in the path must not exist.
	byPath := do(router, jsonRequest(t, http.MethodDelete, "/v1/faces/"+subject, nil))
	if byPath.Code != http.StatusNotFound && byPath.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /v1/faces/{subject} answered %d; the subject must not be routable in the path", byPath.Code)
	}
}

func TestDeleteReportsAnUnknownSubject(t *testing.T) {
	fake := &fakeEnrollment{deleteErr: enrollment.ErrSubjectNotFound}

	rec := do(facesRouter(t, fake), jsonRequest(t, http.MethodDelete, "/v1/faces",
		deleteSubjectRequest{SubjectID: "nobody"}))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// A deployment with no enrollment wired must not answer the gallery paths at
// all, and must still require a key before saying so.
func TestGalleryPathsAreAbsentWhenUnmounted(t *testing.T) {
	h, err := NewRouter(Deps{Config: testConfig(), Logger: discardLogger()})
	if err != nil {
		t.Fatalf("NewRouter() returned an unexpected error: %v", err)
	}

	if rec := do(h, jsonRequest(t, http.MethodPost, "/v1/faces", enrollRequest{})); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d when enrollment is not wired", rec.Code, http.StatusNotFound)
	}

	req := jsonRequest(t, http.MethodPost, "/v1/faces", enrollRequest{})
	req.Header.Del("X-API-Key")
	if rec := do(h, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; an unmounted path answered before the key check", rec.Code, http.StatusUnauthorized)
	}
}

func TestUnusedEnrollmentErrorsStillMap(t *testing.T) {
	if err := mapEnrollmentError(errors.New("something else")); err == nil {
		t.Error("an unclassified error mapped to nothing")
	}
}
