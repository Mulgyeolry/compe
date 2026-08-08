package analyzer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
)

func publicFallbackCompetition(start, end *time.Time) model.Competition {
	return model.Competition{
		Trust:             model.TrustMedium,
		FitScore:          80,
		RegistrationStart: start,
		RegistrationEnd:   end,
	}
}

func Test1PublicCampusFallbackAcceptsHuaweiSafePage(t *testing.T) {
	// Huawei-style SAFE page: campus classification, computer_related=true,
	// announcement=false, no campus-internal markers, explicit public scope,
	// complete registration AND competition ranges. Fallback must accept,
	// keeping dates and without Enrich (asserted separately in integration).
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, shanghai)
	regS := time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)
	regE := time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)
	compS := time.Date(2026, 9, 23, 8, 0, 0, 0, shanghai)
	compE := time.Date(2026, 9, 27, 12, 0, 0, 0, shanghai)
	comp := publicFallbackCompetition(&regS, &regE)
	comp.CompetitionStart = &compS
	comp.CompetitionEnd = &compE

	// computer_related=false on purpose: the fallback must not depend on the
	// AI's unstable computer_related output, only on the deterministic focus hit.
	class := AIClassification{
		ComputerRelated:         false,
		CompetitionAnnouncement: false,
		SourceRole:              SourceCampusForward,
		DocumentType:            DocumentCampusInternal,
		RejectionReason:         "校内转发通知，非官方主办方发布的有效公告，且数学建模竞赛非计算机领域",
	}
	a := &Analyzer{focus: defaultFocus}
	if !a.canUsePublicCampusRulesFallback(
		model.Candidate{Title: "关于2026年华为杯第二十三届中国研究生数学建模竞赛报名的通知"},
		model.Document{URL: "https://gradschool.ustc.edu.cn/article/3487", Title: "关于2026年华为杯第二十三届中国研究生数学建模竞赛报名的通知", Text: "参赛团队报名时间：2026年6月1日8:00至9月19日17:00。竞赛时间：2026年9月23日8:00至9月27日12:00。面向全国高校公开报名。"},
		comp, class) {
		t.Fatal("expected public-campus fallback to accept the Huawei SAFE page even with AI computer_related=false")
	}
	_ = now
}

func Test2PublicCampusFallbackRejectsSchoolSelection(t *testing.T) {
	// Real dangerous scenario: "关于组织我校学生参加全国XXX大赛的通知" with
	// "我校将组织校内选拔" and "面向全国高校". Despite the public scope and
	// complete dates, campus-internal markers must reject it.
	regS := time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)
	regE := time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)
	comp := publicFallbackCompetition(&regS, &regE)
	class := AIClassification{
		ComputerRelated:         true,
		CompetitionAnnouncement: false,
		SourceRole:              SourceCampusForward,
		DocumentType:            DocumentCampusInternal,
	}
	a := &Analyzer{focus: defaultFocus}
	doc := model.Document{Text: "关于组织我校学生参加全国XXX大赛的通知\n我校将组织校内选拔……面向全国高校……"}
	if a.canUsePublicCampusRulesFallback(model.Candidate{Title: "关于组织我校学生参加全国XXX大赛的通知"}, doc, comp, class) {
		t.Fatal("expected campus-internal markers to reject the school-selection page")
	}
}

func Test3PublicCampusFallbackRejectsBareNational(t *testing.T) {
	// "全国大学生XXX竞赛" in the name, but no explicit participation scope
	// (no 面向全国/公开报名/全国高校均可). A bare 全国 is not sufficient.
	regS := time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)
	regE := time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)
	comp := publicFallbackCompetition(&regS, &regE)
	class := AIClassification{
		ComputerRelated:         true,
		CompetitionAnnouncement: false,
		SourceRole:              SourceCampusForward,
		DocumentType:            DocumentCampusInternal,
	}
	a := &Analyzer{focus: defaultFocus}
	doc := model.Document{Text: "全国大学生XXX竞赛，报名时间：2026年6月1日8:00至9月19日17:00"}
	if a.canUsePublicCampusRulesFallback(model.Candidate{Title: "全国大学生XXX竞赛"}, doc, comp, class) {
		t.Fatal("expected bare 全国 name qualifier to NOT satisfy public scope")
	}
}

func Test4PublicCampusFallbackRejectsSingleDate(t *testing.T) {
	// campus + public scope marker, but only a single end date. Fallback must
	// require a complete range.
	class := AIClassification{
		ComputerRelated:         true,
		CompetitionAnnouncement: false,
		SourceRole:              SourceCampusForward,
		DocumentType:            DocumentCampusInternal,
	}
	a := &Analyzer{focus: defaultFocus}
	// RegistrationStart nil, RegistrationEnd non-nil, CompetitionStart/End nil.
	regE := time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)
	comp := model.Competition{Trust: model.TrustMedium, FitScore: 80, RegistrationEnd: &regE}
	doc := model.Document{Text: "报名截止：2026年9月19日17:00。面向全国高校公开报名。"}
	if a.canUsePublicCampusRulesFallback(model.Candidate{Title: "XXX竞赛"}, doc, comp, class) {
		t.Fatal("expected single date to reject fallback")
	}
}

