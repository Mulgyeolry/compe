package analyzer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
)

var (
	yearPattern = regexp.MustCompile(`20\d{2}`)
	// dateTokenPattern requires an explicit full year. It is used by
	// standalone start/end/deadline patterns so a year-less date is never
	// guessed and an archived page cannot be mis-bound to the current edition.
	dateTokenPattern = `20\d{2}\s*[年./-]\s*\d{1,2}\s*[月./-]\s*\d{1,2}\s*日?(?:\s*\d{1,2}\s*(?::|：|时)\s*\d{0,2}\s*分?)?`
	// dateTokenLaxPattern allows the leading year to be omitted. It is used
	// ONLY for the right-hand side of a range expression, where the year is
	// inherited from the explicitly-dated left-hand side. The left-hand side
	// still requires an explicit year via dateTokenPattern.
	dateTokenLaxPattern = `(?:(?:20\d{2})\s*[年./-]\s*)?\d{1,2}\s*[月./-]\s*\d{1,2}\s*日?(?:\s*\d{1,2}\s*(?::|：|时)\s*\d{0,2}\s*分?)?`
	datePartsPattern    = regexp.MustCompile(`(20\d{2})\s*[年./-]\s*(\d{1,2})\s*[月./-]\s*(\d{1,2})\s*日?(?:\s*(\d{1,2})\s*(?::|：|时)\s*(\d{0,2})\s*分?)?`)
	// datePartsLaxPattern captures an optional leading year so the year can be
	// inherited from the range's start when the page omits it.
	datePartsLaxPattern     = regexp.MustCompile(`(?:(20\d{2})\s*[年./-]\s*)?(\d{1,2})\s*[月./-]\s*(\d{1,2})\s*日?(?:\s*(\d{1,2})\s*(?::|：|时)\s*(\d{0,2})\s*分?)?`)
	dateRangePattern        = regexp.MustCompile(`(?:报名|注册)(?:开始)?(?:时间)?\s*[：:]?\s*(` + dateTokenPattern + `)\s*(?:至|到|—|–|~|～)\s*(` + dateTokenLaxPattern + `)`)
	startPattern            = regexp.MustCompile(`(?:报名|注册)(?:开始|开放)(?:时间)?\s*[：:]?\s*(` + dateTokenPattern + `)`)
	endPattern              = regexp.MustCompile(`(?:报名|注册)(?:截止|结束)(?:时间)?\s*[：:]?\s*(` + dateTokenPattern + `)`)
	looseEndPattern         = regexp.MustCompile(`(?:报名|注册)[^。；;\n]{0,80}?(?:截止(?:时间)?(?:为|至|到|于)?|结束(?:时间)?(?:为|至|到|于)?|即日起(?:至|到))\s*[：:]?\s*(` + dateTokenPattern + `)`)
	organizerPattern        = regexp.MustCompile(`(?:主办方|主办单位|主办机构)\s*[：:]\s*([^。；;]{2,100})`)
	feePattern              = regexp.MustCompile(`(?:报名费|参赛费|比赛费用|赛事费用|参赛费用)\s*[：:为]?\s*(?:人民币)?\s*(\d+(?:\.\d{1,2})?\s*元(?:\s*(?:/|每)(?:人|队))?)`)
	upcomingPattern         = regexp.MustCompile(`(?:将于|计划于)[^。！？；;\n]{0,60}(?:开赛|开始比赛)`)
	competitionRangePattern = regexp.MustCompile(`(?:比赛|竞赛|赛事)(?:时间|赛程)\s*[:：]?\s*(` + dateTokenPattern + `)\s*(?:至|到|—|-|~|～)\s*(` + dateTokenLaxPattern + `)`)
	competitionStartPattern = regexp.MustCompile(`(?:开赛|比赛开始|竞赛开始|赛事开始)(?:时间)?\s*[:：]?\s*(` + dateTokenPattern + `)`)
	competitionEndPattern   = regexp.MustCompile(`(?:比赛|竞赛|赛事)(?:结束|截止)(?:时间)?\s*[:：]?\s*(` + dateTokenPattern + `)`)
	// competitionDayRangePattern matches a competition date range whose
	// right-hand side only states a day (e.g. "26" or "26日"), inheriting the
	// year and month from the explicitly-dated left-hand side. It supports both
	// the explicit "比赛/竞赛/赛事时间：" form and a natural-language
	// "将于...~...举行/举办" expression. Group 3 captures an explicit time
	// written after the day (e.g. "18:00" / "18时"); when present the whole
	// fallback is rejected because this adapter does not support a day-only end
	// with its own time and must never inherit the left-hand time. It is used
	// ONLY for competition dates; registration ranges keep the stricter
	// dateTokenLaxPattern and are never broadened to a day-only end.
	competitionDayRangePattern = regexp.MustCompile(
		`(?:(?:比赛|竞赛|赛事)(?:时间|赛程)?\s*[:：]?\s*|(?:将于|计划于|定于)[^。！？；;\n]{0,40}?)` +
			`(` + dateTokenPattern + `)\s*(?:至|到|—|–|-|~|～)\s*(\d{1,2})\s*日?` +
			`(?:\s*(\d{1,2})\s*[：:时]\s*\d{0,2}\s*分?)?` +
			`(?:[^。！？；;\n]{0,40}?(?:举行|举办|开幕|开赛|召开|开始|进行))?`,
	)
)

var defaultFocus = []string{
	"CCF CSP", "CCF CCSP", "CCF CAT", "CCPC", "ICPC", "ACM-ICPC", "团体程序设计天梯赛", "GPLT", "蓝桥杯",
	"华为ICT大赛", "华为 ICT 大赛", "华为软件精英挑战赛", "HarmonyOS", "昇腾", "天池", "百度之星程序设计大赛",
	"中国大学生计算机设计大赛", "中国高校计算机大赛", "全国大学生计算机系统能力大赛", "中国大学生服务外包创新创业大赛",
	"全国大学生信息安全竞赛", "CISC", "0CTF", "全国大学生数学建模竞赛", "CUMCM", "中国国际大学生创新大赛", "挑战杯",
	"中国研究生数学建模竞赛", "中国研究生人工智能创新大赛", "中国研究生网络安全创新大赛", "中国研究生电子设计竞赛",
	"中国研究生创芯大赛", "EDA精英挑战赛", "EDA 精英挑战赛", "中国研究生智慧城市技术与创意设计大赛",
	"中国研究生机器人创新设计大赛", "中国研究生操作系统开源创新大赛", "中国研究生金融科技创新大赛",
	"微软创新杯", "花旗杯金融创新应用大赛", "全国高校计算机能力挑战赛", "司南杯量子计算编程挑战赛",
	"华北五省大学生计算机应用大赛", "海峡两岸暨港澳地区大学生计算机创新作品赛", "东北四省大学生程序设计邀请赛", "长三角大学生计算机设计邀请赛",
}

var defaultPositive = []string{
	"AI Agent", "智能体", "RAG", "大模型", "人工智能", "Go后端", "Go 后端", "后端开发", "云计算", "云原生",
	"Kubernetes", "软件开发", "程序设计", "算法", "黑客松", "Hackathon", "开发者大赛", "计算机", "编程", "开源",
}

var defaultNegative = []string{"烹饪", "厨艺", "舞蹈", "歌唱", "书法", "摄影", "诗歌", "体育", "英语演讲", "服装设计", "公共政策案例", "社会治理案例"}

var postEventTitleMarkers = []string{
	"圆满落幕", "圆满结束", "圆满收官", "大赛收官", "赛事收官", "获奖名单", "获奖作品", "成绩公示",
	"结果公示", "入围名单", "入围作品", "晋级名单", "赛事回顾", "大赛回顾", "精彩回顾", "比赛纪实",
	"颁奖典礼", "成功举办", "总决赛举行", "决赛举行", "议程一览", "申诉投诉处理结果",
	"参赛作品信息核查", "人才沙龙", "座谈会",
	"获奖公告", "获奖结果", "斩获", "获佳绩", "荣获", "夺得", "摘得", "全球总冠军", "喜报",
	"高分说", "经验分享", "选手专访", "赛后报道", "赛后回顾",
}

var genericListingTitleMarkers = []string{
	"竞赛动态 - 中国计算机学会", "竞赛动态-中国计算机学会", "竞赛公告列表", "赛事活动列表", "开发者活动列表", "华为人才在线",
}

