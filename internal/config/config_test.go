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
