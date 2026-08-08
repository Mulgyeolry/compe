package model

import "time"

// EvidenceField identifies a single deterministic piece of competition
// evidence that a research pass may need to fill in.
type EvidenceField string

const (
	// EvidenceRegistrationStart covers the competition's registration-opening
	// date.
	EvidenceRegistrationStart EvidenceField = "registration_start"
	// EvidenceRegistrationEnd covers the competition's registration-closing
	// date.
	EvidenceRegistrationEnd EvidenceField = "registration_end"
	// EvidenceCompetitionStart covers the competition's start date.
	EvidenceCompetitionStart EvidenceField = "competition_start"
	// EvidenceCompetitionEnd covers the competition's end date.
	EvidenceCompetitionEnd EvidenceField = "competition_end"
)

// ResearchReason describes why a canonical competition is missing evidence for
// a given field.
type ResearchReason string

const (
	// ResearchReasonMissing means the field has no value at all.
	ResearchReasonMissing ResearchReason = "missing"
)

// EvidenceGap records one field of a competition that lacks evidence and the
// deterministic reason it is considered a gap.
type EvidenceGap struct {
	Field  EvidenceField
	Reason ResearchReason
}

// ResearchSession is the unit of work for one canonical competition that needs
// further evidence. A single competition produces exactly one session even when
// several fields are missing.
type ResearchSession struct {
	CompetitionID int64
	Gaps          []EvidenceGap
}

// ResearchStateStatus is the terminal-for-now outcome of one evidence-research
// attempt on a single competition field.
type ResearchStateStatus string

const (
	// ResearchStateRetryable means the attempt hit a transient failure (network,
	// temporary service error) and should be retried after a short cooldown.
	ResearchStateRetryable ResearchStateStatus = "retryable"
	// ResearchStateUnresolved means a real attempt completed but found no
	// sufficiently reliable official evidence; retry after a long cooldown.
	ResearchStateUnresolved ResearchStateStatus = "unresolved"
	// ResearchStateResolved means the canonical successfully accepted the
	// evidence for this field; no further research is scheduled.
	ResearchStateResolved ResearchStateStatus = "resolved"
	// ResearchStateSkipped is a terminal state meaning this field should not be
	// researched again.
	ResearchStateSkipped ResearchStateStatus = "skipped"
)

// EvidenceResearchState is the scheduling history of research attempts for one
// competition field. It records only what has actually been attempted, never the
// existence of a gap: a missing row means the gap has simply never been
// researched (implicitly pending), with the canonical Competition as the source
// of truth for whether a gap exists.
type EvidenceResearchState struct {
	CompetitionID int64
	Field         EvidenceField

	Status       ResearchStateStatus
	AttemptCount int

	LastAttemptAt *time.Time
	NextRetryAt   *time.Time

	LastError string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ValidEvidenceField reports whether field is one of the four currently tracked
// date evidence fields.
func ValidEvidenceField(field EvidenceField) bool {
	switch field {
	case EvidenceRegistrationStart, EvidenceRegistrationEnd, EvidenceCompetitionStart, EvidenceCompetitionEnd:
		return true
	default:
		return false
	}
}

// ValidResearchStateStatus reports whether status is a recognized research
// state outcome.
func ValidResearchStateStatus(status ResearchStateStatus) bool {
	switch status {
	case ResearchStateRetryable, ResearchStateUnresolved, ResearchStateResolved, ResearchStateSkipped:
		return true
	default:
		return false
	}
}