var campusInternalMarkers = []string{
	"关于组织学生参加", "关于组织我校学生参加", "关于组织同学参加", "请各班组织", "请各班级组织",
	"请各学院组织", "请各学院", "各学院报送", "各学院提交", "校内报名通知", "学院内部通知",
	"校内选拔", "院内选拔", "仅限本校", "仅面向本校", "本院学生", "我院学生", "本校学生",
	"报名请联系辅导员", "以学院为单位报名", "报送至学院", "提交至学院",
}

var explicitCampusCompetitionMarkers = []string{
	"校内赛", "校内比赛", "校内竞赛", "院内赛", "院内比赛", "院内竞赛",
	"校赛", "校区选拔", "校内选拔", "学校选拔赛", "校园选拔赛",
	"校级选拔", "院级选拔", "学院选拔赛", "本校赛", "本院赛",
}

var publicParticipationMarkers = []string{
	"面向全国", "面向社会", "面向全球", "公开报名", "均可报名", "所有开发者", "全国高校",
	"全市高校", "全省高校", "各高校均可", "社会公众",
}

// publicCampusFallbackMarkers are explicit "open to the public" participation
// scope expressions strong enough to accept a campus-forwarded page's
// deterministic ruleAnalysis facts. A bare "全国", or a competition-name
// qualifier such as 全国大学生/中国研究生/全国研究生, is deliberately NOT included:
// "关于组织我校学生参加全国XX大赛" names 全国 yet is still a campus-internal
// forwarding notice. Only unambiguous participation-scope wording counts.
var publicCampusFallbackMarkers = []string{
	"面向全国", "面向全国高校", "面向全国研究生", "面向全国大学生", "面向全球", "面向社会",
	"面向社会公众", "面向所有高校", "全国高校均可", "各高校均可", "社会公众", "公开报名",
}

type Analyzer struct {
	location *time.Location
	focus    []string
	positive []string
	negative []string
	llm      *LLM
}

func New(cfg config.Config) *Analyzer {
	focus := append([]string{}, defaultFocus...)
	focus = append(focus, cfg.Keywords.Focus...)
	positive := append([]string{}, defaultPositive...)
	positive = append(positive, cfg.Keywords.Positive...)
	negative := append([]string{}, defaultNegative...)
	negative = append(negative, cfg.Keywords.Negative...)
	return &Analyzer{location: cfg.Location, focus: unique(focus), positive: unique(positive), negative: unique(negative), llm: NewLLMFromEnvironment()}
}

func (a *Analyzer) ResearchEnabled() bool { return a != nil && a.llm.Enabled() }

func (a *Analyzer) Version() string { return AIAnalyzerVersion }

func (a *Analyzer) KeywordsForCompetition(competition model.Competition) []string {
	return unique(append(append([]string{}, competition.Keywords...), extractKeywords(strings.Join([]string{
		competition.Name, competition.Organizer, competition.Content, competition.FitReason, competition.TeamRequirement,
	}, " "))...))
}

func (a *Analyzer) CandidateScore(title, snippet string) int {
	text := strings.ToLower(title + " " + snippet)
	if containsAny(title, postEventTitleMarkers) || containsAny(title, genericListingTitleMarkers) {
		return -100
	}
	score := 0
	for _, keyword := range a.focus {
		if strings.Contains(text, strings.ToLower(keyword)) {
			score += 100
		}
	}
	for _, keyword := range a.positive {
		if strings.Contains(text, strings.ToLower(keyword)) {
			score += 15
		}
	}
	negativeHits := 0
	for _, keyword := range a.negative {
		if strings.Contains(text, strings.ToLower(keyword)) {
			negativeHits++
		}
	}
	if negativeHits > 0 && score < 30 {
		return -100
	}
	return score - negativeHits*20
}

func (a *Analyzer) Analyze(ctx context.Context, candidate model.Candidate, doc model.Document, trust model.Trust, now time.Time) (model.Competition, bool, error) {
	text := normalize(doc.Title + " " + candidate.Title + " " + doc.Text)
	if lowValueReason(candidate, doc, text) != "" {
		return model.Competition{}, false, nil
	}
	score := a.CandidateScore(candidate.Title+" "+doc.Title, text)
	competition := a.ruleAnalysis(candidate, doc, trust, text, score, now)
	if score < 15 {
		return competition, false, nil
	}

	// Lifecycle claims deserve the same scrutiny as ambiguous classification:
	// when a model is configured, verify them as edition-bound events instead
	// of allowing a page-wide keyword match to become canonical state.
	needsLLM := score < 60 || competition.Organizer == "" || competition.Content == "" || competition.Status != model.StatusUnknown
	if needsLLM && a.llm.Enabled() {
		// First pass is a single tiny request: is this a trackable
		// announcement at all? Ordinary listing, campus and post-event
		// pages stop here without ever paying for the extraction call.
		classification, raw, err := a.llm.Classify(ctx, candidate, doc)
		if err != nil {
			return pendingCompetition(competition, doc, now, nil, fmt.Errorf("llm classification deferred: %w", err))
		}
		competition.ExtractionAudit = model.AnalysisAudit{
			AnalyzerVersion: AIAnalyzerVersion,
			Model:           a.llm.ModelName(),
			InputHash:       analysisInputHash(doc),
			RawResponses:    []string{truncateRunes(raw, 16000)},
			AnalyzedAt:      now,
		}
		if err := validateClassification(classification); err != nil {
			return pendingCompetition(competition, doc, now, []string{raw}, fmt.Errorf("llm classification validation deferred: %w", err))
		}
		// The classification gate decides whether extraction is worth paying
		// for at all. Listing, campus and post-event pages stop here with a
		// single tiny request, keeping long-context extraction calls for
		// genuine announcements only.
		if !classificationCanUpdateCanonical(classification) {
			if a.canUsePublicCampusRulesFallback(candidate, doc, competition, classification) {
				// The AI rejected the page as campus-internal/forwarding, but the
				// deterministic rules already extracted strong public-competition
				// evidence (explicit participation scope, no campus-internal
				// markers, at least one complete dated range). Keep those
				// ruleAnalysis facts without calling Enrich and without
				// promoting trust or source role.
				competition.ExtractionAudit = buildCampusRulesFallbackAudit(competition, doc, now, a.llm.ModelName())
				return competition, true, nil
			}
			return competition, false, nil
		}
		result, extractionErr := a.llm.Enrich(ctx, candidate, doc)
		partialError := IsPartialEnrichmentError(extractionErr)
		if extractionErr != nil && !partialError {
			return pendingCompetition(competition, doc, now, []string{raw}, fmt.Errorf("llm extraction deferred: %w", extractionErr))
		}
		applyClassification(&result, classification)
		result.RawResponses = append([]string{truncateRunes(raw, 16000)}, result.RawResponses...)
		result, err = validateAIResult(result, doc, now, a.location)
		if err != nil {
			return pendingCompetition(competition, doc, now, result.RawResponses, fmt.Errorf("llm result validation deferred: %w", err))
		}
		competition.ExtractionAudit = buildAnalysisAudit(result, doc, now, a.llm.ModelName(), nil)
		if !aiDocumentCanUpdateCanonical(result) {
			return competition, false, nil
		}
		competition = a.mergeAI(competition, result, doc, now)
		competition.ExtractionAudit = buildAnalysisAudit(result, doc, now, a.llm.ModelName(), competition.Facts)
		if partialError {
			// A partially analyzed result retains stable fields but must not be
			// treated as a complete success: the failed segments (or unresolved
			// ties) must be re-analyzed on the next scan before lifecycle state
			// can be trusted. Signal the retry without clearing the preserved
			// fields via pendingCompetition.
			competition.ExtractionAudit.Error = extractionErr.Error()
			return competition, true, &PendingCandidateError{
				Err: fmt.Errorf("llm extraction partially deferred: %w", extractionErr),
			}
		}
		if !result.ComputerRelated {
			return competition, false, nil
		}
	} else if needsLLM && a.llm.Configured() {
		return pendingCompetition(competition, doc, now, nil, errors.New("llm classification deferred: model circuit is temporarily open"))
	}
	if competition.ExtractionAudit.AnalyzerVersion == "" {
		competition.ExtractionAudit = buildRuleAudit(doc, now, competition.Facts)
	}
	return competition, true, nil
}

