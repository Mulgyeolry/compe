package analyzer

import (
	"context"
	"testing"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
)

func TestRejectsCampusInternalForward(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	analyzer := New(config.Config{Location: time.FixedZone("CST", 8*3600)})
	candidate := model.Candidate{Title: "关于组织学生参加2026年第二十二届百度之星程序设计大赛的通知"}
	document := model.Document{
		Title: "关于组织学生参加2026年第二十二届百度之星程序设计大赛的通知-某大学计算机学院",
		URL:   "https://computer.example.edu.cn/info/1000/1234.htm",
		Text:  "请各班组织本院学生报名，报名材料提交至学院。比赛内容为程序设计与算法。",
	}
	_, relevant, err := analyzer.Analyze(context.Background(), candidate, document, model.TrustMedium, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if relevant {
		t.Fatal("campus-internal forwarding announcement was accepted")
	}
}

func TestRejectsExpiredCollegeSelectionForAuthoritativeCompetition(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	location := time.FixedZone("CST", 8*3600)
	analysis := New(config.Config{Location: location})
	document := model.Document{
		Title: "关于举办2026中国高校计算机大赛人工智能创意赛校内选拔赛的通知-湖南城市学院信息与电子工程学院",
		URL:   "https://xgxy.example.edu.cn/info/2026/ai.htm",
		Text:  "承办单位：信息与电子工程学院。参赛对象主要面向目前在校本科生。报名截止：2026年7月25日。学院组织校内选拔赛。",
	}
	_, relevant, err := analysis.Analyze(context.Background(), model.Candidate{Title: document.Title}, document, model.TrustMedium, time.Date(2026, 8, 4, 20, 0, 0, 0, location))
	if err != nil {
		t.Fatal(err)
	}
	if relevant {
		t.Fatal("expired college-internal selection was accepted because the parent competition is authoritative")
	}
}

func TestRejectsNamedUniversityCampusRoundOfAuthoritativeCompetition(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	analysis := New(config.Config{Location: time.FixedZone("CST", 8*3600)})
	document := model.Document{
		Title: "关于举办2026华为软件精英挑战赛中南大学校内赛的通知-中南大学计算机学院",
		URL:   "https://cse.example.edu.cn/info/2026/huawei.htm",
		Text:  "2026华为软件精英挑战赛中南大学校内赛现已开始报名。",
	}
	_, relevant, err := analysis.Analyze(context.Background(), model.Candidate{Title: document.Title}, document, model.TrustMedium, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if relevant {
		t.Fatal("named university campus round was accepted because the parent competition is authoritative")
	}
}

func TestKeepsPublicUniversityHostedCompetition(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	analyzer := New(config.Config{Location: time.FixedZone("CST", 8*3600)})
	candidate := model.Candidate{Title: "2026重庆市大学生程序设计大赛报名通知"}
	document := model.Document{
		Title: "2026重庆市大学生程序设计大赛报名通知",
		URL:   "https://contest.example.edu.cn/notice/2026.htm",
		Text:  "本赛事面向全市高校公开报名，现已开放报名。主办方：重庆市计算机学会。比赛内容为程序设计与算法。",
	}
	_, relevant, err := analyzer.Analyze(context.Background(), candidate, document, model.TrustMedium, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !relevant {
		t.Fatal("public regional competition hosted on a university site was rejected")
	}
}

func TestRejectsCampusForwardEvenWhenUnderlyingCompetitionIsNational(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	analysis := New(config.Config{Location: time.FixedZone("CST", 8*3600)})
	document := model.Document{
		Title: "关于组织参加2026年全国大学生人工智能创新大赛的通知",
		URL:   "https://gradschool.example.edu.cn/notice/2026.htm",
		Text:  "本赛事面向全国高校公开报名，请各学院组织本校学生提交材料。比赛内容为人工智能应用开发。",
	}
	_, relevant, err := analysis.Analyze(context.Background(), model.Candidate{Title: document.Title}, document, model.TrustMedium, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if relevant {
		t.Fatal("campus forwarding was accepted because the underlying competition was national")
	}
}

func TestRejectsPostEventNews(t *testing.T) {
	analyzer := New(config.Config{Location: time.FixedZone("CST", 8*3600)})
	for _, title := range []string{
		"第十九届中国大学生计算机设计大赛圆满落幕",
		"山大学子斩获2026华为软件精英挑战赛全球总冠军",
		"中国大学生计算机设计大赛获奖公告",
	} {
		if score := analyzer.CandidateScore(title, "获奖队伍参加颁奖典礼"); score >= 0 {
			t.Errorf("post-event article %q score = %d", title, score)
		}
	}
}

func TestConflictingPastDeadlinesCloseWithoutCountdownDate(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	location := time.FixedZone("CST", 8*3600)
	analysis := New(config.Config{Location: location})
	document := model.Document{
		Title: "全国高校 AI Agent 创新大赛延期通知",
		URL:   "https://contest.example.edu.cn/notice/2026",
		Text:  "面向全国高校公开报名。线上报名：即日起至2026年5月4日。报名阶段：即日起至2026年5月15日。比赛内容为AI Agent应用开发。",
	}
	competition, relevant, err := analysis.Analyze(context.Background(), model.Candidate{Title: document.Title}, document, model.TrustMedium, time.Date(2026, 8, 4, 8, 0, 0, 0, location))
	if err != nil {
		t.Fatal(err)
	}
	if !relevant || competition.Status != model.StatusRegistrationClosed {
		t.Fatalf("status=%s relevant=%v", competition.Status, relevant)
	}
	if competition.RegistrationEnd != nil || competition.RegistrationEndRaw != "" {
		t.Fatalf("conflicting deadline was accepted: %v %q", competition.RegistrationEnd, competition.RegistrationEndRaw)
	}
}

func TestExtractsFeeOnlyWhenExplicit(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_MODEL", "")
	analysis := New(config.Config{Location: time.FixedZone("CST", 8*3600)})
	document := model.Document{
		Title: "2026全国大学生软件开发大赛报名通知",
		URL:   "https://contest.example.org/2026",
		Text:  "本赛事面向全国高校公开报名，现已开放报名。报名费为50元/人。比赛内容为软件开发。",
	}
	competition, relevant, err := analysis.Analyze(context.Background(), model.Candidate{Title: document.Title}, document, model.TrustHigh, time.Now())
	if err != nil || !relevant {
		t.Fatalf("relevant=%v err=%v", relevant, err)
	}
	if competition.Fee != "50元/人" || competition.FeeEvidence == "" {
		t.Fatalf("fee=%q evidence=%q", competition.Fee, competition.FeeEvidence)
	}
}

func TestCompetitionStartStatuses(t *testing.T) {
	if status, _ := detectStatus("赛事即将开赛，请参赛队伍提前准备"); status != model.StatusUpcoming {
		t.Fatalf("upcoming status=%s", status)
	}
	if status, _ := detectStatus("赛事今日正式开赛，赛题已经发布"); status != model.StatusOngoing {
		t.Fatalf("ongoing status=%s", status)
	}
	if status, _ := detectStatus("报名已经截止，赛事即将开赛"); status != model.StatusUpcoming {
		t.Fatalf("upcoming start must take precedence over closed registration: %s", status)
	}
	if status, _ := detectStatus("报名已经截止，赛事今日正式开赛"); status != model.StatusOngoing {
		t.Fatalf("official start must take precedence over closed registration: %s", status)
	}
}
