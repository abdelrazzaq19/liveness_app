package enrollment

import (
	"context"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// Errors the enrollment path can return.
var (
	// ErrIdentityMismatch means the submitted face is not the face that was
	// verified live.
	//
	// This is the defence that makes a liveness token mean something. A token
	// proves that some session passed; on its own it says nothing about whose
	// face is in the image that arrives with it. Without this check an attacker
	// passes liveness with their own face and then enrols anybody's photograph,
	// which is precisely the fraud the whole system exists to stop.
	ErrIdentityMismatch = errors.New("enrollment: the submitted face is not the one that was verified")

	// ErrSessionUnavailable means the liveness session behind a token can no
	// longer be read, so identity cannot be bound.
	ErrSessionUnavailable = errors.New("enrollment: the verified capture is no longer available")
)

// LivenessSessions is the little the enrollment path needs to know about
// liveness: what the verified face looked like.
//
// Declared here rather than imported from that package so the dependency points
// inwards. internal/liveness must not know enrollment exists.
type LivenessSessions interface {
	// ReferenceEmbedding returns the descriptor of the first key frame of a
	// passed session — the face that was actually verified.
	ReferenceEmbedding(ctx context.Context, sessionID string) (biometric.Embedding, error)
}

// ArtifactEncoder turns a capture into the bytes to retain.
//
// A port rather than a call into an imaging package, for the same reason the
// repository is one: this package decides what is worth keeping and when, not
// how a JPEG is written. It also lets the aligned crop be swapped for something
// else without the domain noticing.
//
// The aligned crop is what the wiring supplies today: it is what the embedding
// was computed from, it is 112x112 rather than megapixels, and it excludes
// whatever else happened to be in the room.
type ArtifactEncoder func(img image.Image, kps biometric.Keypoints) (data []byte, contentType string, err error)

// IDGenerator produces face identifiers.
type IDGenerator interface {
	NewID() (FaceID, error)
}

// Clock reports the current time.
type Clock interface {
	Now() time.Time
}

// Config is what the enrollment service needs from configuration.
type Config struct {
	// MatchCosineMin is the similarity a search result must reach to count as
	// the same person.
	MatchCosineMin float64

	// IdentityCosineMin is the similarity the submitted face must keep with the
	// face verified in the liveness session.
	IdentityCosineMin float64

	// SearchTopK is how many candidates a search returns.
	SearchTopK int

	// StoreArtifacts decides whether the aligned crop is kept.
	StoreArtifacts bool
}

func (c Config) validate() error {
	var problems []error
	if c.MatchCosineMin <= 0 || c.MatchCosineMin > 1 {
		problems = append(problems, fmt.Errorf("MatchCosineMin must be in (0,1], got %g", c.MatchCosineMin))
	}
	if c.IdentityCosineMin <= 0 || c.IdentityCosineMin > 1 {
		problems = append(problems, fmt.Errorf("IdentityCosineMin must be in (0,1], got %g", c.IdentityCosineMin))
	}
	if c.SearchTopK < 1 {
		problems = append(problems, fmt.Errorf("SearchTopK must be at least 1, got %d", c.SearchTopK))
	}
	return errors.Join(problems...)
}

// Deps is everything the service needs built for it.
type Deps struct {
	Faces     Repository
	Tokens    *TokenService
	Sessions  LivenessSessions
	Analyzer  biometric.Analyzer
	Artifacts ArtifactStore

	// EncodeArtifact is only consulted when Config.StoreArtifacts is on.
	EncodeArtifact ArtifactEncoder

	IDs    IDGenerator
	Clock  Clock
	Logger *slog.Logger
	Config Config
}

// Service registers faces and finds subjects.
type Service struct {
	deps Deps
}

// NewService validates the wiring.
func NewService(d Deps) (*Service, error) {
	var problems []error

	if d.Faces == nil {
		problems = append(problems, errors.New("Faces repository is required"))
	}
	if d.Tokens == nil {
		problems = append(problems, errors.New("Tokens service is required; enrollment without a liveness token is the thing this system prevents"))
	}
	if d.Sessions == nil {
		problems = append(problems, errors.New("Sessions reader is required; without it a token cannot be bound to a face"))
	}
	if d.Analyzer == nil {
		problems = append(problems, errors.New("Analyzer is required"))
	}
	if d.IDs == nil {
		problems = append(problems, errors.New("IDs generator is required"))
	}
	if d.Clock == nil {
		problems = append(problems, errors.New("Clock is required"))
	}
	if d.Logger == nil {
		problems = append(problems, errors.New("Logger is required"))
	}
	if d.Config.StoreArtifacts {
		if d.Artifacts == nil {
			problems = append(problems, errors.New("Artifacts store is required when StoreArtifacts is on"))
		}
		if d.EncodeArtifact == nil {
			problems = append(problems, errors.New("EncodeArtifact is required when StoreArtifacts is on"))
		}
	}
	if err := d.Config.validate(); err != nil {
		problems = append(problems, err)
	}

	if err := errors.Join(problems...); err != nil {
		return nil, fmt.Errorf("enrollment: service: %w", err)
	}
	return &Service{deps: d}, nil
}

// EnrollInput is one registration request.
type EnrollInput struct {
	// Token is the single-use liveness token. Without a valid one nothing is
	// enrolled, whatever else the request carries.
	Token string

	// SubjectID is who this face belongs to, in the caller's own namespace.
	SubjectID string

	// Image is the capture to enrol.
	Image image.Image
}

// EnrollResult is what a successful registration produced.
type EnrollResult struct {
	FaceID    FaceID `json:"face_id"`
	SubjectID string `json:"subject_id"`

	// SessionID is the liveness session that authorised it, echoed so the
	// caller can tie their own records to the verification.
	SessionID string `json:"session_id"`
}

// Enroll registers a face against a subject.
//
// The order matters and is not an implementation detail. The token is spent
// first, so a request that fails later cannot be retried with the same one; the
// identity check runs before anything is written, so a face that does not match
// the verified capture never reaches the gallery.
func (s *Service) Enroll(ctx context.Context, in EnrollInput) (EnrollResult, error) {
	if in.SubjectID == "" {
		return EnrollResult{}, errors.New("enrollment: subject_id is required")
	}
	if in.Image == nil {
		return EnrollResult{}, errors.New("enrollment: an image is required")
	}

	now := s.deps.Clock.Now()

	// Spent before the work, not after. A token that survives a failed
	// enrollment is a token that can be replayed until something gets through,
	// and the cost of that is one wasted verification for an honest subject
	// against unlimited attempts for an attacker.
	sessionID, err := s.deps.Tokens.Redeem(ctx, in.Token, now)
	if err != nil {
		return EnrollResult{}, err
	}

	verified, err := s.deps.Sessions.ReferenceEmbedding(ctx, sessionID)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("%w: %v", ErrSessionUnavailable, err)
	}
	if err := verified.Validate(); err != nil {
		return EnrollResult{}, fmt.Errorf("%w: session %s has no usable reference", ErrSessionUnavailable, sessionID)
	}

	face, err := s.deps.Analyzer.Analyze(ctx, in.Image, biometric.AnalyzeOptions{})
	if err != nil {
		return EnrollResult{}, fmt.Errorf("enrollment: analyse capture: %w", err)
	}
	if err := face.Embedding.Validate(); err != nil {
		return EnrollResult{}, fmt.Errorf("enrollment: capture produced no usable descriptor: %w", err)
	}

	// The check that gives the token its meaning.
	if similarity := verified.Cosine(face.Embedding); similarity < s.deps.Config.IdentityCosineMin {
		// The similarity is logged, never returned. A caller who could read it
		// would learn how close they got, and could walk an image towards the
		// threshold one request at a time.
		s.deps.Logger.WarnContext(ctx, "enrollment refused: submitted face does not match the verified capture",
			slog.String("session_id", sessionID),
			slog.Float64("similarity", similarity),
			slog.Float64("required", s.deps.Config.IdentityCosineMin),
		)
		return EnrollResult{}, ErrIdentityMismatch
	}

	id, err := s.deps.IDs.NewID()
	if err != nil {
		return EnrollResult{}, fmt.Errorf("enrollment: new face id: %w", err)
	}

	stored := Face{
		ID:        id,
		SubjectID: in.SubjectID,
		SessionID: sessionID,
		CreatedAt: now,
		Embedding: face.Embedding,
	}

	// The artifact is written before the row, so a crash between the two leaves
	// an orphaned object rather than a row pointing at nothing. An unreferenced
	// object is rubbish to collect; a row promising an image that is not there
	// is a bug every reader has to handle.
	if s.deps.Config.StoreArtifacts {
		key := ArtifactKeyFor(id)
		if err := s.storeArtifact(ctx, key, in.Image, face); err != nil {
			return EnrollResult{}, err
		}
		stored.ArtifactKey = key
	}

	if err := s.deps.Faces.Insert(ctx, stored); err != nil {
		return EnrollResult{}, fmt.Errorf("enrollment: store face: %w", err)
	}

	s.deps.Logger.InfoContext(ctx, "face enrolled",
		slog.String("face_id", id.String()),
		slog.String("session_id", sessionID),
		slog.Bool("artifact_stored", stored.ArtifactKey != ""),
	)

	return EnrollResult{FaceID: id, SubjectID: in.SubjectID, SessionID: sessionID}, nil
}

