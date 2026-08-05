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
	if !actionable(upcoming, now, 90*24*time.Hour) {
		t.Fatal("upcoming start was incorrectly treated as stale after registration closed")
	}
	events := changeEvents(model.Competition{}, upcoming, true, now, 90*24*time.Hour)
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
	events = changeEvents(upcoming, started, false, now, 90*24*time.Hour)
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
	if actionable(competition, now, 90*24*time.Hour) {
		t.Fatal("expired registration was still actionable")
	}
}

// TestYearlessOpenRegistrationFromPastIsNotActionable guards against an old
// "开始报名" page (no deadline, no year) being pushed as a current edition when
// the system first saw it long ago.
func TestYearlessOpenRegistrationFromPastIsNotActionable(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	competition := model.Competition{
		Name:        "开发者大赛",
		Status:      model.StatusRegistrationOpen,
		FirstSeen:   now.AddDate(0, 0, -400),
		OfficialURL: "https://example.com/contest",
	}
	if actionable(competition, now, 90*24*time.Hour) {
		t.Fatal("yearless open registration first seen long ago must not be actionable")
	}
}

// TestYearlessOpenRegistrationSeenRecentlyStaysActionable ensures the freshness
// guard does not suppress a genuinely current yearless competition.
func TestYearlessOpenRegistrationSeenRecentlyStaysActionable(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	competition := model.Competition{
		Name:        "开发者大赛",
		Status:      model.StatusRegistrationOpen,
		FirstSeen:   now.AddDate(0, 0, -2),
		OfficialURL: "https://example.com/contest",
	}
	if !actionable(competition, now, 90*24*time.Hour) {
		t.Fatal("yearless open registration seen recently should be actionable")
	}
}

// TestYearlessOpenRegistrationWithPastYearIsNotActionable verifies an explicit
// past year in the status evidence is rejected regardless of freshness.
func TestYearlessOpenRegistrationWithPastYearIsNotActionable(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	competition := model.Competition{
		Name:        "2023年开发者大赛",
		Status:      model.StatusRegistrationOpen,
		FirstSeen:   now.AddDate(0, 0, -2),
		OfficialURL: "https://example.com/contest",
	}
	if actionable(competition, now, 90*24*time.Hour) {
		t.Fatal("open registration with explicit past year must not be actionable")
	}
}

// TestDiscoverableSuppressesExplicitPastYearEvenIfFreshlyCrawled is the
// regression guard for the archived-page bug: a page that names an explicit
// past year (e.g. 2023 CCSP) must not be announced even when the system first
// crawled it today, because FirstSeen alone cannot tell a new announcement
// from an old page that is only now being crawled.
func TestDiscoverableSuppressesExplicitPastYearEvenIfFreshlyCrawled(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	for _, name := range []string{
		"2025 CCF CCSP竞赛 10.22举办",
		"2024 CCF CCSP竞赛",
		"2023 CCF CCSP竞赛",
		"2022 CCF CCSP报名通知",
	} {
		competition := model.Competition{
			Name:        name,
			Content:     "CCF CCSP 竞赛通知",
			OfficialURL: "https://www.ccf.org.cn/ccsp/tzgg/",
			Trust:       model.TrustHigh,
			FirstSeen:   now.AddDate(0, 0, 0), // just crawled today
		}
		if discoverableAnnouncement(competition, now, 90*24*time.Hour) {
			t.Errorf("explicit past-year announcement %q must not be discoverable", name)
		}
	}
}

// TestDiscoverableAcceptsCurrentOrNextYear verifies the explicit current or
// next year edition is still announced.
func TestDiscoverableAcceptsCurrentOrNextYear(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	competition := model.Competition{
		Name:        "2026 CCF CCSP竞赛 报名通知",
		Content:     "2026 CCSP 竞赛报名",
		OfficialURL: "https://www.ccf.org.cn/ccsp/tzgg/",
		Trust:       model.TrustHigh,
	}
	if !discoverableAnnouncement(competition, now, 90*24*time.Hour) {
		t.Fatal("current-year (2026) announcement should be discoverable")
	}
}