func pendingCompetition(competition model.Competition, doc model.Document, now time.Time, raw []string, cause error) (model.Competition, bool, error) {
	if competition.Trust == model.TrustLow || strings.TrimSpace(competition.Name) == "" {
		return competition, false, cause
	}
	competition.Status = model.StatusUnknown
	competition.StatusEvidence = ""
	competition.RegistrationPhase = model.RegistrationUnknown
	competition.CompetitionPhase = model.CompetitionUnknown
	competition.RegistrationStart = nil
	competition.RegistrationStartRaw = ""
	competition.RegistrationEnd = nil
	competition.RegistrationEndRaw = ""
	competition.CompetitionStart = nil
	competition.CompetitionStartRaw = ""
	competition.CompetitionEnd = nil
	competition.CompetitionEndRaw = ""
	competition.Organizer = ""
	competition.TeamRequirement = ""
	competition.Fee = ""
	competition.FeeEvidence = ""
	competition.EligibilityNote = ""
	competition.ProblemReleased = false
	competition.Facts = map[string]model.FactEvidence{}
	competition.EntityKey = EntityKey(competition.Name, "")
	competition.FitReason = "AI 分析待补全，报名状态尚未确认。"
	competition.ExtractionAudit = model.AnalysisAudit{
		AnalyzerVersion: AIAnalyzerVersion,
		InputHash:       analysisInputHash(doc),
		RawResponses:    append([]string(nil), raw...),
		Error:           cause.Error(),
		AnalyzedAt:      now,
	}
	return competition, true, &PendingCandidateError{Err: cause}
}

func lowValueReason(candidate model.Candidate, doc model.Document, text string) string {
	if doc.IsListing {
		return "structural listing page"
	}
	title := strings.TrimSpace(candidate.Title + " " + doc.Title)
	if containsAny(title, postEventTitleMarkers) {
		return "post-event news"
	}
	if containsAny(title, genericListingTitleMarkers) {
		return "generic listing page"
	}
	// Match explicit campus-competition markers on a lightly normalised copy of
	// title+text: strip whitespace and both full-width/half-width brackets so a
	// phrase like "（校级）选拔赛" is seen as "校级选拔赛" and matches "校级选拔".
	// This closes the gap where "校级"+"选拔赛" separated by brackets/punctuation
	// would otherwise slip past the deterministic gate.
	normalisedScope := normalizeForMarkerMatch(title + " " + text)
	if containsAny(normalisedScope, explicitCampusCompetitionMarkers) {
		return "explicit campus-internal competition"
	}
	parsed, _ := url.Parse(doc.URL)
	isUniversitySite := parsed != nil && strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".edu.cn")
	strongCampusForward := containsAny(title, []string{"关于组织学生参加", "关于组织我校学生参加", "关于组织同学参加", "关于组织参加"})
	campusStyleTitle := strongCampusForward ||
		(containsAny(title, []string{"报名通知", "延期通知"}) && containsAny(title, []string{"大学", "学院", "教务部", "教务处", "研究生院"}))
	if isUniversitySite {
		if strongCampusForward {
			return "campus-internal forwarding announcement"
		}
		if (containsAny(title+" "+text, campusInternalMarkers) || campusStyleTitle) && !containsAny(text, publicParticipationMarkers) {
			return "campus-internal announcement"
		}
	}
	return ""
}

// canUsePublicCampusRulesFallback reports whether a page that the AI
// classification rejected as campus-internal/forwarding may still keep its
// deterministic ruleAnalysis facts. It is deliberately narrow: it fires only
// for campus-like rejections of a strongly computer-competition-related,
// non-low-trust page whose text shows an explicit public participation scope,
// no campus-internal markers, and at least one complete dated range. It never
// re-runs Enrich and never promotes trust or source role.
func (a *Analyzer) canUsePublicCampusRulesFallback(candidate model.Candidate, doc model.Document, competition model.Competition, classification AIClassification) bool {
	// A. Low-trust sources never fall back.
	if competition.Trust == model.TrustLow {
		return false
	}
	// B. The candidate/doc must explicitly match the deterministic focus list
	// (the project's curated computer/CS competition inventory). This replaces
	// reliance on the AI's free-form computer_related output, which is unstable
	// for e.g. math-modelling competitions the focus list already covers.
	if !containsAny(candidate.Title+" "+doc.Title+" "+doc.Text, a.focus) {
		return false
	}
	// C. Only campus-like misclassification may be rescued. Listing,
	// post-event-news and community pages never fall back, even with dates.
	if classification.SourceRole != SourceCampusForward && classification.DocumentType != DocumentCampusInternal {
		return false
	}
	switch classification.DocumentType {
	case DocumentListing, DocumentPostEventNews, DocumentCommunity:
		return false
	}
	// D. The candidate must be strongly competition-related deterministically.
	if competition.FitScore < 60 {
		return false
	}
	// E. The page must not carry any campus-internal evidence. This check is
	// repeated here as the safety boundary even though lowValueReason already
	// ran earlier.
	scope := candidate.Title + " " + doc.Title + " " + doc.Text
	if containsAny(scope, campusInternalMarkers) || containsAny(scope, explicitCampusCompetitionMarkers) {
		return false
	}
	// F. An explicit public participation scope is required. A bare 全国 or a
	// competition-name qualifier (全国大学生/中国研究生/全国研究生) is not enough.
	if !containsAny(scope, publicCampusFallbackMarkers) {
		return false
	}
	// G. At least one complete dated range is required; a single date is not
	// enough. This keeps the PR #16 range-year safety intact.
	regComplete := competition.RegistrationStart != nil && competition.RegistrationEnd != nil
	compComplete := competition.CompetitionStart != nil && competition.CompetitionEnd != nil
	return regComplete || compComplete
}