func Test5PublicCampusFallbackNeverFiresOnListing(t *testing.T) {
	regS := time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)
	regE := time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)
	comp := publicFallbackCompetition(&regS, &regE)
	class := AIClassification{
		ComputerRelated:         true,
		CompetitionAnnouncement: false,
		SourceRole:              SourceOfficialPrimary,
		DocumentType:            DocumentListing,
	}
	a := &Analyzer{focus: defaultFocus}
	doc := model.Document{Text: "面向全国高校公开报名。报名时间：2026年6月1日8:00至9月19日17:00"}
	if a.canUsePublicCampusRulesFallback(model.Candidate{Title: "竞赛列表"}, doc, comp, class) {
		t.Fatal("expected listing to never fall back")
	}
}

func Test6PublicCampusFallbackNeverFiresOnPostEventNews(t *testing.T) {
	regS := time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)
	regE := time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)
	comp := publicFallbackCompetition(&regS, &regE)
	class := AIClassification{
		ComputerRelated:         true,
		CompetitionAnnouncement: false,
		SourceRole:              SourceCampusForward,
		DocumentType:            DocumentPostEventNews,
	}
	a := &Analyzer{focus: defaultFocus}
	doc := model.Document{Text: "我校学生荣获全国XXX大赛一等奖。面向全国高校。比赛时间：2026年9月23日8:00至9月27日12:00"}
	if a.canUsePublicCampusRulesFallback(model.Candidate{Title: "获奖喜报"}, doc, comp, class) {
		t.Fatal("expected post_event_news to never fall back")
	}
}

func Test7PublicCampusFallbackNeverFiresOnLowTrust(t *testing.T) {
	regS := time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)
	regE := time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)
	comp := publicFallbackCompetition(&regS, &regE)
	comp.Trust = model.TrustLow
	class := AIClassification{
		ComputerRelated:         true,
		CompetitionAnnouncement: false,
		SourceRole:              SourceCampusForward,
		DocumentType:            DocumentCampusInternal,
	}
	a := &Analyzer{focus: defaultFocus}
	doc := model.Document{Text: "面向全国高校公开报名。报名时间：2026年6月1日8:00至9月19日17:00"}
	if a.canUsePublicCampusRulesFallback(model.Candidate{Title: "XXX竞赛"}, doc, comp, class) {
		t.Fatal("expected low trust to never fall back")
	}
}

func Test8PublicCampusFallbackDoesNotAlterOfficialPath(t *testing.T) {
	// An official_primary + announcement=true classification must keep the
	// existing Classify -> Enrich path; the fallback must not fire.
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"rejection_reason":""}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10","identity":{"name":{"value":"2026全国大学生程序设计大赛","evidence":"2026全国大学生程序设计大赛","edition":"2026","confidence":"high"},"organizer":{"value":"中国计算机学会","evidence":"主办方：中国计算机学会","edition":"2026","confidence":"high"}},"facts":{"fee":{"value":"50元/人","evidence":"报名费为50元/人","edition":"2026","confidence":"high"}},"events":[]}`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	analysis := New(config.Config{Location: shanghai})
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, shanghai)

	announcement := model.Document{
		Title: "2026全国大学生程序设计大赛报名通知",
		URL:   "https://www.cspro.org/news/2026",
		Text:  "2026全国大学生程序设计大赛报名通知。本赛事面向全国高校公开报名，现已开放报名。报名费为50元/人。主办方：中国计算机学会。报名时间：2026年6月1日8:00至9月19日17:00。",
	}
	competition, relevant, err := analysis.Analyze(context.Background(), model.Candidate{Title: announcement.Title}, announcement, model.TrustHigh, now)
	if err != nil {
		t.Fatal(err)
	}
	if !relevant {
		t.Fatal("official announcement failed the classification gate")
	}
	if calls.Load() != 2 {
		t.Fatalf("official announcement made %d requests, want exactly 2 (classification + extraction), fallback must not short-circuit Enrich", calls.Load())
	}
	if competition.Fee != "50元/人" {
		t.Errorf("expected Enrich to have applied AI fee, got %q", competition.Fee)
	}
}