// TestIsCurrentEditionUsesPublishDateForYearlessCompetition verifies the AI
// intervention: a year-less competition whose page was published long ago is
// treated as archived even though the system first crawled it today. This is
// the regression guard for the "第N次CSP认证" case.
func TestIsCurrentEditionUsesPublishDateForYearlessCompetition(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	oldPage := model.Competition{
		Name:        "第31次CCF CSP认证",
		OfficialURL: "https://www.ccf.org.cn/ccsp/tzgg/",
		Trust:       model.TrustHigh,
		FirstSeen:   now.AddDate(0, 0, 0), // crawled today
		Facts: map[string]model.FactEvidence{
			model.FactPublishedAt: {Value: "2022-08-18", Raw: "2022-08-18", Evidence: "2022-08-18"},
		},
	}
	if discoverableAnnouncement(oldPage, now, 90*24*time.Hour) {
		t.Fatal("yearless competition published long ago must not be discoverable")
	}

	freshPage := model.Competition{
		Name:        "CCSP竞赛报名",
		OfficialURL: "https://www.ccf.org.cn/ccsp/tzgg/",
		Trust:       model.TrustHigh,
		Facts: map[string]model.FactEvidence{
			model.FactPublishedAt: {Value: "2026-07-20", Raw: "2026-07-20", Evidence: "2026-07-20"},
		},
	}
	if !discoverableAnnouncement(freshPage, now, 90*24*time.Hour) {
		t.Fatal("yearless competition published recently should be discoverable")
	}
}

// TestIsCurrentEditionFallsBackToFirstSeenWithoutPublishDate ensures the
// publish-date signal is an enhancement, not a requirement: a yearless
// competition with no publish date and a recent FirstSeen still passes.
func TestIsCurrentEditionFallsBackToFirstSeenWithoutPublishDate(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	competition := model.Competition{
		Name:        "开发者大赛",
		OfficialURL: "https://example.com/contest",
		Trust:       model.TrustHigh,
		FirstSeen:   now.AddDate(0, 0, -2),
	}
	if !isCurrentEdition(competition, now, 90*24*time.Hour) {
		t.Fatal("yearless competition without publish date but recent FirstSeen should be current")
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
	events := changeEvents(model.Competition{}, competition, true, now, 90*24*time.Hour)
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
		if events := changeEvents(model.Competition{}, competition, true, now, 90*24*time.Hour); len(events) != 0 {
			t.Errorf("ineligible discovery %q created events: %#v", competition.Name, events)
		}
	}
}

func TestDiscoveryNotifiesYearlessCompetitionSeenRecently(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	// Title has no year, but the page was first seen within the window, so it
	// is a current announcement rather than an archived previous-year page.
	competition := model.Competition{
		Name:        "腾讯云黑客松官网",
		Content:     "面向开发者开放的云端黑客松赛事",
		OfficialURL: "https://tch.cloud.tencent.com",
		Trust:       model.TrustHigh,
		FirstSeen:   now.AddDate(0, 0, -2),
	}
	events := changeEvents(model.Competition{}, competition, true, now, 90*24*time.Hour)
	if len(events) != 1 || events[0].Type != "competition_discovered" {
		t.Fatalf("recently seen yearless competition did not create discovery event: %#v", events)
	}
}

func TestDiscoverySuppressesYearlessCompetitionSeenLongAgo(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	// Title has no year and the page was first seen well outside the window,
	// so it is treated as an archived previous-year page and suppressed.
	competition := model.Competition{
		Name:        "开发者大赛",
		Content:     "面向开发者的年度大赛",
		OfficialURL: "https://developer.example.com/contest",
		Trust:       model.TrustHigh,
		FirstSeen:   now.AddDate(0, 0, -400),
	}
	if events := changeEvents(model.Competition{}, competition, true, now, 90*24*time.Hour); len(events) != 0 {
		t.Fatalf("yearless competition seen long ago created events: %#v", events)
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
