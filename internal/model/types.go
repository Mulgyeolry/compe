package model

import "time"

type Trust string

const (
	TrustHigh   Trust = "high"
	TrustMedium Trust = "medium"
	TrustLow    Trust = "low"
)

type Status string

const (
	StatusUnknown            Status = "unknown"
	StatusPreview            Status = "preview"
	StatusUpcoming           Status = "upcoming"
	StatusRegistrationOpen   Status = "registration_open"
	StatusRegistrationClosed Status = "registration_closed"
	StatusOngoing            Status = "ongoing"
	StatusFinished           Status = "finished"
)

type RegistrationPhase string

const (
	RegistrationUnknown RegistrationPhase = "unknown"
	RegistrationPreview RegistrationPhase = "preview"
	RegistrationOpen    RegistrationPhase = "open"
	RegistrationClosed  RegistrationPhase = "closed"
)

type CompetitionPhase string

const (
	CompetitionUnknown  CompetitionPhase = "unknown"
	CompetitionUpcoming CompetitionPhase = "upcoming"
	CompetitionOngoing  CompetitionPhase = "ongoing"
	CompetitionFinished CompetitionPhase = "finished"
)

type ParticipationDecision string

const (
	ParticipationUndecided     ParticipationDecision = "undecided"
	ParticipationParticipating ParticipationDecision = "participating"
	ParticipationDeclined      ParticipationDecision = "declined"
)

type Candidate struct {
	SourceID   string
	SourceName string
	Title      string
	URL        string
	Snippet    string
}

type Document struct {
	Title          string
	URL            string
	Text           string
	RawText        string
	PublishedAtRaw string
	IsListing      bool
	ContentType    string
	Segments       []DocumentSegment
}

type DocumentSegment struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Page int    `json:"page,omitempty"`
	Text string `json:"text"`
}

type AnalysisRejection struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
	Value  string `json:"value,omitempty"`
}

type AnalysisAudit struct {
	AnalyzerVersion string              `json:"analyzer_version"`
	Model           string              `json:"model,omitempty"`
	InputHash       string              `json:"input_hash"`
	SegmentIDs      []string            `json:"segment_ids,omitempty"`
	RawResponses    []string            `json:"raw_responses,omitempty"`
	AcceptedFields  []string            `json:"accepted_fields,omitempty"`
	Rejections      []AnalysisRejection `json:"rejections,omitempty"`
	Error           string              `json:"error,omitempty"`
	AnalyzedAt      time.Time           `json:"analyzed_at"`
}

type FactEvidence struct {
	Value      string    `json:"value"`
	Raw        string    `json:"raw"`
	Evidence   string    `json:"evidence"`
	Edition    string    `json:"edition"`
	SourceURL  string    `json:"source_url"`
	Confidence string    `json:"confidence"`
	ObservedAt time.Time `json:"observed_at"`
}

const (
	FactOrganizer         = "organizer"
	FactRegistrationState = "registration_state"
	FactCompetitionState  = "competition_state"
	FactRegistrationStart = "registration_start"
	FactRegistrationEnd   = "registration_end"
	FactCompetitionStart  = "competition_start"
	FactCompetitionEnd    = "competition_end"
	FactTeamRequirement   = "team_requirement"
	FactFee               = "fee"
	FactEligibility       = "eligibility"
	FactContent           = "competition_contents"
	FactPublishedAt       = "published_at"
)

// ResearchSource is secondary context used only for qualitative analysis.
// It must never overwrite official registration facts.
type ResearchSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text"`
	Kind  string `json:"kind"`
}

type AnalysisReference struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Evidence string `json:"evidence"`
}

type CompetitionAnalysis struct {
	Summary      string              `json:"summary"`
	SuitableFor  string              `json:"suitable_for"`
	Skills       []string            `json:"skills"`
	Difficulty   string              `json:"difficulty"`
	ResumeValue  string              `json:"resume_value"`
	Caveats      string              `json:"caveats"`
	Confidence   string              `json:"confidence"`
	References   []AnalysisReference `json:"references"`
	AnalyzedAt   time.Time           `json:"analyzed_at"`
	AnalysisHash string              `json:"analysis_hash"`
}

type Competition struct {
	ID                   int64
	EntityKey            string
	Name                 string
	Organizer            string
	Status               Status
	StatusEvidence       string
	RegistrationPhase    RegistrationPhase
	CompetitionPhase     CompetitionPhase
	RegistrationStart    *time.Time
	RegistrationStartRaw string
	RegistrationEnd      *time.Time
	RegistrationEndRaw   string
	CompetitionStart     *time.Time
	CompetitionStartRaw  string
	CompetitionEnd       *time.Time
	CompetitionEndRaw    string
	TeamRequirement      string
	Fee                  string
	FeeEvidence          string
	Keywords             []string
	Analysis             CompetitionAnalysis
	Content              string
	FitScore             int
	FitReason            string
	EligibilityNote      string
	OfficialURL          string
	Trust                Trust
	ProblemReleased      bool
	Facts                map[string]FactEvidence
	AnalyzerVersion      string
	ExtractionAudit      AnalysisAudit
	ContentHash          string
	FirstSeen            time.Time
	LastSeen             time.Time
}

type Event struct {
	Type string
	Key  string
}

type DeliveryFrequency string

const (
	DeliveryImmediate DeliveryFrequency = "immediate"
	DeliveryDaily     DeliveryFrequency = "daily"
	DeliveryWeekly    DeliveryFrequency = "weekly"
)

type User struct {
	ID         int64
	Email      string
	VerifiedAt time.Time
	Enabled    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type UserPreferences struct {
	UserID                int64
	Frequency             DeliveryFrequency
	DeliveryTime          string
	WeeklyDay             time.Weekday
	Timezone              string
	MinTrust              Trust
	AllowEligibilityRisk  bool
	NotifyPreview         bool
	NotifyRegistration    bool
	NotifyUpcoming        bool
	NotifyStarted         bool
	NotifyProblemRelease  bool
	NotifyDeadline7Days   bool
	NotifyDeadline1Day    bool
	NotifyImportantUpdate bool
	Categories            []string
	OrganizerTypes        []string
	CompetitionScopes     []string
	Regions               []string
	IncludeKeywords       []string
	ExcludeKeywords       []string
}

type NotificationUser struct {
	User        User
	Preferences UserPreferences
}

type UserNotificationItem struct {
	NotificationID int64
	Competition    Competition
	Event          Event
	Decision       ParticipationDecision
}

type UserCompetitionDecision struct {
	UserID        int64
	CompetitionID int64
	Decision      ParticipationDecision
	UpdatedAt     time.Time
}

type UserDeliveryGroup struct {
	GroupKey string
	User     User
	Items    []UserNotificationItem
}

type UserNotificationHistory struct {
	ID              int64
	CompetitionName string
	EventType       string
	Status          string
	DueAt           time.Time
	SentAt          *time.Time
	LastError       string
}
