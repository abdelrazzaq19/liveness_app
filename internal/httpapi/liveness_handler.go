package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/imaging"
	"github.com/ziad/liveness-verifier/internal/liveness"
)

// LivenessService is what the handler needs from the domain.
//
// Declared here rather than taken as the concrete type so the handler can be
// tested without a database behind it.
type LivenessService interface {
	Start(ctx context.Context) (*liveness.Session, error)
	SubmitFrame(ctx context.Context, id liveness.SessionID, in liveness.FrameInput) (liveness.FrameResult, error)
	Complete(ctx context.Context, id liveness.SessionID, nonce string) (liveness.Verdict, error)
	Get(ctx context.Context, id liveness.SessionID, nonce string) (*liveness.Session, error)
}

// headerSessionNonce carries the session's nonce on calls that have no body to
// put it in.
//
// A header rather than a query parameter: query strings end up in access logs,
// browser history, and referrer headers, and this one is a credential.
const headerSessionNonce = "X-Session-Nonce"

// livenessHandler serves the session endpoints.
type livenessHandler struct {
	svc    LivenessService
	log    *slog.Logger
	limits imaging.Limits

	// maxBodyBytes caps the request body.
	//
	// Larger than the frame limit because base64 inflates by a third, plus room
	// for the surrounding JSON. Enforced on the reader, so an oversized upload
	// is refused while it arrives rather than after it has all been buffered.
	maxBodyBytes int64
}

// The routes are registered in router.go rather than here, because they no
// longer share one middleware chain: creating a session needs an operator key,
// operating one needs the session's nonce, and a single mount point cannot say
// that.

func (h *livenessHandler) start(w http.ResponseWriter, r *http.Request) {
	session, err := h.svc.Start(r.Context())
	if err != nil {
		respond(w, r, h.log, FailWith(http.StatusInternalServerError, CodeInternal,
			"could not start a verification session", err))
		return
	}
	writeJSON(w, h.log, http.StatusCreated, newStartSessionResponse(session, time.Now()))
}

func (h *livenessHandler) status(w http.ResponseWriter, r *http.Request) {
	id := liveness.SessionID(chi.URLParam(r, "id"))

	session, err := h.svc.Get(r.Context(), id, r.Header.Get(headerSessionNonce))
	if err != nil {
		respond(w, r, h.log, mapLivenessError(err))
		return
	}
	writeJSON(w, h.log, http.StatusOK, newSessionStatusResponse(session, time.Now()))
}

func (h *livenessHandler) complete(w http.ResponseWriter, r *http.Request) {
	id := liveness.SessionID(chi.URLParam(r, "id"))

	verdict, err := h.svc.Complete(r.Context(), id, r.Header.Get(headerSessionNonce))
	if err != nil {
		respond(w, r, h.log, mapLivenessError(err))
		return
	}

	writeJSON(w, h.log, http.StatusOK, completeResponse{
		SessionID: verdict.SessionID.String(),
		State:     string(verdict.State),
		Passed:    verdict.Passed,
	})
}

func (h *livenessHandler) submitFrame(w http.ResponseWriter, r *http.Request) {
	id := liveness.SessionID(chi.URLParam(r, "id"))

	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)

	var req submitFrameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respond(w, r, h.log, Fail(http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
				"the frame is larger than this endpoint accepts"))
			return
		}
		respond(w, r, h.log, FailWith(http.StatusBadRequest, CodeBadRequest,
			"the request body is not valid JSON", err))
		return
	}

	if req.Seq <= 0 {
		respond(w, r, h.log, Fail(http.StatusBadRequest, CodeBadRequest,
			"seq must be a positive, strictly increasing number"))
		return
	}
	if req.Nonce == "" {
		respond(w, r, h.log, Fail(http.StatusBadRequest, CodeBadRequest, "nonce is required"))
		return
	}

	raw, err := decodeFramePayload(req.Frame)
	if err != nil {
		respond(w, r, h.log, FailWith(http.StatusBadRequest, CodeBadRequest,
			"the frame is not valid base64", err))
		return
	}

	img, err := imaging.Decode(bytes.NewReader(raw), h.limits)
	if err != nil {
		respond(w, r, h.log, mapImagingError(err))
		return
	}

	result, err := h.svc.SubmitFrame(r.Context(), id, liveness.FrameInput{
		Seq:   req.Seq,
		Nonce: req.Nonce,
		PHash: imaging.PHash(img),
		Image: img,
	})
	if err != nil {
		respond(w, r, h.log, mapLivenessError(err))
		return
	}

	writeJSON(w, h.log, http.StatusOK, newSubmitFrameResponse(result))
}

