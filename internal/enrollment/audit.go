package enrollment

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AuditAction names what was attempted.
type AuditAction string

const (
	AuditEnroll AuditAction = "ENROLL"
	AuditSearch AuditAction = "SEARCH"
	AuditDelete AuditAction = "DELETE"
)

// AuditOutcome is the answer, deliberately coarse.
//
// "Refused" is recorded; which defence refused is not. This table is read by
// more people than the logs are, and a reviewer who can see that an attempt was
// rejected does not also need to know it was the identity check rather than the
// token — that is the sort of detail that turns an audit trail into a manual
// for getting past it.
type AuditOutcome string

const (
	AuditOK      AuditOutcome = "OK"
	AuditRefused AuditOutcome = "REFUSED"
)

// AuditEntry is one line of the trail.
//
// It carries no embedding, no image, and no score. An audit log is the record
// that is kept longest and read most widely, so it holds what happened and to
// whom, never the biometric itself.
type AuditEntry struct {
	At      time.Time
	Action  AuditAction
	Outcome AuditOutcome

	// Not every action has every field: a search has no face until it finds
	// one, and a deletion has no session behind it.
	SubjectID string
	FaceID    FaceID
	SessionID string

	// Affected is how many rows the action touched, which is the only thing a
	// deletion has to report.
	Affected int
}

// Validate reports an entry that would make the trail unreadable.
func (e AuditEntry) Validate() error {
	var problems []error

	switch e.Action {
	case AuditEnroll, AuditSearch, AuditDelete:
	default:
		problems = append(problems, fmt.Errorf("unknown action %q", e.Action))
	}
	switch e.Outcome {
	case AuditOK, AuditRefused:
	default:
		problems = append(problems, fmt.Errorf("unknown outcome %q", e.Outcome))
	}
	if e.At.IsZero() {
		problems = append(problems, errors.New("At is required; an entry with no time cannot be ordered"))
	}

	return errors.Join(problems...)
}

// Audit records actions that do not write to the gallery.
//
// Writes to the gallery are audited by the repository instead, in the same
// transaction as the row they describe. Splitting it this way is deliberate:
// the guarantee that no template is stored without a record is structural
// rather than a habit somebody has to remember, and an interface that let a
// caller store a face and then forget the audit would make it a habit again.
type Audit interface {
	Record(ctx context.Context, e AuditEntry) error
}
