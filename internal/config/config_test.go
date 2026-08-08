package config

import (
	"os"
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

// TestExampleConfigCCPCSourceUsesCCPCAPI guards against the CCPC source being
// reverted to a plain "page" kind pointing at the SPA shell (/placard), which
// yields an empty shell and never any real candidates. The example config must
// declare the ccpc_api adapter with a resolvable base URL.
func TestExampleConfigCCPCSourceUsesCCPCAPI(t *testing.T) {
	path := filepath.Join("..", "..", "sources.example.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range cfg.Sources {
		if source.ID != "ccpc" {
			continue
		}
		if source.Kind != "ccpc_api" {
			t.Fatalf("ccpc source kind = %q, want ccpc_api", source.Kind)
		}
		if source.URL == "" {
			t.Fatal("ccpc source must declare a url")
		}
		return
	}
	t.Fatal("example config is missing the ccpc source")
}

// TestCCPCAPISourceNeedsURLCovers the validate branch added for the ccpc_api
// kind: like page/rss, it must declare a url.
func TestCCPCAPISourceNeedsURL(t *testing.T) {
	cfg := Config{Sources: []Source{{ID: "ccpc", Name: "CCPC 公告", Kind: "ccpc_api"}}}
	if err := validate(&cfg); err == nil {
		t.Fatal("ccpc_api source without url must be rejected")
	}
}

// TestCCPCAPISourceValidates loads a real YAML document that uses the ccpc_api
// kind and asserts it is accepted end-to-end.
func TestCCPCAPISourceValidates(t *testing.T) {
	path := t.TempDir() + "/ccpc.yaml"
	content := `
schedule: "0 20 * * *"
sources:
  - id: ccpc
    name: CCPC 公告
    kind: ccpc_api
    url: https://ccpc.io/
    trust: high
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config with ccpc_api source must load: %v", err)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].Kind != "ccpc_api" {
		t.Fatalf("ccpc_api source was not loaded: %#v", cfg.Sources)
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