// decodeFramePayload accepts a bare base64 string or a browser data URL.
//
// canvas.toDataURL produces the latter, and stripping it here keeps the client
// from having to do string surgery on its own output.
func decodeFramePayload(payload string) ([]byte, error) {
	if payload == "" {
		return nil, errors.New("frame is empty")
	}
	if idx := strings.Index(payload, ";base64,"); idx >= 0 && strings.HasPrefix(payload, "data:") {
		payload = payload[idx+len(";base64,"):]
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
}

// mapLivenessError turns a domain error into a response.
//
// The status codes carry the meaning; the messages deliberately do not name
// which defence fired. A client that learns it was the identity check rather
// than the spoof check learns which one to work around next.
func mapLivenessError(err error) error {
	switch {
	case errors.Is(err, liveness.ErrSessionNotFound):
		return FailWith(http.StatusNotFound, CodeNotFound, "no such session", err)

	// A wrong nonce is a failure to authorise, not a failed verification. It
	// must never touch the session: otherwise anyone who learned an id could
	// destroy somebody else's attempt by sending one bad request.
	case errors.Is(err, liveness.ErrWrongNonce):
		return FailWith(http.StatusForbidden, CodeForbidden,
			"the session nonce is missing or wrong", err)

	case errors.Is(err, liveness.ErrSessionExpired):
		return FailWith(http.StatusGone, CodeGone, "the session expired", err)

	case errors.Is(err, liveness.ErrSessionFinished):
		return FailWith(http.StatusConflict, CodeConflict, "the session has already finished", err)

	case errors.Is(err, liveness.ErrVersionConflict):
		return FailWith(http.StatusConflict, CodeConflict,
			"another frame for this session is being processed; retry", err)

	case errors.Is(err, liveness.ErrChallengesIncomplete):
		return FailWith(http.StatusConflict, CodeConflict,
			"the session has challenges still to complete", err)

	// Every replay and spoof defence collapses to one response. They are
	// different failures internally and must look identical from outside.
	case errors.Is(err, liveness.ErrSequenceReplay),
		errors.Is(err, liveness.ErrStaticReplay),
		errors.Is(err, liveness.ErrSpoofDetected),
		errors.Is(err, liveness.ErrIdentityChanged):
		return FailWith(http.StatusUnprocessableEntity, CodeUnprocessable,
			"verification failed", err)

	default:
		// A failed session surfaces as the recorded reason, which is not a
		// sentinel. It is still a verification failure rather than a fault.
		return FailWith(http.StatusUnprocessableEntity, CodeUnprocessable, "verification failed", err)
	}
}

func mapImagingError(err error) error {
	switch {
	case errors.Is(err, imaging.ErrTooLarge), errors.Is(err, imaging.ErrTooManyPixels):
		return FailWith(http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			"the frame is larger than this endpoint accepts", err)

	case errors.Is(err, imaging.ErrUnsupportedFormat):
		return FailWith(http.StatusUnsupportedMediaType, CodeBadRequest,
			"the frame must be a JPEG or a PNG", err)

	case errors.Is(err, imaging.ErrCorrupt):
		return FailWith(http.StatusBadRequest, CodeBadRequest, "the frame could not be decoded", err)

	case errors.Is(err, biometric.ErrLowQuality):
		return FailWith(http.StatusUnprocessableEntity, CodeUnprocessable,
			"the frame is not usable; hold steady in good light", err)

	default:
		return FailWith(http.StatusBadRequest, CodeBadRequest, "the frame could not be read", err)
	}
}