// SearchResult is what a 1:N lookup found.
type SearchResult struct {
	// Matched says whether the best candidate cleared the threshold.
	Matched bool `json:"matched"`

	// Best is the top candidate when Matched is true.
	Best Match `json:"best,omitempty"`

	// Candidates are the results considered, most similar first. Included so an
	// operator can see a near miss, which is what makes a threshold tunable.
	Candidates []Match `json:"candidates,omitempty"`
}

// Search finds the subject a face belongs to.
func (s *Service) Search(ctx context.Context, img image.Image) (SearchResult, error) {
	if img == nil {
		return SearchResult{}, errors.New("enrollment: an image is required")
	}

	face, err := s.deps.Analyzer.Analyze(ctx, img, biometric.AnalyzeOptions{})
	if err != nil {
		return SearchResult{}, fmt.Errorf("enrollment: analyse query: %w", err)
	}
	if err := face.Embedding.Validate(); err != nil {
		return SearchResult{}, fmt.Errorf("enrollment: query produced no usable descriptor: %w", err)
	}

	candidates, err := s.deps.Faces.Search(ctx, face.Embedding, s.deps.Config.SearchTopK)
	if err != nil {
		return SearchResult{}, fmt.Errorf("enrollment: search gallery: %w", err)
	}
	if len(candidates) == 0 {
		// An empty gallery and a gallery where nobody matched are different
		// operational problems, and a caller that cannot tell them apart will
		// diagnose an empty database as a recognition failure.
		n, cerr := s.deps.Faces.Count(ctx)
		if cerr == nil && n == 0 {
			return SearchResult{}, ErrNoMatch
		}
		return SearchResult{Matched: false}, nil
	}

	result := SearchResult{Candidates: candidates}
	if candidates[0].Score >= s.deps.Config.MatchCosineMin {
		result.Matched = true
		result.Best = candidates[0]
	}
	return result, nil
}

// DeleteSubject removes every face belonging to a subject.
func (s *Service) DeleteSubject(ctx context.Context, subjectID string) (int, error) {
	if subjectID == "" {
		return 0, errors.New("enrollment: subject_id is required")
	}

	n, err := s.deps.Faces.DeleteSubject(ctx, subjectID)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrSubjectNotFound
	}

	s.deps.Logger.InfoContext(ctx, "subject deleted from the gallery",
		slog.Int("faces_removed", n),
	)
	return n, nil
}

// storeArtifact writes the retained image.
func (s *Service) storeArtifact(ctx context.Context, key string, img image.Image, face biometric.Face) error {
	data, contentType, err := s.deps.EncodeArtifact(img, face.Keypoints)
	if err != nil {
		return fmt.Errorf("enrollment: encode artifact: %w", err)
	}
	if err := s.deps.Artifacts.Put(ctx, key, data, contentType); err != nil {
		return fmt.Errorf("enrollment: store artifact: %w", err)
	}
	return nil
}
