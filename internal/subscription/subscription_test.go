package subscription

import (
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestClassifyAndMatch(t *testing.T) {
	competition := model.Competition{
		Name:      "高校 AI Agent 与 RAG 应用挑战赛",
		Content:   "使用 Go 后端开发智能体应用",
		Trust:     model.TrustMedium,
		FitReason: "适合大模型应用实践",
		Status:    model.StatusRegistrationOpen,
	}
	classification := Profile(competition)
	preferences := model.UserPreferences{
		Categories:         []string{"ai_data"},
		MinTrust:           model.TrustMedium,
		NotifyRegistration: true,
		ExcludeKeywords:    []string{"摄影"},
	}
	now := time.Date(2026, 8, 4, 8, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	events := MatchingEventsForUser(preferences, competition, classification, []model.Event{{Type: "registration_opened", Key: "open"}}, model.ParticipationUndecided, now)
	if len(events) != 1 {
		t.Fatalf("expected AI competition to match, classification=%v events=%v", classification, events)
	}
	preferences.Categories = []string{"algorithm"}
	if got := MatchingEventsForUser(preferences, competition, classification, events, model.ParticipationUndecided, now); len(got) != 0 {
		t.Fatalf("unexpected category match: %v", got)
	}
}

func TestParticipationLifecycleDeliveryRules(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	future := now.AddDate(0, 0, 3)
	past := now.AddDate(0, 0, -1)
	competition := model.Competition{Status: model.StatusRegistrationOpen, RegistrationEnd: &future}
	if !EventDeliverable(competition, "registration_opened", model.ParticipationUndecided, now) {
		t.Fatal("valid registration notice was blocked")
	}
	competition.RegistrationEnd = &past
	if EventDeliverable(competition, "registration_opened", model.ParticipationUndecided, now) {
		t.Fatal("expired registration notice remained deliverable")
	}
	competition.Status = model.StatusOngoing
	competition.RegistrationEnd = &past
	if EventDeliverable(competition, "competition_started", model.ParticipationUndecided, now) {
		t.Fatal("undecided user received a start notice")
	}
	if !EventDeliverable(competition, "competition_started", model.ParticipationParticipating, now) {
		t.Fatal("participant did not receive a start notice")
	}
	competition.CompetitionEnd = &past
	if EventDeliverable(competition, "competition_started", model.ParticipationParticipating, now) {
		t.Fatal("ended competition remained deliverable")
	}
	competition.CompetitionEnd = nil
	if EventDeliverable(competition, "competition_started", model.ParticipationDeclined, now) {
		t.Fatal("declined competition remained deliverable")
	}
}

func TestNextDelivery(t *testing.T) {
	location, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 8, 3, 9, 30, 0, 0, location)
	preferences := model.UserPreferences{
		Frequency:    model.DeliveryDaily,
		DeliveryTime: "08:00",
		Timezone:     "Asia/Shanghai",
	}
	due, err := NextDelivery(now, preferences)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 4, 8, 0, 0, 0, location)
	if !due.Equal(want) {
		t.Fatalf("due=%s want=%s", due, want)
	}
}

func TestProfileEnterpriseRegionalCompetition(t *testing.T) {
	competition := model.Competition{
		Name:        "2026 重庆市开发者程序设计大赛",
		Organizer:   "某某科技有限公司",
		Content:     "面向全市高校公开报名的软件开发挑战赛",
		OfficialURL: "https://contest.example.com/2026",
	}
	profile := Profile(competition)
	if !contains(profile.OrganizerTypes, "enterprise") || !contains(profile.Scopes, "regional") || !contains(profile.Regions, "重庆") {
		t.Fatalf("unexpected profile: %#v", profile)
	}
	preferences := model.UserPreferences{
		Categories:        []string{"algorithm", "development"},
		OrganizerTypes:    []string{"enterprise"},
		CompetitionScopes: []string{"regional"},
		Regions:           []string{"重庆"},
	}
	if !MatchesCompetition(preferences, competition, profile) {
		t.Fatal("matching regional enterprise competition was rejected")
	}
	preferences.Regions = []string{"四川"}
	if MatchesCompetition(preferences, competition, profile) {
		t.Fatal("regional competition outside the selected region matched")
	}
}

func TestExistingCampusAndPostEventRowsDoNotMatch(t *testing.T) {
	preferences := model.UserPreferences{}
	tests := []model.Competition{
		{Name: "关于组织参加2026年第八届中国研究生人工智能创新大赛的通知", OfficialURL: "https://gradschool.example.edu.cn/notice/1", Status: model.StatusRegistrationOpen},
		{Name: "2026年中国大学生计算机设计大赛报名通知-某大学教务部", OfficialURL: "https://jwc.example.edu.cn/notice/2", Status: model.StatusPreview},
		{Name: "中国大学生计算机设计大赛总决赛圆满落幕", OfficialURL: "https://example.org.cn/news/3", Status: model.StatusFinished},
		{Name: "竞赛动态 - 中国计算机学会", OfficialURL: "https://example.org.cn/list", Status: model.StatusRegistrationOpen},
		{Name: "关于举办2026华为软件精英挑战赛中南大学校内赛的通知-中南大学计算机学院", OfficialURL: "https://cse.example.edu.cn/notice", Status: model.StatusRegistrationOpen, Trust: model.TrustMedium},
		{Name: "关于举办2026年中国大学生计算机设计大赛校区选拔赛的通知", OfficialURL: "https://college.example.edu.cn/notice/4", Status: model.StatusRegistrationOpen, Trust: model.TrustMedium},
		{Name: "山大学子斩获2026华为软件精英挑战赛全球总冠军", OfficialURL: "https://news.example.edu.cn/notice/5", Status: model.StatusFinished, Trust: model.TrustMedium},
	}
	for _, competition := range tests {
		if MatchesCompetition(preferences, competition, Profile(competition)) {
			t.Errorf("low-value competition matched: %s", competition.Name)
		}
	}
}

