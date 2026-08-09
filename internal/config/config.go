package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Source struct {
	ID             string   `yaml:"id"`
	Name           string   `yaml:"name"`
	Kind           string   `yaml:"kind"`
	URL            string   `yaml:"url"`
	Query          string   `yaml:"query"`
	Limit          int      `yaml:"limit"`
	Trust          string   `yaml:"trust"`
	AllowedDomains []string `yaml:"allowed_domains"`
}

type Fetch struct {
	TimeoutSeconds int   `yaml:"timeout_seconds"`
	MaxBytes       int64 `yaml:"max_bytes"`
	MaxCandidates  int   `yaml:"max_candidates_per_source"`
	// MaxRetries is the number of additional attempts made for transient
	// network errors or 429/5xx responses. Retries use exponential backoff.
	MaxRetries int `yaml:"max_retries"`
}

// Discovery controls how newly seen competitions are judged fresh enough to
// notify about when their title carries no explicit year. A competition whose
// title lacks a year is still notified if the page was first seen or published
// within AnnouncementFreshnessDays, which distinguishes a current edition from
// an archived page from a previous year.
type Discovery struct {
	AnnouncementFreshnessDays int `yaml:"announcement_freshness_days"`
}

// EvidenceResearch controls how evidence-gap research is scheduled. A
// competition with missing dates is only admitted to research when its gap is
// "due": never-researched gaps are due now, retryable/unresolved gaps become due
// again after their cooldown, and a budget bounds how many competitions enter a
// single run.
type EvidenceResearch struct {
	// Enabled gates whether Evidence Research actually runs. It defaults to
	// false so a configured LLM never silently starts agent research; when
	// disabled the pipeline performs zero Search/Fetch/Extractor/ResearchState
	// work and ordinary source scanning is unchanged.
	Enabled bool `yaml:"enabled"`
	// MaxCompetitionsPerRun caps how many competition sessions are admitted to
	// a single planning pass.
	MaxCompetitionsPerRun int `yaml:"max_competitions_per_run"`
	// RetryCooldownHours is the short cooldown after a transient (retryable)
	// research failure.
	RetryCooldownHours int `yaml:"retry_cooldown_hours"`
	// UnresolvedCooldownHours is the long cooldown after a real but unsuccessful
	// (unresolved) research attempt.
	UnresolvedCooldownHours int `yaml:"unresolved_cooldown_hours"`
}

// Alert controls operator notifications when a data source becomes unhealthy.
// A source is considered failed when discovery or candidate fetch errors, and
// the operator is notified only after the source has failed for
// ConsecutiveFailureLimit consecutive scan cycles so a single flake does not
// page anyone.
type Alert struct {
	Enabled                 bool `yaml:"enabled"`
	ConsecutiveFailureLimit int  `yaml:"consecutive_failure_limit"`
}

type Keywords struct {
	Positive []string `yaml:"positive"`
	Negative []string `yaml:"negative"`
	Focus    []string `yaml:"focus"`
}

type Enrichment struct {
	Enabled        bool     `yaml:"enabled"`
	MaxSources     int      `yaml:"max_sources"`
	AllowedDomains []string `yaml:"allowed_domains"`
}

// Retention controls conservative database maintenance. Cleanup removes or
// compacts only data that can be regenerated, while event and notification
// keys are retained so an old competition is never sent twice.
type Retention struct {
	Enabled                      bool  `yaml:"-"`
	ConfiguredEnabled            *bool `yaml:"enabled"`
	ObservationDays              int   `yaml:"observation_days"`
	ClosedCompetitionContentDays int   `yaml:"closed_competition_content_days"`
	ExpiredAuthenticationDays    int   `yaml:"expired_authentication_days"`
}

type Web struct {
	Enabled          bool
	ListenAddr       string
	PublicBaseURL    string
	AppSecret        string
	AppriseSenderURL string
	VerificationTTL  time.Duration
	SessionTTL       time.Duration
}