func (a *Analyzer) ruleAnalysis(candidate model.Candidate, doc model.Document, trust model.Trust, text string, score int, now time.Time) model.Competition {
	name := cleanTitle(firstNonEmpty(doc.Title, candidate.Title))
	organizer := extractOrganizer(text)
	status, evidence := detectStatus(text)
	start, startRaw, end, endRaw := extractDates(text, a.location)
	competitionStart, competitionStartRaw, competitionEnd, competitionEndRaw := extractCompetitionDates(text, a.location)
	if end != nil && end.Before(model.DayStart(now.In(a.location))) && status == model.StatusRegistrationOpen {
		status = model.StatusRegistrationClosed
	}
	if end == nil && status == model.StatusRegistrationOpen {
		candidates := registrationEndDates(text, a.location)
		if len(candidates) > 0 {
			allPast := true
			for _, candidateEnd := range candidates {
				if !candidateEnd.Before(model.DayStart(now.In(a.location))) {
					allPast = false
					break
				}
			}
			if allPast {
				status = model.StatusRegistrationClosed
			}
		}
	}
	team, teamEvidence := extractTeam(text)
	fee, feeEvidence := extractFee(text)
	content, contentEvidence := extractContent(text, a.positive)

	fitReason := recommendationReason(text)
	eligibility := ""
	if strings.Contains(text, "本科生") && containsAny(name+" "+text, a.focus) {
		eligibility = "可能不符合参赛资格：原文提到参赛对象为本科生，请以官方规则为准。"
		score = max(0, score-20)
	}
	if status == "" {
		status = model.StatusUnknown
	}
	if status == model.StatusRegistrationOpen && end == nil {
		// An undated "报名中" found somewhere in a page is not enough. The
		// rules-only fallback accepts it only when the evidence is explicitly
		// tied to the current (or a future) edition. The AI path additionally
		// accepts a recent, evidenced publication date.
		if year := YearFromText(name + " " + evidence); year == 0 || year < now.In(a.location).Year() {
			status = model.StatusUnknown
			evidence = ""
		}
	}
	problemReleased := containsAny(text, []string{"赛题已发布", "赛题正式发布", "赛题现已发布"}) || (strings.Contains(text, "赛题发布") && !strings.Contains(text, "赛题发布会"))
	registrationPhase, competitionPhase := model.PhasesForLegacyStatus(status)
	edition := ""
	if year := YearFromText(name + " " + evidence); year != 0 {
		edition = fmt.Sprintf("%d", year)
	}
	facts := make(map[string]model.FactEvidence)
	putFact(facts, model.FactOrganizer, organizer, organizer, findEvidence(text, []string{"主办方", "主办单位", "主办机构"}), edition, doc.URL, trust, doc.PublishedAtRaw, now)
	putFact(facts, model.FactRegistrationStart, startRaw, startRaw, findEvidence(text, []string{startRaw}), edition, doc.URL, trust, doc.PublishedAtRaw, now)
	putFact(facts, model.FactRegistrationEnd, endRaw, endRaw, findEvidence(text, []string{endRaw}), edition, doc.URL, trust, doc.PublishedAtRaw, now)
	putFact(facts, model.FactCompetitionStart, competitionStartRaw, competitionStartRaw, findEvidence(text, []string{competitionStartRaw}), edition, doc.URL, trust, doc.PublishedAtRaw, now)
	putFact(facts, model.FactCompetitionEnd, competitionEndRaw, competitionEndRaw, findEvidence(text, []string{competitionEndRaw}), edition, doc.URL, trust, doc.PublishedAtRaw, now)
	putFact(facts, model.FactTeamRequirement, team, team, teamEvidence, edition, doc.URL, trust, doc.PublishedAtRaw, now)
	putFact(facts, model.FactFee, fee, fee, feeEvidence, edition, doc.URL, trust, doc.PublishedAtRaw, now)
	putFact(facts, model.FactContent, content, content, contentEvidence, edition, doc.URL, trust, doc.PublishedAtRaw, now)
	putFact(facts, model.FactEligibility, eligibility, eligibility, findEvidence(text, []string{"本科生", "研究生", "参赛对象"}), edition, doc.URL, trust, doc.PublishedAtRaw, now)
	putFact(facts, model.FactPublishedAt, doc.PublishedAtRaw, doc.PublishedAtRaw, doc.PublishedAtRaw, edition, doc.URL, trust, doc.PublishedAtRaw, now)
	if registrationPhase != model.RegistrationUnknown {
		putFact(facts, model.FactRegistrationState, string(registrationPhase), string(registrationPhase), evidence, edition, doc.URL, trust, doc.PublishedAtRaw, now)
	}
	if competitionPhase != model.CompetitionUnknown {
		putFact(facts, model.FactCompetitionState, string(competitionPhase), string(competitionPhase), evidence, edition, doc.URL, trust, doc.PublishedAtRaw, now)
	}
	competition := model.Competition{
		EntityKey:            EntityKey(name, organizer),
		Name:                 name,
		Organizer:            organizer,
		Status:               status,
		StatusEvidence:       evidence,
		RegistrationPhase:    registrationPhase,
		CompetitionPhase:     competitionPhase,
		RegistrationStart:    start,
		RegistrationStartRaw: startRaw,
		RegistrationEnd:      end,
		RegistrationEndRaw:   endRaw,
		CompetitionStart:     competitionStart,
		CompetitionStartRaw:  competitionStartRaw,
		CompetitionEnd:       competitionEnd,
		CompetitionEndRaw:    competitionEndRaw,
		TeamRequirement:      team,
		Fee:                  fee,
		FeeEvidence:          feeEvidence,
		Keywords:             extractKeywords(text),
		Content:              content,
		FitScore:             min(100, max(0, score)),
		FitReason:            fitReason,
		EligibilityNote:      eligibility,
		OfficialURL:          doc.URL,
		Trust:                trust,
		ProblemReleased:      problemReleased,
		Facts:                facts,
	}
	model.NormalizeLifecycle(&competition)
	return competition
}