func TestRegionalPreferenceDoesNotBlockNationalCompetition(t *testing.T) {
	competition := model.Competition{
		Name:    "2026年全国大学生程序设计大赛",
		Content: "面向全国高校公开报名",
		Trust:   model.TrustHigh,
	}
	profile := Profile(competition)
	preferences := model.UserPreferences{
		Categories:        []string{"algorithm"},
		CompetitionScopes: []string{"national", "regional"},
		Regions:           []string{"重庆"},
		MinTrust:          model.TrustMedium,
	}
	if !MatchesCompetition(preferences, competition, profile) {
		t.Fatalf("national competition was blocked by regional preference: %#v", profile)
	}
}

func TestStructuredKeywordAndAliasMatching(t *testing.T) {
	competition := model.Competition{
		Name:     "云端应用创新赛",
		Keywords: []string{"服务端", "云原生"},
		Trust:    model.TrustHigh,
	}
	preferences := model.UserPreferences{IncludeKeywords: []string{"backend"}, MinTrust: model.TrustMedium}
	if !MatchesCompetition(preferences, competition, Profile(competition)) {
		t.Fatal("controlled backend alias did not match the structured 服务端 keyword")
	}
	preferences.IncludeKeywords = []string{"摄影"}
	if MatchesCompetition(preferences, competition, Profile(competition)) {
		t.Fatal("unrelated keyword matched")
	}
}

// startedEventCompetition returns a competition that passes the profile filters
// (development category, high trust, ongoing) and carries a start event.
func startedEventCompetition() model.Competition {
	return model.Competition{
		Name:        "2026 全国大学生软件开发大赛",
		Content:     "面向全国高校的软件开发比赛",
		Status:      model.StatusOngoing,
		OfficialURL: "https://contest.example.com/2026",
		Trust:       model.TrustHigh,
	}
}

// TestStartedNoticeRespectsNotifyStartedDisabled verifies the regression: a
// user who participates but has NOT enabled start notifications must not
// receive a competition_started event. The old code special-cased
// competition_started so it was always enabled, bypassing NotifyStarted.
func TestStartedNoticeRespectsNotifyStartedDisabled(t *testing.T) {
	competition := startedEventCompetition()
	preferences := model.UserPreferences{
		Categories:    []string{"development"},
		MinTrust:      model.TrustMedium,
		NotifyStarted: false,
	}
	events := MatchingEventsForUser(preferences, competition, Profile(competition),
		[]model.Event{{Type: "competition_started", Key: "started"}}, model.ParticipationParticipating, time.Now())
	if len(events) != 0 {
		t.Fatalf("start notice delivered despite NotifyStarted=false: %#v", events)
	}
}

// TestStartedNoticeDeliveredWhenEnabledAndParticipating verifies that a
// participating user with start notifications enabled receives the event.
func TestStartedNoticeDeliveredWhenEnabledAndParticipating(t *testing.T) {
	competition := startedEventCompetition()
	preferences := model.UserPreferences{
		Categories:    []string{"development"},
		MinTrust:      model.TrustMedium,
		NotifyStarted: true,
	}
	events := MatchingEventsForUser(preferences, competition, Profile(competition),
		[]model.Event{{Type: "competition_started", Key: "started"}}, model.ParticipationParticipating, time.Now())
	if len(events) != 1 || events[0].Type != "competition_started" {
		t.Fatalf("expected one start notice, got %#v", events)
	}
}

// TestStartedNoticeBlockedForUndecidedUser verifies the fix did not break the
// participation gate: an enabled but undecided user still gets no start notice.
func TestStartedNoticeBlockedForUndecidedUser(t *testing.T) {
	competition := startedEventCompetition()
	preferences := model.UserPreferences{
		Categories:    []string{"development"},
		MinTrust:      model.TrustMedium,
		NotifyStarted: true,
	}
	events := MatchingEventsForUser(preferences, competition, Profile(competition),
		[]model.Event{{Type: "competition_started", Key: "started"}}, model.ParticipationUndecided, time.Now())
	if len(events) != 0 {
		t.Fatalf("start notice delivered to an undecided user: %#v", events)
	}
}

// TestStartedNoticeBlockedForDeclinedUser verifies a user who explicitly
// declined does not receive the start notice.
func TestStartedNoticeBlockedForDeclinedUser(t *testing.T) {
	competition := startedEventCompetition()
	preferences := model.UserPreferences{
		Categories:    []string{"development"},
		MinTrust:      model.TrustMedium,
		NotifyStarted: true,
	}
	events := MatchingEventsForUser(preferences, competition, Profile(competition),
		[]model.Event{{Type: "competition_started", Key: "started"}}, model.ParticipationDeclined, time.Now())
	if len(events) != 0 {
		t.Fatalf("start notice delivered to a declining user: %#v", events)
	}
}
