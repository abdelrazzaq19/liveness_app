// Package enrollment owns the face gallery: registering a face against a
// subject, and finding a subject from a face.
//
// Like internal/liveness, it declares the interfaces it needs and owns none of
// their implementations. Nothing here imports a database driver, an object
// store client, or net/http.
package enrollment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// Errors callers are expected to branch on.
var (
	// ErrFaceNotFound means no row exists with that id.
	ErrFaceNotFound = errors.New("enrollment: face not found")

	// ErrSubjectNotFound means the subject has no enrolled faces.
	ErrSubjectNotFound = errors.New("enrollment: subject has no enrolled faces")

	// ErrNoMatch means the search found nothing above the similarity floor.
	//
	// Distinct from an empty gallery on purpose: "nobody matched" and "there was
	// nobody to match against" are different operational problems, and a caller
	// that cannot tell them apart will misdiagnose an empty database as a
	// recognition failure.
	ErrNoMatch = errors.New("enrollment: no face in the gallery matched")
)

// normTolerance is how far a stored embedding may sit from the unit sphere.
//
// Loose enough for float32 accumulated over 512 elements, tight enough that a
// vector which was never normalised at all cannot pass.
const normTolerance = 1e-4

// FaceID identifies one enrolled capture.
type FaceID string

func (id FaceID) String() string { return string(id) }

// Face is one enrolled capture of one subject.
//
// A subject may have several. Enrolling the same person from more than one
// capture is what makes 1:N search robust to pose and lighting, and averaging
// them into a single vector would discard exactly that.
type Face struct {
	ID        FaceID    `json:"id"`
	SubjectID string    `json:"subject_id"`
	CreatedAt time.Time `json:"created_at"`

	// SessionID is the liveness session that authorised this enrollment.
	//
	// Recorded so that an enrollment can always be traced back to a verified
	// capture. It is not a foreign key: sessions are purged on a schedule, and
	// losing the session row must not take the provenance with it.
	SessionID string `json:"session_id"`

	// ArtifactKey is the object store key of the aligned crop, when one was
	// kept. Empty means no image was retained, which is the default.
	ArtifactKey string `json:"artifact_key,omitempty"`

	// Embedding is never serialised. It is the biometric template itself:
	// enough to impersonate the subject against this system and, because
	// ArcFace vectors transfer, against others using the same model.
	Embedding biometric.Embedding `json:"-"`
}

// Validate reports a face that must not be stored.
func (f Face) Validate() error {
	var problems []error

	if f.ID == "" {
		problems = append(problems, errors.New("id is required"))
	}
	if f.SubjectID == "" {
		problems = append(problems, errors.New("subject_id is required"))
	}
	if f.SessionID == "" {
		// Enforced here rather than left to the caller: an enrollment with no
		// liveness session behind it is exactly the thing this system exists to
		// prevent, and a missing field is how that would happen quietly.
		problems = append(problems, errors.New("session_id is required; an enrollment must be traceable to a verified capture"))
	}
	if err := f.Embedding.Validate(); err != nil {
		problems = append(problems, fmt.Errorf("embedding: %w", err))
	} else if n := f.Embedding.Norm(); math.Abs(n-1) > normTolerance {
		// Checked here rather than left to the storage layer, and checked at
		// all because biometric.Embedding.Validate deliberately does not: it
		// only asks that a vector have some magnitude, which is the right rule
		// for a vector still being computed.
		//
		// A gallery is different. Every threshold in this system is a cosine
		// similarity, and a vector that is not on the unit sphere compares
		// against the stored ones on a different scale from the one those
		// thresholds were chosen for. Refused rather than normalised on the
		// way in: the embedder already normalises, so a vector arriving here
		// without a unit norm means something upstream went wrong, and quietly
		// fixing it would hide that.
		problems = append(problems, fmt.Errorf(
			"embedding must be L2-normalised for cosine comparison, norm is %.6f", n))
	}

	return errors.Join(problems...)
}

// Match is one search result.
type Match struct {
	FaceID    FaceID  `json:"face_id"`
	SubjectID string  `json:"subject_id"`
	Score     float64 `json:"score"`
}

// Repository stores and searches the gallery.
//
// Search returns results ordered by descending similarity. It applies no
// threshold of its own: deciding what counts as a match is the service's
// judgement, and a repository that silently filtered would make that decision
// untestable and invisible.
type Repository interface {
	// Insert stores a face together with the audit entry describing it, in one
	// transaction.
	//
	// The two travel together because the rule they enforce is only real if they
	// cannot come apart: a stored template with no record of who stored it, or
	// on whose authority, is precisely what an audit trail exists to prevent. An
	// interface that took them separately would make that a habit somebody has
	// to remember rather than something the type system insists on.
	Insert(ctx context.Context, f Face, entry AuditEntry) error

	// Search returns at most topK candidates, most similar first.
	Search(ctx context.Context, query biometric.Embedding, topK int) ([]Match, error)

	// DeleteSubject removes every face belonging to a subject and reports how
	// many rows went. The embedding is deleted outright rather than flagged: a
	// biometric template that is still readable has not been deleted.
	//
	// The audit entry is written in the same transaction and survives the rows
	// it describes. That is the whole point of keeping the trail in a separate
	// table: the record of a deletion must outlive the deletion.
	DeleteSubject(ctx context.Context, subjectID string, entry AuditEntry) (int, error)

	// Count reports how many faces are in the gallery, for readiness reporting
	// and for telling an empty gallery apart from a failed match.
	Count(ctx context.Context) (int, error)
}