func (a *Analyzer) mergeAI(base model.Competition, result AIResult, doc model.Document, now time.Time) model.Competition {
	base.FitScore = min(100, max(base.FitScore, result.FitScore))
	if result.Recommendation.Value != "" {
		base.FitReason = result.Recommendation.Value
	}
	if result.Identity.Name.Value != "" {
		base.Name = result.Identity.Name.Value
	}
	// Page-wide regex extraction is useful as a fallback when no model is
	// configured, but it is not edition-safe. Once a validated v2 result is
	// available, only its evidenced facts may populate these fields. The one
	// deliberate exception is the narrow deterministic competition day-only
	// range (explicit full start, same-month bare-day end): it is strong,
	// unambiguous evidence that survives an AI merge only when the model omits
	// competition dates entirely. All other rule-extracted dates are cleared.
	savedCompStart, savedCompStartRaw, savedCompEnd, savedCompEndRaw := base.CompetitionStart, base.CompetitionStartRaw, base.CompetitionEnd, base.CompetitionEndRaw
	preserveDayOnlyComp := isNarrowDayOnlyCompetitionRange(savedCompStart, savedCompEnd, savedCompStartRaw, savedCompEndRaw)
	// Preserve the original deterministic FactEvidence for the narrow range so
	// it can be restored verbatim (including its page-evidence snippet and
	// SourceURL/trust metadata) rather than rebuilt from a bare-day raw value.
	savedCompStartFact, savedCompStartFactOK := base.Facts[model.FactCompetitionStart]
	savedCompEndFact, savedCompEndFactOK := base.Facts[model.FactCompetitionEnd]
	base.Organizer = ""
	base.RegistrationStart = nil
	base.RegistrationStartRaw = ""
	base.RegistrationEnd = nil
	base.RegistrationEndRaw = ""
	base.CompetitionStart = nil
	base.CompetitionStartRaw = ""
	base.CompetitionEnd = nil
	base.CompetitionEndRaw = ""
	base.TeamRequirement = ""
	base.Fee = ""
	base.FeeEvidence = ""
	base.EligibilityNote = ""
	base.Facts = make(map[string]model.FactEvidence)
	if result.Identity.Organizer.Value != "" {
		base.Organizer = result.Identity.Organizer.Value
	}
	if result.Facts.TeamRequirement.Value != "" {
		base.TeamRequirement = result.Facts.TeamRequirement.Value
	}
	if result.Facts.Fee.Value != "" {
		base.Fee = result.Facts.Fee.Value
		base.FeeEvidence = result.Facts.Fee.Evidence
	}
	if result.Facts.CompetitionContents.Value != "" {
		base.Content = result.Facts.CompetitionContents.Value
	}
	if result.Facts.Eligibility.Value != "" {
		base.EligibilityNote = result.Facts.Eligibility.Value
	}
	if result.Facts.RegistrationStart.Value != "" {
		if parsed := parseDate(result.Facts.RegistrationStart.Value, a.location); parsed != nil {
			base.RegistrationStart = parsed
			base.RegistrationStartRaw = result.Facts.RegistrationStart.Value
		}
	}
	if result.Facts.RegistrationEnd.Value != "" {
		if parsed := parseDate(result.Facts.RegistrationEnd.Value, a.location); parsed != nil {
			base.RegistrationEnd = parsed
			base.RegistrationEndRaw = result.Facts.RegistrationEnd.Value
		}
	}
	if result.Facts.CompetitionStart.Value != "" {
		if parsed := parseDate(result.Facts.CompetitionStart.Value, a.location); parsed != nil {
			base.CompetitionStart = parsed
			base.CompetitionStartRaw = result.Facts.CompetitionStart.Value
		}
	}
	if result.Facts.CompetitionEnd.Value != "" {
		if parsed := parseDate(result.Facts.CompetitionEnd.Value, a.location); parsed != nil {
			base.CompetitionEnd = parsed
			base.CompetitionEndRaw = result.Facts.CompetitionEnd.Value
		}
	}
	putAIFact(base.Facts, model.FactOrganizer, result.Identity.Organizer, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	putAIFact(base.Facts, model.FactRegistrationStart, result.Facts.RegistrationStart, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	putAIFact(base.Facts, model.FactRegistrationEnd, result.Facts.RegistrationEnd, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	putAIFact(base.Facts, model.FactCompetitionStart, result.Facts.CompetitionStart, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	putAIFact(base.Facts, model.FactCompetitionEnd, result.Facts.CompetitionEnd, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	// When the model accepted the page but did not provide any competition
	// dates, the narrow deterministic day-only range (validated in
	// ruleAnalysis) is safe to keep. AI-validated competition dates always win;
	// the range is restored only when the model gave none. Registration dates
	// and every other rule-extracted fact are still cleared above.
	if preserveDayOnlyComp && base.CompetitionStart == nil && base.CompetitionEnd == nil {
		base.CompetitionStart = savedCompStart
		base.CompetitionStartRaw = savedCompStartRaw
		base.CompetitionEnd = savedCompEnd
		base.CompetitionEndRaw = savedCompEndRaw
		// Restore the original deterministic FactEvidence verbatim so the
		// page-evidence snippet, SourceURL and trust metadata survive the merge.
		// AI-validated competition dates (handled above) still take precedence.
		if savedCompStartFactOK {
			base.Facts[model.FactCompetitionStart] = savedCompStartFact
		}
		if savedCompEndFactOK {
			base.Facts[model.FactCompetitionEnd] = savedCompEndFact
		}
	}
	putAIFact(base.Facts, model.FactTeamRequirement, result.Facts.TeamRequirement, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	putAIFact(base.Facts, model.FactFee, result.Facts.Fee, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	putAIFact(base.Facts, model.FactEligibility, result.Facts.Eligibility, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	putAIFact(base.Facts, model.FactContent, result.Facts.CompetitionContents, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	putAIFact(base.Facts, model.FactPublishedAt, result.Facts.PublishedAt, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	if _, exists := base.Facts[model.FactPublishedAt]; !exists && doc.PublishedAtRaw != "" {
		putFact(base.Facts, model.FactPublishedAt, doc.PublishedAtRaw, doc.PublishedAtRaw, doc.PublishedAtRaw, result.Identity.Edition.Value, doc.URL, base.Trust, doc.PublishedAtRaw, now)
	}
	registrationPhase, competitionPhase, evidence := phasesFromAIEvents(model.RegistrationUnknown, model.CompetitionUnknown, result.Events)
	base.RegistrationPhase = registrationPhase
	base.CompetitionPhase = competitionPhase
	base.StatusEvidence = evidence
	for _, event := range result.Events {
		if event.Type == AIEventProblemReleased {
			base.ProblemReleased = true
		}
		switch event.Type {
		case AIEventPreviewed, AIEventRegistrationAnnounced:
			if registrationPhase == model.RegistrationPreview {
				putFact(base.Facts, model.FactRegistrationState, string(registrationPhase), string(registrationPhase), event.Evidence, event.Edition, doc.URL, base.Trust, doc.PublishedAtRaw, now)
			}
		case AIEventRegistrationOpened:
			if registrationPhase == model.RegistrationOpen {
				putFact(base.Facts, model.FactRegistrationState, string(registrationPhase), string(registrationPhase), event.Evidence, event.Edition, doc.URL, base.Trust, doc.PublishedAtRaw, now)
			}
		case AIEventRegistrationClosed:
			if registrationPhase == model.RegistrationClosed {
				putFact(base.Facts, model.FactRegistrationState, string(registrationPhase), string(registrationPhase), event.Evidence, event.Edition, doc.URL, base.Trust, doc.PublishedAtRaw, now)
			}
		case AIEventCompetitionUpcoming:
			if competitionPhase == model.CompetitionUpcoming {
				putFact(base.Facts, model.FactCompetitionState, string(competitionPhase), string(competitionPhase), event.Evidence, event.Edition, doc.URL, base.Trust, doc.PublishedAtRaw, now)
			}
		case AIEventCompetitionStarted:
			if competitionPhase == model.CompetitionOngoing {
				putFact(base.Facts, model.FactCompetitionState, string(competitionPhase), string(competitionPhase), event.Evidence, event.Edition, doc.URL, base.Trust, doc.PublishedAtRaw, now)
			}
		case AIEventCompetitionFinished:
			if competitionPhase == model.CompetitionFinished {
				putFact(base.Facts, model.FactCompetitionState, string(competitionPhase), string(competitionPhase), event.Evidence, event.Edition, doc.URL, base.Trust, doc.PublishedAtRaw, now)
			}
		}
	}
	if base.RegistrationStart != nil && base.RegistrationEnd != nil && base.RegistrationEnd.Before(*base.RegistrationStart) {
		base.RegistrationStart, base.RegistrationEnd = nil, nil
		base.RegistrationStartRaw, base.RegistrationEndRaw = "", ""
		delete(base.Facts, model.FactRegistrationStart)
		delete(base.Facts, model.FactRegistrationEnd)
		if base.RegistrationPhase == model.RegistrationOpen {
			base.RegistrationPhase, base.StatusEvidence = model.RegistrationUnknown, ""
			delete(base.Facts, model.FactRegistrationState)
		}
	}
	if base.CompetitionStart != nil && base.CompetitionEnd != nil && base.CompetitionEnd.Before(*base.CompetitionStart) {
		base.CompetitionStart, base.CompetitionEnd = nil, nil
		base.CompetitionStartRaw, base.CompetitionEndRaw = "", ""
		delete(base.Facts, model.FactCompetitionStart)
		delete(base.Facts, model.FactCompetitionEnd)
		if base.CompetitionPhase != model.CompetitionUnknown {
			base.CompetitionPhase, base.StatusEvidence = model.CompetitionUnknown, ""
			delete(base.Facts, model.FactCompetitionState)
		}
	}
	if base.RegistrationPhase == model.RegistrationOpen && base.RegistrationStart != nil && base.RegistrationStart.After(now.In(a.location).Add(24*time.Hour)) {
		base.RegistrationPhase, base.StatusEvidence = model.RegistrationUnknown, ""
		delete(base.Facts, model.FactRegistrationState)
	}
	if base.RegistrationEnd != nil && base.RegistrationEnd.Before(model.DayStart(now.In(a.location))) && base.RegistrationPhase == model.RegistrationOpen {
		base.RegistrationPhase = model.RegistrationClosed
		if fact, exists := base.Facts[model.FactRegistrationState]; exists {
			fact.Value, fact.Raw = string(model.RegistrationClosed), string(model.RegistrationClosed)
			base.Facts[model.FactRegistrationState] = fact
		}
	}
	// Status is derived solely from the AI-validated phases, never from the
	// rule-based fallback. Clearing it before NormalizeLifecycle prevents a
	// legacy status from resurrecting phases when events were withheld (e.g. on
	// a partially analyzed result).
	base.Status = model.StatusUnknown
	model.NormalizeLifecycle(&base)
	base.EntityKey = EntityKey(base.Name, base.Organizer)
	return base
}

func TrustForURL(raw string, source config.Source, cfg config.Config) model.Trust {
	parsed, err := url.Parse(raw)
	if err != nil {
		return model.TrustLow
	}
	host := strings.ToLower(parsed.Hostname())
	if domainMatch(host, cfg.HighDomains) {
		return model.TrustHigh
	}
	if domainMatch(host, cfg.MediumDomains) || strings.HasSuffix(host, ".edu.cn") || strings.HasSuffix(host, ".gov.cn") {
		return model.TrustMedium
	}
	if source.Kind != "search" {
		switch source.Trust {
		case "high":
			return model.TrustHigh
		case "medium":
			return model.TrustMedium
		}
	}
	return model.TrustLow
}

func EntityKey(name, organizer string) string {
	base := strings.ToLower(name + "|" + organizer)
	for _, noise := range []string{"报名通知", "开始报名", "报名开始", "正式开放报名", "即将启动", "敬请期待", "报名预告", "启动仪式", "赛题发布会", "官网"} {
		base = strings.ReplaceAll(base, strings.ToLower(noise), "")
	}
	var builder strings.Builder
	for _, r := range base {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	if builder.Len() == 0 {
		sum := sha256.Sum256([]byte(base))
		return hex.EncodeToString(sum[:12])
	}
	return builder.String()
}

func extractDates(text string, loc *time.Location) (*time.Time, string, *time.Time, string) {
	start, startRaw, end, endRaw := extractDateRange(text, loc, dateRangePattern, startPattern, endPattern)
	uniqueEnds := looseRegistrationEnds(text, loc)
	switch len(uniqueEnds) {
	case 1:
		end, endRaw = uniqueEnds[0].When, uniqueEnds[0].Raw
	case 0:
		// no loose deadline found; keep whatever the strict patterns produced
	default:
		// Conflicting registration deadlines on one page are not safe enough
		// for countdown reminders. Keep the field unpublished until a later
		// source resolves the conflict.
		end, endRaw = nil, ""
	}
	return start, startRaw, end, endRaw
}

func extractCompetitionDates(text string, loc *time.Location) (*time.Time, string, *time.Time, string) {
	start, startRaw, end, endRaw := extractDateRange(text, loc, competitionRangePattern, competitionStartPattern, competitionEndPattern)
	if start != nil && end != nil {
		return start, startRaw, end, endRaw
	}
	// A competition range whose right-hand side only states a day ("25~26" /
	// "25日至26日" / "将于25~26日举行") inherits year and month from the
	// explicitly-dated left-hand side. This is competition-only: registration
	// dates are parsed by extractDates and never get this day-only fallback.
	if dStart, dRaw, dEnd, dEndRaw := extractCompetitionDayRange(text, loc); dStart != nil && dEnd != nil {
		return dStart, dRaw, dEnd, dEndRaw
	}
	return start, startRaw, end, endRaw
}

// extractCompetitionDayRange parses a competition range whose right-hand side
// states only a day. The left-hand side must be an explicit full year+month+day
// date (validated against the real calendar). The right-hand day inherits year
// and month from it, but only when it does not sort before the start day; a
// "30日~2日" pattern would imply a cross-month range and is rejected rather than
// guessed. If the right-hand day is followed by an explicit time ("26日18:00" /
// "26日18时") the whole fallback is rejected: a day-only end must not carry its
// own time and the left-hand time must never be inherited. The matched clause
// must also carry clear competition semantics and must not be registration-like
// (报名/注册/缴费/材料提交), so a "报名将于...~...进行" range is never mistaken
// for competition dates.
func extractCompetitionDayRange(text string, loc *time.Location) (*time.Time, string, *time.Time, string) {
	match := competitionDayRangePattern.FindStringSubmatchIndex(text)
	if len(match) != 8 {
		return nil, "", nil, ""
	}
	// match indices: [0,1]=full, [2,3]=start, [4,5]=day, [6,7]=right time.
	// The right-time group is optional and may not participate; its indices are
	// -1 then, which must be treated as "no explicit time".
	hasRightTime := match[6] >= 0 && match[7] >= 0 && strings.TrimSpace(text[match[6]:match[7]]) != ""
	if hasRightTime {
		// The right end wrote its own explicit time. This fallback does not
		// support day-only+time, so refuse rather than inherit the left time.
		return nil, "", nil, ""
	}
	// The local clause must be clearly a competition time, not registration.
	if !competitionDayRangeClauseOK(text, match[0], match[1]) {
		return nil, "", nil, ""
	}
	start := parseDate(text[match[2]:match[3]], loc)
	if start == nil {
		// The left-hand side must be an explicit, valid full date.
		return nil, "", nil, ""
	}
	startRaw := strings.TrimSpace(text[match[2]:match[3]])
	end := parseDayOnlyEnd(text[match[4]:match[5]], *start, loc)
	if end == nil || end.Before(*start) {
		return nil, "", nil, ""
	}
	return start, startRaw, end, strings.TrimSpace(text[match[4]:match[5]])
}

// competitionDayRangeClauseOK guards the day-only competition fallback against
// registration-like expressions. It inspects the sentence containing the
// matched date range: any registration / fee / material-submission marker
// excludes the clause, and a clear competition-semantic marker must be present.
// Merely having "比赛/竞赛" earlier in the sentence is not enough, because
// "比赛报名将于..." is still registration semantics.
func competitionDayRangeClauseOK(text string, startIdx, endIdx int) bool {
	clause := localClause(text, startIdx, endIdx)
	for _, marker := range competitionExclusionMarkers {
		if strings.Contains(clause, marker) {
			return false
		}
	}
	for _, marker := range competitionPositiveMarkers {
		if strings.Contains(clause, marker) {
			return true
		}
	}
	return false
}

// localClause returns the sentence containing the byte range [startIdx,endIdx),
// bounded by Chinese/full-width sentence delimiters and line breaks.
func localClause(text string, startIdx, endIdx int) string {
	left := startIdx
	for left > 0 {
		if strings.ContainsRune("。！？!?；;\n", rune(text[left-1])) {
			break
		}
		left--
	}
	right := endIdx
	for right < len(text) {
		if strings.ContainsRune("。！？!?；;\n", rune(text[right])) {
			break
		}
		right++
	}
	return text[left:right]
}

// extractDateRange parses a (start, end) date pair from text using a range
// pattern first, then falling back to individual start/end patterns. It is
// shared by registration and competition date extraction so the two paths
// cannot drift in parsing behavior.
//
// The left-hand date of a range must carry an explicit year. The right-hand
// date may omit the year, in which case it inherits the left-hand year so a
// page like "2026年6月1日8:00至9月19日17:00" resolves to the same edition.
// Inheriting the year is a range-only behaviour: standalone start/end/deadline
// patterns still require an explicit year and never guess. If inheriting the
// year would place the end before the start (a cross-year year-less range), the
// whole range is rejected rather than fabricating a wrapped date. The raw
// evidence strings always mirror the original page text verbatim.
func extractDateRange(text string, loc *time.Location, rangePattern, startPat, endPat *regexp.Regexp) (*time.Time, string, *time.Time, string) {
	if match := rangePattern.FindStringSubmatch(text); len(match) == 3 {
		start := parseDate(match[1], loc)
		if start == nil {
			// The left-hand side of a range must be an explicit full date.
			return nil, "", nil, ""
		}
		startRaw := strings.TrimSpace(match[1])
		end := parseDate(match[2], loc)
		if end == nil {
			// Right-hand side omitted the year; inherit it from the start.
			end = parseDateInheritYear(match[2], start.Year(), loc)
		}
		if end == nil {
			return nil, "", nil, ""
		}
		if end.Before(*start) {
			// A year-less end that lands before the start would imply a
			// wrapped/cross-year range. Refuse it instead of guessing.
			return nil, "", nil, ""
		}
		return start, startRaw, end, strings.TrimSpace(match[2])
	}
	var start, end *time.Time
	var startRaw, endRaw string
	if match := startPat.FindStringSubmatch(text); len(match) == 2 {
		start, startRaw = parseDate(match[1], loc), strings.TrimSpace(match[1])
	}
	if match := endPat.FindStringSubmatch(text); len(match) == 2 {
		end, endRaw = parseDate(match[1], loc), strings.TrimSpace(match[1])
	}
	return start, startRaw, end, endRaw
}

// parseDateInheritYear parses a date that may omit its year, filling in
// inheritYear when no explicit year is present. It returns nil when the raw
// text carries no usable month/day or the resulting date is invalid. It never
// guesses a year on its own: the caller always supplies the year to inherit.
func parseDateInheritYear(raw string, inheritYear int, loc *time.Location) *time.Time {
	parts := datePartsLaxPattern.FindStringSubmatch(raw)
	if len(parts) != 6 {
		return nil
	}
	year := inheritYear
	if parts[1] != "" {
		if _, err := fmt.Sscanf(parts[1], "%d", &year); err != nil {
			return nil
		}
	}
	values := make([]int, 4)
	for i := 2; i < len(parts); i++ {
		if parts[i] != "" {
			if _, err := fmt.Sscanf(parts[i], "%d", &values[i-2]); err != nil {
				return nil
			}
		}
	}
	month, day, hour, minute := values[0], values[1], values[2], values[3]
	if hour == 24 && minute == 0 {
		hour, minute = 23, 59
	}
	if year < 2000 || month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 {
		return nil
	}
	parsed := time.Date(year, time.Month(month), day, hour, minute, 0, 0, loc)
	if parsed.Year() != year || int(parsed.Month()) != month || parsed.Day() != day {
		return nil
	}
	return &parsed
}

// bareDayPattern matches a day-only raw value ("26" or "26日") with optional
// surrounding whitespace. It distinguishes the narrow day-only competition
// range from arbitrary page-wide regex dates (which carry an explicit month
// and/or year on the end).
var bareDayPattern = regexp.MustCompile(`^\s*\d{1,2}\s*日?\s*$`)

// competitionExclusionMarkers are the semantics that must never be treated as
// competition dates. "比赛报名" (competition registration) is still registration,
// so these markers are checked on the local clause regardless of a surrounding
// 比赛/竞赛 token.
var competitionExclusionMarkers = []string{
	"报名", "注册", "缴费", "交费", "报名费", "参赛缴费", "材料提交", "材料上传", "材料递交",
}

// competitionPositiveMarkers establish that the local clause describes the
// competition time itself rather than a registration window. They include the
// explicit "比赛/竞赛/赛事时间/赛程" prefixes and competition venue/happening
// verbs such as 举行/举办/开幕/开赛/召开/总决赛.
var competitionPositiveMarkers = []string{
	"比赛时间", "竞赛时间", "赛事时间", "赛程", "举行", "举办", "开幕", "开赛", "召开", "总决赛", "决赛",
}

// isNarrowDayOnlyCompetitionRange reports whether the given competition date
// pair is exactly the narrow deterministic day-only range: an explicit full
// year+month+day start, a bare-day end (same month, valid order), with the end
// inheriting the start's year and month. Only this strong, unambiguous
// evidence is safe to preserve across an AI merge when the model omits
// competition dates; arbitrary page-wide regex dates and month+day ranges are
// excluded.
func isNarrowDayOnlyCompetitionRange(start, end *time.Time, startRaw, endRaw string) bool {
	if start == nil || end == nil {
		return false
	}
	// The end inherits the start's year+month; a different month means this is
	// not a same-month day-only range.
	if end.Year() != start.Year() || end.Month() != start.Month() {
		return false
	}
	// The start must carry an explicit full date (parseDate already validated
	// it), not a bare day. The end must be a bare day with no month.
	if bareDayPattern.MatchString(startRaw) || !bareDayPattern.MatchString(endRaw) {
		return false
	}
	// A valid, non-cross-month range.
	return !end.Before(*start)
}

// parseDayOnlyEnd parses the right-hand side of a competition range when it
// states only a day ("26" or "26日"), inheriting the year and month from the
// explicitly-dated start. It validates against the real calendar and refuses
// the range when the day sorts before the start day, so a cross-month pattern
// such as "4月30日~2日" is never guessed. The end time is always 00:00: it must
// never inherit the left-hand time, because the right side did not state one.
func parseDayOnlyEnd(raw string, start time.Time, loc *time.Location) *time.Time {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "日"))
	var day int
	if _, err := fmt.Sscanf(trimmed, "%d", &day); err != nil {
		return nil
	}
	if day < 1 || day > 31 || day < start.Day() {
		return nil
	}
	parsed := time.Date(start.Year(), start.Month(), day, 0, 0, 0, 0, loc)
	if parsed.Year() != start.Year() || int(parsed.Month()) != int(start.Month()) || parsed.Day() != day {
		return nil
	}
	return &parsed
}

type datedEvidence struct {
	When *time.Time
	Raw  string
}

// looseRegistrationEnds collects every registration deadline hint on the
// page using the loose pattern. The caller decides whether a single value
// can be trusted or conflicting values must be discarded.
func looseRegistrationEnds(text string, loc *time.Location) []datedEvidence {
	seen := map[int64]bool{}
	var result []datedEvidence
	for _, match := range looseEndPattern.FindAllStringSubmatch(text, -1) {
		if len(match) != 2 {
			continue
		}
		parsed := parseDate(match[1], loc)
		if parsed == nil || seen[parsed.Unix()] {
			continue
		}
		seen[parsed.Unix()] = true
		result = append(result, datedEvidence{When: parsed, Raw: strings.TrimSpace(match[1])})
	}
	return result
}

func registrationEndDates(text string, loc *time.Location) []*time.Time {
	entries := looseRegistrationEnds(text, loc)
	result := make([]*time.Time, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.When)
	}
	return result
}

func parseDate(raw string, loc *time.Location) *time.Time {
	parts := datePartsPattern.FindStringSubmatch(raw)
	if len(parts) != 6 {
		return nil
	}
	values := make([]int, 5)
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			_, _ = fmt.Sscanf(parts[i], "%d", &values[i-1])
		}
	}
	year, month, day, hour, minute := values[0], values[1], values[2], values[3], values[4]
	if hour == 24 && minute == 0 {
		hour, minute = 23, 59
	}
	if year < 2000 || month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 {
		return nil
	}
	parsed := time.Date(year, time.Month(month), day, hour, minute, 0, 0, loc)
	if parsed.Year() != year || int(parsed.Month()) != month || parsed.Day() != day {
		return nil
	}
	return &parsed
}

func detectStatus(text string) (model.Status, string) {
	if evidence := findEvidence(text, []string{"比赛已结束", "赛事已结束", "圆满结束", "圆满落幕", "圆满收官", "大赛收官", "赛事收官", "已完赛"}); evidence != "" {
		return model.StatusFinished, evidence
	}
	if evidence := findEvidence(text, []string{"正式开赛", "比赛正式开始", "赛事正式开始", "比赛现已开始", "赛事现已开始"}); evidence != "" {
		return model.StatusOngoing, evidence
	}
	if evidence := findEvidence(text, []string{"即将开赛", "即将开始比赛", "过几天开赛", "几天后开赛", "数日后开赛", "开赛倒计时", "距离开赛", "将于近日开赛"}); evidence != "" {
		return model.StatusUpcoming, evidence
	}
	if match := upcomingPattern.FindString(text); match != "" {
		return model.StatusUpcoming, match
	}
	if evidence := findEvidence(text, []string{"报名已截止", "报名结束", "停止报名"}); evidence != "" {
		return model.StatusRegistrationClosed, evidence
	}
	if evidence := findEvidence(text, []string{"报名通道已开启", "报名已经开始", "报名已开始", "开始报名", "报名中", "现已开放报名", "正式开放报名", "即日起"}); evidence != "" {
		return model.StatusRegistrationOpen, evidence
	}
	if evidence := findEvidence(text, []string{"即将启动", "敬请期待", "报名预告", "新一届赛事", "启动仪式", "赛题发布会", "预约报名", "即将开放报名", "预计开放报名", "将于"}); evidence != "" {
		return model.StatusPreview, evidence
	}
	if evidence := findEvidence(text, []string{"开放报名"}); evidence != "" {
		return model.StatusRegistrationOpen, evidence
	}
	if evidence := findEvidence(text, []string{"比赛进行中", "赛事进行中"}); evidence != "" {
		return model.StatusOngoing, evidence
	}
	return "", ""
}

func extractOrganizer(text string) string {
	match := organizerPattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func extractTeam(text string) (string, string) {
	for _, marker := range []string{"可单人参赛", "单人参赛", "组队参赛", "团队参赛", "每队", "每支队伍"} {
		if evidence := findEvidence(text, []string{marker}); evidence != "" {
			return evidence, evidence
		}
	}
	return "", ""
}

func extractFee(text string) (string, string) {
	for _, marker := range []string{"免费报名", "不收取报名费", "免报名费", "参赛免费", "报名费为0元", "报名费为 0 元"} {
		if evidence := findEvidence(text, []string{marker}); evidence != "" {
			return "免费", evidence
		}
	}
	match := feePattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return "", ""
	}
	evidence := findEvidence(text, []string{match[0]})
	if evidence == "" {
		return "", ""
	}
	return strings.TrimSpace(match[1]), evidence
}

func extractContent(text string, keywords []string) (string, string) {
	for _, keyword := range keywords {
		if evidence := findEvidence(text, []string{keyword}); evidence != "" {
			return evidence, evidence
		}
	}
	return "", ""
}

func findEvidence(text string, markers []string) string {
	lower := strings.ToLower(text)
	for _, marker := range markers {
		index := strings.Index(lower, strings.ToLower(marker))
		if index < 0 {
			continue
		}
		runes := []rune(text)
		runeIndex := len([]rune(text[:index]))
		start, end := runeIndex, runeIndex
		for start > 0 && !strings.ContainsRune("。！？!?；;\n", runes[start-1]) && runeIndex-start < 80 {
			start--
		}
		for end < len(runes) && !strings.ContainsRune("。！？!?；;\n", runes[end]) && end-runeIndex < 160 {
			end++
		}
		return strings.TrimSpace(string(runes[start:end]))
	}
	return ""
}

func recommendationReason(text string) string {
	pairs := []struct {
		Terms  []string
		Reason string
	}{
		{[]string{"AI Agent", "智能体", "RAG", "大模型"}, "贴合 AI Agent、RAG 与大模型应用方向，可形成项目型实践经历。"},
		{[]string{"Go后端", "Go 后端", "后端开发", "云原生", "云计算", "Kubernetes"}, "贴合 Go 后端、云计算和云原生方向，适合作为工程实践经历。"},
		{[]string{"程序设计", "算法", "CCF CSP", "CCPC", "ICPC"}, "能检验算法与编程基础，对技术实习简历有帮助。"},
		{[]string{"黑客松", "Hackathon", "软件开发", "开发者大赛"}, "强调软件开发和快速交付，适合沉淀可展示的参赛项目。"},
	}
	for _, pair := range pairs {
		if containsAny(text, pair.Terms) {
			return pair.Reason
		}
	}
	return "与计算机学习和软件实践相关，可根据官方规则评估投入产出。"
}

func extractKeywords(text string) []string {
	groups := []struct {
		Tag     string
		Markers []string
	}{
		{"AI Agent", []string{"AI Agent", "智能体", "Agent应用"}},
		{"RAG", []string{"RAG", "检索增强生成"}},
		{"大模型应用", []string{"大模型", "LLM", "生成式人工智能"}},
		{"人工智能", []string{"人工智能", "机器学习", "深度学习", "计算机视觉", "自然语言处理"}},
		{"Go", []string{"Golang", "Go语言", "Go 语言", "Go后端", "Go 后端"}},
		{"后端开发", []string{"后端", "服务端", "微服务", "API开发", "API 开发"}},
		{"云计算", []string{"云计算", "公有云", "云服务"}},
		{"云原生", []string{"云原生", "Kubernetes", "K8s", "容器化", "Service Mesh"}},
		{"软件开发", []string{"软件开发", "应用开发", "移动应用", "Web开发", "Web 开发"}},
		{"算法", []string{"算法", "程序设计", "ACM", "CCPC", "ICPC", "CCF CSP"}},
		{"黑客松", []string{"黑客松", "Hackathon"}},
		{"网络安全", []string{"网络安全", "信息安全", "CTF", "攻防"}},
		{"HarmonyOS", []string{"HarmonyOS", "鸿蒙"}},
		{"昇腾", []string{"昇腾", "Ascend"}},
		{"开源", []string{"开源", "Open Source"}},
		{"物联网", []string{"物联网", "IoT", "嵌入式"}},
	}
	var result []string
	for _, group := range groups {
		if containsAny(text, group.Markers) {
			result = append(result, group.Tag)
		}
	}
	return unique(result)
}

func cleanTitle(value string) string {
	value = strings.TrimSpace(value)
	for _, separator := range []string{" | ", " - ", "_"} {
		if parts := strings.Split(value, separator); len(parts) > 1 && len([]rune(parts[0])) >= 6 {
			value = parts[0]
			break
		}
	}
	return value
}

func normalize(value string) string { return strings.Join(strings.Fields(value), " ") }

func putAIFact(facts map[string]model.FactEvidence, key string, fact AIFact, sourceURL string, trust model.Trust, publishedAt string, observedAt time.Time) {
	if fact.Value == "" {
		return
	}
	putFact(facts, key, fact.Value, fact.Value, fact.Evidence, fact.Edition, sourceURL, trust, publishedAt, observedAt)
}

func putFact(facts map[string]model.FactEvidence, key, value, raw, evidence, edition, sourceURL string, trust model.Trust, publishedAt string, observedAt time.Time) {
	if facts == nil || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" || strings.TrimSpace(evidence) == "" {
		return
	}
	facts[key] = model.FactEvidence{
		Value:      strings.TrimSpace(value),
		Raw:        strings.TrimSpace(raw),
		Evidence:   normalize(evidence),
		Edition:    strings.TrimSpace(edition),
		SourceURL:  strings.TrimSpace(sourceURL),
		Confidence: computedFactConfidence(trust, evidence, edition, publishedAt),
		ObservedAt: observedAt,
	}
}

func buildAnalysisAudit(result AIResult, doc model.Document, now time.Time, modelName string, facts map[string]model.FactEvidence) model.AnalysisAudit {
	return model.AnalysisAudit{
		AnalyzerVersion: AIAnalyzerVersion,
		Model:           modelName,
		InputHash:       analysisInputHash(doc),
		SegmentIDs:      append([]string(nil), result.SegmentIDs...),
		RawResponses:    append([]string(nil), result.RawResponses...),
		AcceptedFields:  sortedFactKeys(facts),
		Rejections:      append([]model.AnalysisRejection(nil), result.Rejections...),
		AnalyzedAt:      now,
	}
}

func buildRuleAudit(doc model.Document, now time.Time, facts map[string]model.FactEvidence) model.AnalysisAudit {
	return model.AnalysisAudit{
		AnalyzerVersion: AIAnalyzerVersion,
		InputHash:       analysisInputHash(doc),
		AcceptedFields:  sortedFactKeys(facts),
		AnalyzedAt:      now,
	}
}

// buildCampusRulesFallbackAudit records a public-campus rules-only fallback:
// the AI classification was rejected (campus forwarding), no Enrich was called,
// and only the deterministic ruleAnalysis facts were accepted. It is not logged
// as an AI extraction success.
func buildCampusRulesFallbackAudit(competition model.Competition, doc model.Document, now time.Time, modelName string) model.AnalysisAudit {
	return model.AnalysisAudit{
		AnalyzerVersion: AIAnalyzerVersion,
		Model:           modelName,
		InputHash:       analysisInputHash(doc),
		RawResponses:    append([]string(nil), competition.ExtractionAudit.RawResponses...),
		AcceptedFields:  sortedFactKeys(competition.Facts),
		Rejections: []model.AnalysisRejection{{
			Field:  "document_type",
			Reason: "AI classification rejected the page as campus forwarding; deterministic public-campus rules fallback accepted evidenced facts.",
		}},
		AnalyzedAt: now,
	}
}

func analysisInputHash(doc model.Document) string {
	parts := []string{doc.URL, doc.Title, doc.PublishedAtRaw, doc.Text}
	for _, segment := range doc.Segments {
		parts = append(parts, segment.ID, segment.Text)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func sortedFactKeys(facts map[string]model.FactEvidence) []string {
	keys := make([]string, 0, len(facts))
	for key := range facts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// computedFactConfidence deliberately ignores the model's self-reported
// confidence. It is derived from independently verifiable properties.
func computedFactConfidence(trust model.Trust, evidence, edition, publishedAt string) string {
	score := 0
	switch trust {
	case model.TrustHigh:
		score += 3
	case model.TrustMedium:
		score += 2
	}
	if len([]rune(normalize(evidence))) >= 8 {
		score += 2
	}
	if strings.TrimSpace(edition) != "" {
		score++
	}
	if strings.TrimSpace(publishedAt) != "" {
		score++
	}
	if trust == model.TrustLow {
		return "low"
	}
	if trust == model.TrustMedium {
		if score >= 4 {
			return "medium"
		}
		return "low"
	}
	switch {
	case score >= 6:
		return "high"
	case score >= 4:
		return "medium"
	default:
		return "low"
	}
}

func containsAny(text string, terms []string) bool {
	lower := strings.ToLower(text)
	for _, term := range terms {
		if strings.Contains(lower, strings.ToLower(term)) {
			return true
		}
	}
	return false
}

// normalizeForMarkerMatch strips whitespace and both full-width and half-width
// brackets from text so deterministic marker matching is robust to punctuation
// like "（校级）选拔赛" (which would otherwise not contain the contiguous
// substring "校级选拔"). It is only used for marker detection, never for raw
// evidence storage.
func normalizeForMarkerMatch(text string) string {
	for _, sep := range []string{" ", "\t", "\n", "\r", "（", "）", "(", ")", "【", "】", "[", "]", "：", ":", "，", ",", "。", ".", "、"} {
		text = strings.ReplaceAll(text, sep, "")
	}
	return text
}

func domainMatch(host string, domains []string) bool {
	for _, domain := range domains {
		domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func unique(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value))
		if key != "" && !seen[key] {
			seen[key] = true
			result = append(result, strings.TrimSpace(value))
		}
	}
	sort.Strings(result)
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "暂未公布"
}

func YearFromText(text string) int {
	match := yearPattern.FindString(text)
	if match == "" {
		return 0
	}
	var year int
	_, _ = fmt.Sscanf(match, "%d", &year)
	return year
}
