package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"log/slog"
	"net/http"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/enrollment"
	"github.com/ziad/liveness-verifier/internal/imaging"
)

// EnrollmentService is what the faces handler needs from the domain.
type EnrollmentService interface {
	Enroll(ctx context.Context, in enrollment.EnrollInput) (enrollment.EnrollResult, error)
	Search(ctx context.Context, img image.Image) (enrollment.SearchResult, error)
	DeleteSubject(ctx context.Context, subjectID string) (int, error)
}

// facesHandler serves the gallery endpoints.
type facesHandler struct {
	svc    EnrollmentService
	log    *slog.Logger
	limits imaging.Limits

	maxBodyBytes int64
}

func (h *facesHandler) enroll(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)

	var req enrollRequest
	if !decodeJSON(w, r, h.log, &req) {
		return
	}

	if req.Token == "" {
		respond(w, r, h.log, Fail(http.StatusBadRequest, CodeBadRequest, "token is required"))
		return
	}
	if req.SubjectID == "" {
		respond(w, r, h.log, Fail(http.StatusBadRequest, CodeBadRequest, "subject_id is required"))
		return
	}

	img, ok := decodeImage(w, r, h.log, req.Image, h.limits)
	if !ok {
		return
	}

	result, err := h.svc.Enroll(r.Context(), enrollment.EnrollInput{
		Token:     req.Token,
		SubjectID: req.SubjectID,
		Image:     img,
	})
	if err != nil {
		respond(w, r, h.log, mapEnrollmentError(err))
		return
	}

	writeJSON(w, h.log, http.StatusCreated, enrollResponse{
		FaceID:    result.FaceID.String(),
		SubjectID: result.SubjectID,
		SessionID: result.SessionID,
	})
}

func (h *facesHandler) search(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)

	var req searchRequest
	if !decodeJSON(w, r, h.log, &req) {
		return
	}

	img, ok := decodeImage(w, r, h.log, req.Image, h.limits)
	if !ok {
		return
	}

	result, err := h.svc.Search(r.Context(), img)
	if err != nil {
		respond(w, r, h.log, mapEnrollmentError(err))
		return
	}

	writeJSON(w, h.log, http.StatusOK, newSearchResponse(result))
}

// deleteSubject removes every face belonging to a subject.
//
// The subject travels in the body, not in the path.
//
// A path segment reads as the RESTful choice, and it is the wrong one here: a
// subject id is chosen by the integrator and is routinely a national identity
// number or an account number. Paths land in access logs, proxy logs, and
// browser history, none of which are places that identifier should reach. The
// same reasoning already keeps it out of query strings.
func (h *facesHandler) deleteSubject(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)

	var req deleteSubjectRequest
	if !decodeJSON(w, r, h.log, &req) {
		return
	}
	if req.SubjectID == "" {
		respond(w, r, h.log, Fail(http.StatusBadRequest, CodeBadRequest, "subject_id is required"))
		return
	}

	n, err := h.svc.DeleteSubject(r.Context(), req.SubjectID)
	if err != nil {
		respond(w, r, h.log, mapEnrollmentError(err))
		return
	}

	writeJSON(w, h.log, http.StatusOK, deleteSubjectResponse{FacesRemoved: n})
}

// decodeJSON reads a JSON body, answering the caller on failure.
//
// Returns false when it has already written a response, so the caller returns
// without writing a second one.
func decodeJSON(w http.ResponseWriter, r *http.Request, log *slog.Logger, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respond(w, r, log, Fail(http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
				"the request is larger than this endpoint accepts"))
			return false
		}
		respond(w, r, log, FailWith(http.StatusBadRequest, CodeBadRequest,
			"the request body is not valid JSON", err))
		return false
	}
	return true
}

// decodeImage turns a base64 payload into an image, answering on failure.
func decodeImage(w http.ResponseWriter, r *http.Request, log *slog.Logger, payload string, limits imaging.Limits) (image.Image, bool) {
	raw, err := decodeFramePayload(payload)
	if err != nil {
		respond(w, r, log, FailWith(http.StatusBadRequest, CodeBadRequest,
			"the image is not valid base64", err))
		return nil, false
	}

	img, err := imaging.Decode(bytes.NewReader(raw), limits)
	if err != nil {
		respond(w, r, log, mapImagingError(err))
		return nil, false
	}
	return img, true
}

// mapEnrollmentError turns a domain error into a response.
//
// As on the liveness path, the status carries the meaning and the message does
// not name which defence fired.
func mapEnrollmentError(err error) error {
	switch {
	// An invalid, spent, or expired token are one answer, matching the domain.
	// Anything finer tells a caller whether a token they captured was real.
	case errors.Is(err, enrollment.ErrTokenInvalid):
		return FailWith(http.StatusForbidden, CodeForbidden,
			"the liveness token is missing, expired, or already used", err)

	// A face that does not match the verified capture is a failed verification,
	// not a malformed request.
	case errors.Is(err, enrollment.ErrIdentityMismatch):
		return FailWith(http.StatusUnprocessableEntity, CodeUnprocessable,
			"the submitted face does not match the verified capture", err)

	case errors.Is(err, enrollment.ErrSessionUnavailable):
		return FailWith(http.StatusGone, CodeGone,
			"the verified capture is no longer available; verify again", err)

	case errors.Is(err, enrollment.ErrSubjectNotFound):
		return FailWith(http.StatusNotFound, CodeNotFound, "no faces are enrolled for that subject", err)

	case errors.Is(err, enrollment.ErrNoMatch):
		return FailWith(http.StatusNotFound, CodeNotFound, "the gallery is empty", err)

	case errors.Is(err, biometric.ErrNoFaceFound), errors.Is(err, biometric.ErrLowQuality):
		return FailWith(http.StatusUnprocessableEntity, CodeUnprocessable,
			"the image is not usable; move closer, hold steady, and use good light", err)

	default:
		return FailWith(http.StatusInternalServerError, CodeInternal, "the request could not be completed", err)
	}
}
