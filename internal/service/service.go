package service

import (
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/authn"
	"competition-assistant/internal/config"
	"competition-assistant/internal/fetcher"
	"competition-assistant/internal/notifier"
	"competition-assistant/internal/store"
)

// Service orchestrates the competition pipeline: crawling announcements,
// analysing their lifecycle, persisting state and delivering per-user
// notifications. It is the single entry point the web layer and CLI use, so
// every concern lives behind a narrow, stable API.
//
// The type intentionally stays in one file while its methods are grouped by
// responsibility across sibling files:
//   - service_scan.go     crawling, analysis orchestration and event derivation
//   - service_delivery.go notification scheduling and sending
//   - service_user.go     per-user backfill and participation choices
type Service struct {
	cfg         config.Config
	store       *store.Store
	collector   fetcher.Collector
	analyzer    *analyzer.Analyzer
	notifier    notifier.Sender
	now         func() time.Time
	log         *slog.Logger
	auth        *authn.Manager
	publicURL   string
	operationMu sync.Mutex

	// evidenceResearchPhaseTimeoutOverride allows tests to shrink the whole-phase
	// research timeout so the started-session persistence semantics can be
	// exercised without a real 120s wait. When zero it falls back to
	// evidenceResearchPhaseTimeout.
	evidenceResearchPhaseTimeoutOverride time.Duration
}

// SetEvidenceResearchPhaseTimeout overrides the whole-phase research timeout,
// primarily for tests of started-session persistence under a phase deadline.
func (s *Service) SetEvidenceResearchPhaseTimeout(d time.Duration) {
	s.evidenceResearchPhaseTimeoutOverride = d
}

// ErrCompetitionExpired guards writes that would act on a competition whose
// edition has already ended (e.g. opting in after the final event).
var ErrCompetitionExpired = errors.New("competition has already ended")

func New(cfg config.Config, database *store.Store, collector fetcher.Collector, analysis *analyzer.Analyzer, sender notifier.Sender, logger *slog.Logger) *Service {
	return &Service{cfg: cfg, store: database, collector: collector, analyzer: analysis, notifier: sender, now: time.Now, log: logger}
}

// SetNow overrides the clock used by the service, primarily for tests.
func (s *Service) SetNow(now func() time.Time) { s.now = now }

// EnableMultiUser activates per-user delivery and configures the public URL
// used to build email action links. It must be called before the service is
// used by the web layer.
func (s *Service) EnableMultiUser(publicURL string, manager *authn.Manager) {
	s.publicURL = strings.TrimRight(publicURL, "/")
	s.auth = manager
	if !supportsExternalEmailLinks(s.publicURL) {
		s.log.Warn("email action links disabled for local or private public URL", "public_url", s.publicURL)
	}
}

// freshnessWindow returns how long after first observation a competition
// without a year in its title is still treated as a current announcement.
func (s *Service) freshnessWindow() time.Duration {
	return time.Duration(s.cfg.Discovery.AnnouncementFreshnessDays) * 24 * time.Hour
}
