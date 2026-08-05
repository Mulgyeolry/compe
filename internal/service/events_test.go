package service

import (
	"testing"
	"time"

	"competition-assistant/internal/model"
	"competition-assistant/internal/subscription"
)

func TestOnlyOfficialStartCreatesLifecycleEventAfterRegistrationCloses(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	closedAt := now.AddDate(0, 0, -3)

	upcoming := model.Competition{
		Status:          model.StatusUpcoming,
		RegistrationEnd: &closedAt,
	}
	if !actionable(upcoming, now) {
		t.Fatal("upcoming start was incorrectly treated as stale after registration closed")
	}
	events := changeEvents(model.Competition{}, upcoming, true, now)
	if len(events) != 1 || events[0].Type != "competition_upcoming" {
		t.Fatalf("upcoming lifecycle event was not created: %#v", events)
	}
	if subscription.EventDeliverable(upcoming, events[0].Type, model.ParticipationUndecided, now) {
		t.Fatal("upcoming status must not notify undecided users")
	}
	if !subscription.EventDeliverable(upcoming, events[0].Type, model.ParticipationParticipating, now) {
		t.Fatal("participating user did not receive upcoming status")
	}

	started := upcoming
	started.Status = model.StatusOngoing
	events = changeEvents(upcoming, started, false, now)
	if len(events) != 1 || events[0].Type != "competition_started" {
		t.Fatalf("unexpected start events: %#v", events)
	}
}

func TestEventsAreReducedToCanonicalCurrentState(t *testing.T) {
	competition := model.Competition{Status: model.StatusRegistrationOpen}
	events := eventsForCurrentState(competition, []model.Event{
		{Type: "preview_detected", Key: "preview"},
		{Type: "registration_opened", Key: "registration_open"},
	})
	if len(events) != 1 || events[0].Type != "registration_opened" {
		t.Fatalf("stale same-scan event was retained: %#v", events)
	}
}

func TestExpiredRegistrationIsNotActionable(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	closedAt := now.AddDate(0, 0, -1)
	competition := model.Competition{Status: model.StatusRegistrationOpen, RegistrationEnd: &closedAt}
	if actionable(competition, now) {
		t.Fatal("expired registration was still actionable")
	}
}

func TestCurrentSpecificAnnouncementCanNotifyWithUnknownRegistration(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	competition := model.Competition{
		Name:        "2026年全国大学生云原生开发大赛通知",
		Content:     "面向全国高校的软件开发与云原生赛事",
		OfficialURL: "https://contest.example.org/2026/notice",
		Trust:       model.TrustMedium,
	}
	events := changeEvents(model.Competition{}, competition, true, now)
	if len(events) != 1 || events[0].Type != "competition_discovered" {
		t.Fatalf("unknown-state current announcement did not create discovery event: %#v", events)
	}
	if !subscription.EventDeliverable(competition, events[0].Type, model.ParticipationUndecided, now) {
		t.Fatal("discovery event was not deliverable")
	}
	filtered := eventsForCurrentState(competition, events)
	if len(filtered) != 1 || filtered[0].Type != "competition_discovered" {
		t.Fatalf("discovery event was removed from current state: %#v", filtered)
	}
}

func TestDiscoveryDoesNotNotifyListingOrPastEdition(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	for _, competition := range []model.Competition{
		{Name: "2025年全国大学生软件开发大赛", OfficialURL: "https://example.org/2025", Trust: model.TrustHigh},
		{Name: "2026年竞赛公告列表", OfficialURL: "https://example.org/list", Trust: model.TrustHigh},
		{Name: "2026年高校程序设计校赛", OfficialURL: "https://school.example.edu.cn/notice", Trust: model.TrustMedium},
	} {
		if events := changeEvents(model.Competition{}, competition, true, now); len(events) != 0 {
			t.Errorf("ineligible discovery %q created events: %#v", competition.Name, events)
		}
	}
}

func TestExternalEmailLinkBaseValidation(t *testing.T) {
	invalid := []string{"", "localhost:8080", "http://localhost:8080", "http://127.0.0.1:8080", "http://192.168.1.8:8080", "http://10.0.0.2"}
	for _, value := range invalid {
		if supportsExternalEmailLinks(value) {
			t.Errorf("local/private URL accepted: %q", value)
		}
	}
	valid := []string{"https://competitions.example.com", "http://162.14.96.144:8080"}
	for _, value := range valid {
		if !supportsExternalEmailLinks(value) {
			t.Errorf("public URL rejected: %q", value)
		}
	}
}
