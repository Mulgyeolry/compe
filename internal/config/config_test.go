package config

import (
	"path/filepath"
	"testing"
)

func TestExampleConfigurationLoads(t *testing.T) {
	path := filepath.Join("..", "..", "sources.example.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) < 10 || cfg.Location == nil || cfg.Schedule != "0 20 * * *" {
		t.Fatalf("example config is incomplete: sources=%d schedule=%q", len(cfg.Sources), cfg.Schedule)
	}
	if !cfg.Retention.Enabled || cfg.Retention.ObservationDays != 30 || cfg.Retention.ClosedCompetitionContentDays != 180 {
		t.Fatalf("example retention policy is incomplete: %#v", cfg.Retention)
	}
}

func TestRetentionCanBeExplicitlyDisabled(t *testing.T) {
	disabled := false
	cfg := Config{Retention: Retention{ConfiguredEnabled: &disabled}}
	applyDefaults(&cfg)
	if cfg.Retention.Enabled {
		t.Fatal("explicit retention.enabled=false was ignored")
	}
}

func TestFetchRetriesAndAlertDefaults(t *testing.T) {
	cfg := Config{}
	applyDefaults(&cfg)
	if cfg.Fetch.MaxRetries != 2 {
		t.Fatalf("default fetch.max_retries = %d, want 2", cfg.Fetch.MaxRetries)
	}
	if cfg.Alert.ConsecutiveFailureLimit != 3 {
		t.Fatalf("default alert.consecutive_failure_limit = %d, want 3", cfg.Alert.ConsecutiveFailureLimit)
	}
	if cfg.Discovery.AnnouncementFreshnessDays != 90 {
		t.Fatalf("default discovery.announcement_freshness_days = %d, want 90", cfg.Discovery.AnnouncementFreshnessDays)
	}
}

func TestInvalidFetchRetriesIsRejected(t *testing.T) {
	cfg := Config{Fetch: Fetch{MaxRetries: 11}, Sources: []Source{{ID: "s", Name: "s", Kind: "page", URL: "https://example.com"}}}
	if err := validate(&cfg); err == nil {
		t.Fatal("fetch.max_retries=11 must be rejected")
	}
}

func TestInvalidAlertThresholdIsRejected(t *testing.T) {
	cfg := Config{Alert: Alert{ConsecutiveFailureLimit: 0}, Sources: []Source{{ID: "s", Name: "s", Kind: "page", URL: "https://example.com"}}}
	if err := validate(&cfg); err == nil {
		t.Fatal("alert.consecutive_failure_limit=0 must be rejected")
	}
}

func TestWebConfigurationRequiresSecretsAndAbsoluteURL(t *testing.T) {
	path := filepath.Join("..", "..", "sources.example.yaml")
	t.Setenv("WEB_LISTEN_ADDR", ":8080")
	t.Setenv("PUBLIC_BASE_URL", "http://competitions.example.com")
	t.Setenv("APP_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("APPRISE_SENDER_URL", "mailtos://sender:code@smtp.example.com:465/?from=sender%40example.com")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Web.Enabled || cfg.Web.ListenAddr != ":8080" || cfg.Web.VerificationTTL == 0 {
		t.Fatalf("web configuration was not loaded: %#v", cfg.Web)
	}

	t.Setenv("APP_SECRET", "short")
	if _, err := Load(path); err == nil {
		t.Fatal("short APP_SECRET was accepted")
	}
}