func TestHuaweiAnalyzeUsesFallbackWithoutEnrich(t *testing.T) {
	// End-to-end through Analyze: the AI rejects the Huawei page as campus
	// forwarding, but the deterministic fallback accepts its rules facts.
	// Exactly one LLM request (classification only) — Enrich must not run.
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// computer_related=false: the fallback must not depend on AI's unstable
		// computer_related output.
		_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10","document_type":"campus_internal","source_role":"campus_forwarding","computer_related":false,"competition_announcement":false,"rejection_reason":"校内转发通知，非官方主办方发布的有效公告，且数学建模竞赛非计算机领域"}`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	analysis := New(config.Config{Location: shanghai})
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, shanghai)

	huawei := model.Document{
		Title: "关于2026年华为杯第二十三届中国研究生数学建模竞赛报名的通知",
		URL:   "https://gradschool.ustc.edu.cn/article/3487",
		Text:  "参赛团队报名时间：2026年6月1日8:00至9月19日17:00。参赛缴费时间：2026年6月1日8:00至9月21日17:00。竞赛时间：2026年9月23日8:00至9月27日12:00。本赛事面向全国高校公开报名。",
	}
	competition, relevant, err := analysis.Analyze(context.Background(), model.Candidate{Title: huawei.Title}, huawei, model.TrustMedium, now)
	if err != nil {
		t.Fatal(err)
	}
	if !relevant {
		t.Fatal("expected Huawei public-campus page to be relevant via fallback")
	}
	if calls.Load() != 1 {
		t.Fatalf("Huawei fallback made %d LLM requests, want exactly 1 (classification only, no Enrich)", calls.Load())
	}
	if competition.RegistrationStart == nil || competition.RegistrationEnd == nil {
		t.Error("expected deterministic registration range to be preserved")
	}
	if competition.CompetitionStart == nil || competition.CompetitionEnd == nil {
		t.Error("expected deterministic competition range to be preserved")
	}
	if competition.Trust != model.TrustMedium {
		t.Errorf("fallback must not promote trust, got %v", competition.Trust)
	}
	if competition.ExtractionAudit.AnalyzerVersion != AIAnalyzerVersion {
		t.Errorf("fallback audit analyzer_version = %q, want %q", competition.ExtractionAudit.AnalyzerVersion, AIAnalyzerVersion)
	}
	if len(competition.ExtractionAudit.Rejections) == 0 {
		t.Error("fallback audit must record a campus-forwarding rejection note")
	}
}

func TestNonFocusCampusPageNeverFallsBack(t *testing.T) {
	// A campus-forwarded page that does NOT hit the deterministic focus list
	// must never fall back, even if every other condition is satisfied.
	regS := time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)
	regE := time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)
	comp := publicFallbackCompetition(&regS, &regE)
	class := AIClassification{
		CompetitionAnnouncement: false,
		SourceRole:              SourceCampusForward,
		DocumentType:            DocumentCampusInternal,
	}
	a := &Analyzer{focus: defaultFocus}
	// "某地区技能大赛" is not in defaultFocus.
	doc := model.Document{Title: "2026年某地区信息技术技能大赛", Text: "面向全国高校公开报名。报名时间：2026年6月1日8:00至9月19日17:00。"}
	if a.canUsePublicCampusRulesFallback(model.Candidate{Title: "2026年某地区信息技术技能大赛"}, doc, comp, class) {
		t.Fatal("expected a non-focus campus page to never fall back")
	}
}

func TestLowValueRejectsXiamenStyleSchoolSelection(t *testing.T) {
	// "（校级）选拔赛" must be caught by the deterministic gate before AI. After
	// normalisation "（校级）选拔赛" -> "校级选拔赛", it matches "校级选拔".
	doc := model.Document{
		URL:   "https://cxw.xmu.edu.cn/cms/abc",
		Title: "2026年厦门大学计算机设计竞赛 暨2026年（第19届）中国大学生计算机设计大赛（校级）选拔赛报名通知 - 厦门大学本科生创新网",
		Text:  "参赛对象：厦门大学在校本科学生。本赛事面向全国高校公开报名。选拔赛报名时间：2026年6月1日8:00至9月19日17:00。",
	}
	if reason := lowValueReason(model.Candidate{Title: doc.Title}, doc, doc.Text); reason == "" {
		t.Fatal("expected the school-selection page to be rejected by the deterministic gate")
	}
}

func TestLowValueKeepsBeiyuOfficial4C(t *testing.T) {
	// A genuine official 4C (中国大学生计算机设计大赛) national announcement hosted
	// on a .edu.cn site must NOT be rejected by the campus gate.
	doc := model.Document{
		URL:   "https://jsjds.blcu.edu.cn/news/4c2026",
		Title: "关于2026年（第19届）中国大学生计算机设计大赛报名的通知",
		Text:  "2026年（第19届）中国大学生计算机设计大赛面向全国高校在校生公开报名。报名时间：2026年6月1日8:00至9月19日17:00。",
	}
	if reason := lowValueReason(model.Candidate{Title: doc.Title}, doc, doc.Text); reason != "" {
		t.Fatalf("expected the official 4C announcement not to be rejected, got reason %q", reason)
	}
}
