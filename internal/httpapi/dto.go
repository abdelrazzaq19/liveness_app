package httpapi

import (
	"time"

	"github.com/ziad/liveness-verifier/internal/liveness"
)

// startSessionResponse is what a client needs to begin.
//
// The challenges are listed in the order they will be asked, because the client
// has to show them. Their unpredictability is what defends against a recording,
// and that survives the client knowing them: the attacker would have to prepare
// the recording after the session starts and inside its ninety seconds.
type startSessionResponse struct {
	SessionID         string    `json:"session_id"`
	Nonce             string    `json:"nonce"`
	Challenges        []string  `json:"challenges"`
	ExpiresAt         time.Time `json:"expires_at"`
	ChallengeDeadline time.Time `json:"challenge_deadline"`
}

func newStartSessionResponse(s *liveness.Session) startSessionResponse {
	return startSessionResponse{
		SessionID:         s.ID.String(),
		Nonce:             s.Nonce,
		Challenges:        challengeNames(s.Challenges),
		ExpiresAt:         s.ExpiresAt,
		ChallengeDeadline: s.ChallengeDeadline,
	}
}

// submitFrameRequest carries one captured frame.
type submitFrameRequest struct {
	// Seq must increase strictly across the session.
	Seq int64 `json:"seq"`

	// Nonce ties the frame to the session it was issued for.
	Nonce string `json:"nonce"`

	// Frame is the image, base64 encoded. A browser data URL prefix is
	// accepted, since that is what canvas.toDataURL produces.
	Frame string `json:"frame"`
}

// submitFrameResponse is what the client shows the subject next.
//
// It deliberately carries no score, no landmark, and no embedding. A client
// that could see how close a frame came to the spoof threshold could tune an
// attack against it one request at a time.
type submitFrameResponse struct {
	State     string `json:"state"`
	Challenge string `json:"challenge,omitempty"`
	Advanced  bool   `json:"advanced"`
	Completed bool   `json:"completed"`
	Remaining int    `json:"remaining"`

	// Reason tells the subject what to do differently. It never names which
	// defence rejected the frame.
	Reason string `json:"reason,omitempty"`
}

func newSubmitFrameResponse(r liveness.FrameResult) submitFrameResponse {
	return submitFrameResponse{
		State:     string(r.State),
		Challenge: string(r.Challenge),
		Advanced:  r.Advanced,
		Completed: r.Completed,
		Remaining: r.Remaining,
		Reason:    r.Reason,
	}
}

// completeResponse is the verdict.
type completeResponse struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Passed    bool   `json:"passed"`
}

// sessionStatusResponse describes a session without exposing anything
// biometric.
type sessionStatusResponse struct {
	SessionID  string    `json:"session_id"`
	State      string    `json:"state"`
	Challenge  string    `json:"challenge,omitempty"`
	Challenges []string  `json:"challenges"`
	Remaining  int       `json:"remaining"`
	Completed  bool      `json:"completed"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func newSessionStatusResponse(s *liveness.Session) sessionStatusResponse {
	return sessionStatusResponse{
		SessionID:  s.ID.String(),
		State:      string(s.State),
		Challenge:  string(s.ActiveChallenge()),
		Challenges: challengeNames(s.Challenges),
		Remaining:  s.Remaining(),
		Completed:  s.AllComplete(),
		ExpiresAt:  s.ExpiresAt,
	}
}

func challengeNames(cs []liveness.ChallengeKind) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}
