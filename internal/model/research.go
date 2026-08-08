package model

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