type Config struct {
	Schedule      string         `yaml:"schedule"`
	Timezone      string         `yaml:"timezone"`
	DBPath        string         `yaml:"db_path"`
	SearxngURL    string         `yaml:"searxng_url"`
	AppriseURL    string         `yaml:"apprise_url"`
	HighDomains   []string       `yaml:"high_trust_domains"`
	MediumDomains []string       `yaml:"medium_trust_domains"`
	Fetch         Fetch            `yaml:"fetch"`
	Alert         Alert            `yaml:"alert"`
	Discovery     Discovery        `yaml:"discovery"`
	EvidenceResearch EvidenceResearch `yaml:"evidence_research"`
	Keywords      Keywords         `yaml:"keywords"`
	Enrichment    Enrichment     `yaml:"enrichment"`
	Retention     Retention      `yaml:"retention"`
	Sources       []Source       `yaml:"sources"`
	Location      *time.Location `yaml:"-"`
	Web           Web            `yaml:"-"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&cfg)
	applyEnvironment(&cfg)
	if err := validate(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Schedule == "" {
		cfg.Schedule = "0 20 * * *"
	}
	if cfg.Timezone == "" {
		cfg.Timezone = "Asia/Shanghai"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "data/competitions.db"
	}
	if cfg.SearxngURL == "" {
		cfg.SearxngURL = "http://searxng:8080"
	}
	if cfg.AppriseURL == "" {
		cfg.AppriseURL = "http://apprise:8000/notify/"
	}
	if cfg.Fetch.TimeoutSeconds == 0 {
		cfg.Fetch.TimeoutSeconds = 20
	}
	if cfg.Fetch.MaxBytes == 0 {
		cfg.Fetch.MaxBytes = 5 * 1024 * 1024
	}
	if cfg.Fetch.MaxCandidates == 0 {
		cfg.Fetch.MaxCandidates = 40
	}
	if cfg.Fetch.MaxRetries == 0 {
		cfg.Fetch.MaxRetries = 2
	}
	if cfg.Alert.ConsecutiveFailureLimit == 0 {
		cfg.Alert.ConsecutiveFailureLimit = 3
	}
	if cfg.Discovery.AnnouncementFreshnessDays == 0 {
		cfg.Discovery.AnnouncementFreshnessDays = 90
	}
	if cfg.EvidenceResearch.MaxCompetitionsPerRun == 0 {
		cfg.EvidenceResearch.MaxCompetitionsPerRun = 5
	}
	if cfg.EvidenceResearch.RetryCooldownHours == 0 {
		cfg.EvidenceResearch.RetryCooldownHours = 6
	}
	if cfg.EvidenceResearch.UnresolvedCooldownHours == 0 {
		cfg.EvidenceResearch.UnresolvedCooldownHours = 72
	}
	if cfg.Enrichment.MaxSources == 0 {
		cfg.Enrichment.MaxSources = 5
	}
	// Cleanup is enabled by default, but an explicit YAML boolean still wins.
	cfg.Retention.Enabled = true
	if cfg.Retention.ConfiguredEnabled != nil {
		cfg.Retention.Enabled = *cfg.Retention.ConfiguredEnabled
	}
	if cfg.Retention.ObservationDays == 0 {
		cfg.Retention.ObservationDays = 30
	}
	if cfg.Retention.ClosedCompetitionContentDays == 0 {
		cfg.Retention.ClosedCompetitionContentDays = 180
	}
	if cfg.Retention.ExpiredAuthenticationDays == 0 {
		cfg.Retention.ExpiredAuthenticationDays = 7
	}
	cfg.Web.VerificationTTL = 10 * time.Minute
	cfg.Web.SessionTTL = 30 * 24 * time.Hour
	for i := range cfg.Sources {
		if cfg.Sources[i].Limit == 0 {
			cfg.Sources[i].Limit = cfg.Fetch.MaxCandidates
		}
	}
}

func applyEnvironment(cfg *Config) {
	if value := os.Getenv("APP_TIMEZONE"); value != "" {
		cfg.Timezone = value
	}
	if value := os.Getenv("APP_SCHEDULE"); value != "" {
		cfg.Schedule = value
	}
	if value := os.Getenv("DB_PATH"); value != "" {
		cfg.DBPath = value
	}
	if value := os.Getenv("SEARXNG_URL"); value != "" {
		cfg.SearxngURL = strings.TrimRight(value, "/")
	}
	if value := os.Getenv("APPRISE_API_URL"); value != "" {
		cfg.AppriseURL = value
	}
	if value := os.Getenv("FETCH_TIMEOUT_SECONDS"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			cfg.Fetch.TimeoutSeconds = parsed
		}
	}
	if value := strings.TrimSpace(os.Getenv("CLEANUP_ENABLED")); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			cfg.Retention.Enabled = parsed
		}
	}
	applyPositiveIntEnv("OBSERVATION_RETENTION_DAYS", &cfg.Retention.ObservationDays)
	applyPositiveIntEnv("CLOSED_COMPETITION_CONTENT_RETENTION_DAYS", &cfg.Retention.ClosedCompetitionContentDays)
	applyPositiveIntEnv("EXPIRED_AUTH_RETENTION_DAYS", &cfg.Retention.ExpiredAuthenticationDays)
	if value := strings.TrimSpace(os.Getenv("WEB_LISTEN_ADDR")); value != "" {
		cfg.Web.Enabled = true
		cfg.Web.ListenAddr = value
	}
	cfg.Web.PublicBaseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")), "/")
	cfg.Web.AppSecret = os.Getenv("APP_SECRET")
	cfg.Web.AppriseSenderURL = strings.TrimSpace(os.Getenv("APPRISE_SENDER_URL"))
}

func applyPositiveIntEnv(name string, target *int) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			*target = parsed
		}
	}
}

func validate(cfg *Config) error {
	if len(cfg.Sources) == 0 {
		return errors.New("config must contain at least one source")
	}
	if cfg.Enrichment.Enabled {
		if cfg.Enrichment.MaxSources < 1 || cfg.Enrichment.MaxSources > 10 {
			return errors.New("enrichment.max_sources must be between 1 and 10")
		}
		if len(cfg.Enrichment.AllowedDomains) == 0 {
			return errors.New("enrichment.allowed_domains must not be empty when enrichment is enabled")
		}
	}
	if cfg.Fetch.MaxRetries < 0 || cfg.Fetch.MaxRetries > 10 {
		return errors.New("fetch.max_retries must be between 0 and 10")
	}
	if cfg.Alert.ConsecutiveFailureLimit < 1 || cfg.Alert.ConsecutiveFailureLimit > 30 {
		return errors.New("alert.consecutive_failure_limit must be between 1 and 30")
	}
	if cfg.Discovery.AnnouncementFreshnessDays < 1 || cfg.Discovery.AnnouncementFreshnessDays > 3650 {
		return errors.New("discovery.announcement_freshness_days must be between 1 and 3650")
	}
	if cfg.EvidenceResearch.MaxCompetitionsPerRun < 1 || cfg.EvidenceResearch.MaxCompetitionsPerRun > 100 {
		return errors.New("evidence_research.max_competitions_per_run must be between 1 and 100")
	}
	if cfg.EvidenceResearch.RetryCooldownHours < 1 || cfg.EvidenceResearch.RetryCooldownHours > 168 {
		return errors.New("evidence_research.retry_cooldown_hours must be between 1 and 168")
	}
	if cfg.EvidenceResearch.UnresolvedCooldownHours < 1 || cfg.EvidenceResearch.UnresolvedCooldownHours > 720 {
		return errors.New("evidence_research.unresolved_cooldown_hours must be between 1 and 720")
	}
	if cfg.Retention.Enabled {
		if cfg.Retention.ObservationDays < 7 || cfg.Retention.ObservationDays > 3650 {
			return errors.New("retention.observation_days must be between 7 and 3650")
		}
		if cfg.Retention.ClosedCompetitionContentDays < 30 || cfg.Retention.ClosedCompetitionContentDays > 3650 {
			return errors.New("retention.closed_competition_content_days must be between 30 and 3650")
		}
		if cfg.Retention.ExpiredAuthenticationDays < 1 || cfg.Retention.ExpiredAuthenticationDays > 365 {
			return errors.New("retention.expired_authentication_days must be between 1 and 365")
		}
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("invalid timezone %q: %w", cfg.Timezone, err)
	}
	cfg.Location = loc
	if cfg.Web.Enabled {
		if len(cfg.Web.AppSecret) < 32 {
			return errors.New("APP_SECRET must contain at least 32 characters when the web interface is enabled")
		}
		parsed, err := url.Parse(cfg.Web.PublicBaseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return errors.New("PUBLIC_BASE_URL must be an absolute http or https URL when the web interface is enabled")
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("PUBLIC_BASE_URL must not contain a query string or fragment")
		}
		if cfg.Web.AppriseSenderURL == "" {
			return errors.New("APPRISE_SENDER_URL is required when the web interface is enabled")
		}
		senderURL, err := url.Parse(cfg.Web.AppriseSenderURL)
		if err != nil || (senderURL.Scheme != "mailto" && senderURL.Scheme != "mailtos") || senderURL.Host == "" {
			return errors.New("APPRISE_SENDER_URL must be a valid mailto or mailtos Apprise URL")
		}
	}
	seen := map[string]bool{}
	for _, source := range cfg.Sources {
		if source.ID == "" || source.Name == "" {
			return errors.New("every source needs id and name")
		}
		if seen[source.ID] {
			return fmt.Errorf("duplicate source id %q", source.ID)
		}
		seen[source.ID] = true
		switch source.Kind {
		case "page", "rss", "ccpc_api":
			if source.URL == "" {
				return fmt.Errorf("source %q needs url", source.ID)
			}
		case "search":
			if source.Query == "" {
				return fmt.Errorf("source %q needs query", source.ID)
			}
		default:
			return fmt.Errorf("source %q has unsupported kind %q", source.ID, source.Kind)
		}
	}
	return nil
}
