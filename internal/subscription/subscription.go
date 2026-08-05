package subscription

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"competition-assistant/internal/model"
)

type Category struct {
	ID          string
	Name        string
	Description string
	Keywords    []string
}

type FilterOption struct {
	ID          string
	Name        string
	Description string
}

type Classification struct {
	Categories     []string
	OrganizerTypes []string
	Scopes         []string
	Regions        []string
}

var categories = []Category{
	{ID: "algorithm", Name: "算法竞赛", Description: "CSP、CCPC、ICPC、程序设计与算法", Keywords: []string{"ccf csp", "ccpc", "icpc", "acm", "算法", "程序设计", "编程竞赛"}},
	{ID: "ai_data", Name: "AI 与数据", Description: "AI Agent、RAG、大模型、机器学习与数据科学", Keywords: []string{"ai agent", "智能体", "rag", "大模型", "人工智能", "机器学习", "深度学习", "数据挖掘", "数据科学", "自然语言处理", "计算机视觉"}},
	{ID: "development", Name: "软件开发", Description: "后端、前端、移动应用、开源与工程实践", Keywords: []string{"go后端", "go 后端", "后端", "前端", "软件开发", "应用开发", "移动开发", "开源", "开发者大赛"}},
	{ID: "cloud_native", Name: "云计算与云原生", Description: "云平台、容器、Kubernetes 与 DevOps", Keywords: []string{"云计算", "云原生", "kubernetes", "docker", "devops", "容器", "微服务"}},
	{ID: "hackathon", Name: "黑客松", Description: "限时开发、企业命题和创新编程", Keywords: []string{"黑客松", "hackathon", "编程马拉松"}},
	{ID: "security", Name: "网络安全", Description: "CTF、安全攻防、密码学与漏洞分析", Keywords: []string{"ctf", "网络安全", "信息安全", "安全攻防", "密码学", "漏洞", "逆向"}},
	{ID: "iot_hardware", Name: "硬件与物联网", Description: "HarmonyOS、嵌入式、IoT、芯片与昇腾", Keywords: []string{"harmonyos", "鸿蒙", "嵌入式", "iot", "物联网", "芯片", "昇腾", "机器人"}},
	{ID: "innovation", Name: "创新创业", Description: "计算机相关创新项目与大学生创新赛事", Keywords: []string{"创新创业", "互联网+", "中国国际大学生创新大赛", "创新项目", "创业大赛"}},
	{ID: "general", Name: "综合计算机", Description: "其他计算机、信息技术与数字化赛事", Keywords: []string{"计算机", "信息技术", "数字技术", "ict", "软件", "编程"}},
}

var organizerTypes = []FilterOption{
	{ID: "enterprise", Name: "企业赛事", Description: "由大中小型科技企业或产业平台主办"},
	{ID: "government_society", Name: "政府与学协会", Description: "政府部门、学会、协会和基金会主办"},
	{ID: "university", Name: "高校公开赛事", Description: "高校主办但面向校外或多校开放"},
	{ID: "community", Name: "开源与技术社区", Description: "开源基金会、社区和开发者组织主办"},
	{ID: "unspecified", Name: "主办方待确认", Description: "来源可信，但主办方类型尚未明确"},
}

var competitionScopes = []FilterOption{
	{ID: "international", Name: "国际赛事", Description: "全球、国际或跨国范围"},
	{ID: "national", Name: "全国赛事", Description: "全国、中国区或面向全国高校"},
	{ID: "regional", Name: "地方赛事", Description: "省、市、区域或地方赛区赛事"},
	{ID: "online_open", Name: "线上开放赛事", Description: "不限地区的线上公开挑战"},
	{ID: "unspecified", Name: "范围待确认", Description: "官方暂未明确赛事覆盖范围"},
}

var regionNames = []string{
	"北京", "天津", "河北", "山西", "内蒙古", "辽宁", "吉林", "黑龙江", "上海", "江苏", "浙江", "安徽",
	"福建", "江西", "山东", "河南", "湖北", "湖南", "广东", "广西", "海南", "重庆", "四川", "贵州", "云南",
	"西藏", "陕西", "甘肃", "青海", "宁夏", "新疆", "香港", "澳门", "台湾",
}

var enterpriseDomains = []string{
	"huawei.com", "huaweicloud.com", "aliyun.com", "tianchi.aliyun.com", "cloud.tencent.com", "tencent.com",
	"baidu.com", "jd.com", "volcengine.com", "bytedance.com", "oppo.com", "vivo.com", "xiaomi.com", "meituan.com",
}

func Categories() []Category {
	result := make([]Category, len(categories))
	copy(result, categories)
	return result
}

func CategoryIDs() []string {
	result := make([]string, 0, len(categories))
	for _, category := range categories {
		result = append(result, category.ID)
	}
	return result
}

func ValidCategory(value string) bool {
	for _, category := range categories {
		if category.ID == value {
			return true
		}
	}
	return false
}

func OrganizerTypes() []FilterOption { return cloneOptions(organizerTypes) }

func CompetitionScopes() []FilterOption { return cloneOptions(competitionScopes) }

func OrganizerTypeIDs() []string { return optionIDs(organizerTypes) }

func CompetitionScopeIDs() []string { return optionIDs(competitionScopes) }

func ValidOrganizerType(value string) bool { return validOption(organizerTypes, value) }

func ValidCompetitionScope(value string) bool { return validOption(competitionScopes, value) }

func CategoryName(id string) string {
	for _, category := range categories {
		if category.ID == id {
			return category.Name
		}
	}
	return id
}

func OrganizerTypeName(id string) string { return optionName(organizerTypes, id) }

func CompetitionScopeName(id string) string { return optionName(competitionScopes, id) }

func Profile(competition model.Competition) Classification {
	text := strings.ToLower(strings.Join([]string{
		competition.Name,
		competition.Organizer,
		competition.Content,
		competition.FitReason,
		competition.StatusEvidence,
		strings.Join(competition.Keywords, " "),
		competition.Analysis.Summary,
		competition.Analysis.SuitableFor,
		strings.Join(competition.Analysis.Skills, " "),
	}, " "))
	result := Classification{}
	for _, category := range categories {
		if containsAny(text, category.Keywords) {
			result.Categories = append(result.Categories, category.ID)
		}
	}
	if len(result.Categories) == 0 {
		result.Categories = append(result.Categories, "general")
	}
	result.OrganizerTypes = classifyOrganizerTypes(competition, text)
	result.Regions = detectRegions(text)
	result.Scopes = classifyScopes(text, result.Regions)
	return result
}

// MatchingEventsForUser applies both profile filters and the per-user lifecycle
// of a competition. Undecided users only receive preview/registration notices;
// a start notice is reserved for users who explicitly chose to participate.
func MatchingEventsForUser(preferences model.UserPreferences, competition model.Competition, classification Classification, events []model.Event, decision model.ParticipationDecision, now time.Time) []model.Event {
	if !MatchesCompetition(preferences, competition, classification) {
		return nil
	}
	result := make([]model.Event, 0, len(events))
	for _, event := range events {
		enabled := event.Type == "competition_started" || eventEnabled(preferences, event.Type)
		if enabled && EventDeliverable(competition, event.Type, decision, now) {
			result = append(result, event)
		}
	}
	return result
}

func EventDeliverable(competition model.Competition, eventType string, decision model.ParticipationDecision, now time.Time) bool {
	model.NormalizeLifecycle(&competition)
	if decision == model.ParticipationDeclined || competition.CompetitionPhase == model.CompetitionFinished {
		return false
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if competition.CompetitionEnd != nil && competition.CompetitionEnd.In(now.Location()).Before(day) {
		return false
	}
	switch eventType {
	case "competition_discovered":
		return competition.RegistrationPhase == model.RegistrationUnknown && competition.CompetitionPhase == model.CompetitionUnknown
	case "preview_detected", "registration_opened":
		if decision != model.ParticipationParticipating && competition.RegistrationEnd != nil && competition.RegistrationEnd.In(now.Location()).Before(day) {
			return false
		}
		if eventType == "preview_detected" {
			return competition.RegistrationPhase == model.RegistrationPreview
		}
		return competition.RegistrationPhase == model.RegistrationOpen
	case "competition_upcoming":
		return decision == model.ParticipationParticipating && competition.CompetitionPhase == model.CompetitionUpcoming
	case "competition_started":
		return decision == model.ParticipationParticipating && competition.CompetitionPhase == model.CompetitionOngoing
	case "problem_released":
		return decision == model.ParticipationParticipating && competition.ProblemReleased
	default:
		return false
	}
}

func MatchesCompetition(preferences model.UserPreferences, competition model.Competition, classification Classification) bool {
	if !catalogEligible(competition) {
		return false
	}
	if trustRank(competition.Trust) < trustRank(preferences.MinTrust) {
		return false
	}
	if !preferences.AllowEligibilityRisk && strings.TrimSpace(competition.EligibilityNote) != "" {
		return false
	}
	if len(preferences.Categories) > 0 && !intersects(preferences.Categories, classification.Categories) {
		return false
	}
	if len(preferences.OrganizerTypes) > 0 && !intersects(preferences.OrganizerTypes, classification.OrganizerTypes) {
		return false
	}
	if len(preferences.CompetitionScopes) > 0 && !intersects(preferences.CompetitionScopes, classification.Scopes) {
		return false
	}
	if len(preferences.Regions) > 0 && contains(classification.Scopes, "regional") && !intersects(preferences.Regions, classification.Regions) {
		return false
	}
	haystack := strings.ToLower(strings.Join([]string{
		competition.Name,
		competition.Organizer,
		competition.Content,
		competition.FitReason,
		competition.TeamRequirement,
		strings.Join(competition.Keywords, " "),
		competition.Analysis.Summary,
		competition.Analysis.SuitableFor,
		strings.Join(competition.Analysis.Skills, " "),
	}, " "))
	if len(preferences.IncludeKeywords) > 0 && !matchesAnySearchKeyword(haystack, preferences.IncludeKeywords) {
		return false
	}
	if matchesAnySearchKeyword(haystack, preferences.ExcludeKeywords) {
		return false
	}
	return true
}

var searchKeywordAliases = [][]string{
	{"后端", "后端开发", "服务端", "backend", "server side"},
	{"ai agent", "aiagent", "智能体", "agent应用"},
	{"rag", "检索增强生成"},
	{"大模型", "llm", "大型语言模型", "生成式人工智能"},
	{"云原生", "cloud native", "cloudnative", "kubernetes", "k8s"},
	{"云计算", "cloud computing"},
	{"程序设计", "编程竞赛", "算法竞赛"},
	{"软件开发", "应用开发", "开发者大赛"},
	{"网络安全", "信息安全", "cybersecurity"},
	{"物联网", "iot"},
	{"鸿蒙", "harmonyos"},
	{"昇腾", "ascend"},
}

func matchesAnySearchKeyword(text string, keywords []string) bool {
	normalizedText := normalizeSearchText(text)
	for _, keyword := range keywords {
		normalizedKeyword := normalizeSearchText(keyword)
		if normalizedKeyword == "" {
			continue
		}
		if strings.Contains(normalizedText, normalizedKeyword) {
			return true
		}
		for _, aliases := range searchKeywordAliases {
			matchedGroup := false
			for _, alias := range aliases {
				if normalizedKeyword == normalizeSearchText(alias) {
					matchedGroup = true
					break
				}
			}
			if !matchedGroup {
				continue
			}
			for _, alias := range aliases {
				if strings.Contains(normalizedText, normalizeSearchText(alias)) {
					return true
				}
			}
		}
	}
	return false
}

func normalizeSearchText(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

var catalogPostEventMarkers = []string{
	"圆满落幕", "圆满结束", "圆满收官", "大赛收官", "赛事收官", "获奖名单", "获奖作品", "成绩公示",
	"结果公示", "入围名单", "晋级名单", "赛事回顾", "大赛回顾", "颁奖典礼", "成功举办", "决赛举行",
	"获奖公告", "获奖结果", "斩获", "获佳绩", "荣获", "夺得", "摘得", "全球总冠军", "喜报",
	"参赛作品信息核查", "参赛作品信息限时核查",
	"高分说", "经验分享", "选手专访", "赛后报道", "赛后回顾",
}

var catalogPublicMarkers = []string{"面向全国", "全国", "全市", "全省", "全球", "国际", "公开报名", "社会公众", "区域赛", "省赛", "市赛"}

func catalogEligible(competition model.Competition) bool {
	model.NormalizeLifecycle(&competition)
	title := strings.TrimSpace(competition.Name)
	text := strings.Join([]string{title, competition.Content, competition.StatusEvidence}, " ")
	if containsAny(title, catalogPostEventMarkers) || containsAny(title, []string{"竞赛动态 - 中国计算机学会", "竞赛动态-中国计算机学会", "竞赛公告列表", "赛事活动列表"}) {
		return false
	}
	if containsAny(text, []string{"校内赛", "校内比赛", "校内竞赛", "院内赛", "院内比赛", "院内竞赛", "校赛", "校区选拔", "校内选拔", "学校选拔赛", "校园选拔赛", "校级选拔", "院级选拔", "学院选拔赛", "本校赛", "本院赛", "仅限本院", "仅面向本院", "本学院学生", "我院学生", "报送至学院"}) {
		return false
	}
	if competition.Status == model.StatusPreview && strings.Contains(title, "总决赛") && !containsAny(title, []string{"即将", "预告", "敬请期待", "启动"}) {
		return false
	}
	if strings.Contains(title, "延期通知") && competition.RegistrationEnd == nil {
		return false
	}
	parsed, _ := url.Parse(competition.OfficialURL)
	isUniversitySite := parsed != nil && strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".edu.cn")
	if isUniversitySite {
		strongCampusTitle := containsAny(title, []string{"关于组织学生参加", "关于组织我校学生参加", "关于组织参加", "关于组织报名参加", "校内报名通知"})
		campusTitle := strongCampusTitle ||
			containsAny(title, []string{"报名通知", "竞赛通知", "参赛报名", "组织报名", "校赛", "选拔赛", "赛前培训"})
		if strongCampusTitle || (campusTitle && competition.Trust != model.TrustHigh) || (campusTitle && !containsAny(text, catalogPublicMarkers)) {
			return false
		}
	}
	if containsAny(title, []string{"公共政策案例分析", "社会治理案例"}) && !containsAny(title, []string{"人工智能", "算法", "程序设计", "软件", "计算机", "大模型"}) {
		return false
	}
	return true
}

// CatalogEligible is the shared final gate used both before delivery and when
// purging legacy rows created under older filtering rules.
func CatalogEligible(competition model.Competition) bool {
	return catalogEligible(competition)
}

func NormalizeRegions(values []string) ([]string, error) {
	normalized, err := NormalizeKeywords(values)
	if err != nil {
		return nil, err
	}
	valid := make(map[string]bool, len(regionNames))
	for _, region := range regionNames {
		valid[region] = true
	}
	result := make([]string, 0, len(normalized))
	for _, raw := range normalized {
		region := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(raw, "省"), "市"), "自治区")
		switch raw {
		case "内蒙古自治区":
			region = "内蒙古"
		case "广西壮族自治区":
			region = "广西"
		case "宁夏回族自治区":
			region = "宁夏"
		case "新疆维吾尔自治区":
			region = "新疆"
		case "西藏自治区":
			region = "西藏"
		}
		if !valid[region] {
			return nil, fmt.Errorf("暂不识别地区 %q，请填写省级行政区简称，例如“重庆、四川”", raw)
		}
		result = append(result, region)
	}
	sort.Strings(result)
	return result, nil
}

func classifyOrganizerTypes(competition model.Competition, text string) []string {
	var result []string
	host := ""
	if parsed, err := url.Parse(competition.OfficialURL); err == nil {
		host = strings.ToLower(parsed.Hostname())
	}
	if containsAny(text, []string{"有限公司", "集团", "科技公司", "云计算公司", "企业", "产业平台"}) || domainIn(host, enterpriseDomains) {
		result = append(result, "enterprise")
	}
	if containsAny(text, []string{"人民政府", "教育部", "工信部", "委员会", "计算机学会", "协会", "学会", "基金会"}) {
		result = append(result, "government_society")
	}
	if containsAny(text, []string{"大学", "学院", "高校", "教务处"}) {
		result = append(result, "university")
	}
	if containsAny(text, []string{"开源社区", "技术社区", "开发者社区", "开源基金会", "开源联盟", "社区主办"}) {
		result = append(result, "community")
	}
	if len(result) == 0 {
		result = append(result, "unspecified")
	}
	return uniqueStrings(result)
}

func classifyScopes(text string, regions []string) []string {
	switch {
	case containsAny(text, []string{"国际", "全球", "世界", "亚太", "international", "global"}):
		return []string{"international"}
	case containsAny(text, []string{"全国", "中国区", "全国高校", "国家级", "国赛"}):
		return []string{"national"}
	case len(regions) > 0 || containsAny(text, []string{"省赛", "市赛", "区域赛", "赛区", "地方赛"}):
		return []string{"regional"}
	case containsAny(text, []string{"线上", "在线", "不限地区", "所有开发者", "公开挑战"}):
		return []string{"online_open"}
	default:
		return []string{"unspecified"}
	}
}

func detectRegions(text string) []string {
	var result []string
	for _, region := range regionNames {
		if strings.Contains(text, strings.ToLower(region)) {
			result = append(result, region)
		}
	}
	return result
}

func cloneOptions(values []FilterOption) []FilterOption {
	result := make([]FilterOption, len(values))
	copy(result, values)
	return result
}

func optionIDs(values []FilterOption) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.ID)
	}
	return result
}

func validOption(values []FilterOption, id string) bool {
	for _, value := range values {
		if value.ID == id {
			return true
		}
	}
	return false
}

func optionName(values []FilterOption, id string) string {
	for _, value := range values {
		if value.ID == id {
			return value.Name
		}
	}
	return id
}

func domainIn(host string, domains []string) bool {
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func NextDelivery(now time.Time, preferences model.UserPreferences) (time.Time, error) {
	location, err := time.LoadLocation(preferences.Timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load user timezone %q: %w", preferences.Timezone, err)
	}
	now = now.In(location)
	if preferences.Frequency == model.DeliveryImmediate {
		return now, nil
	}
	hour, minute, err := parseClock(preferences.DeliveryTime)
	if err != nil {
		return time.Time{}, err
	}
	scheduled := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, location)
	switch preferences.Frequency {
	case model.DeliveryDaily:
		if !scheduled.After(now) {
			scheduled = scheduled.AddDate(0, 0, 1)
		}
	case model.DeliveryWeekly:
		days := (int(preferences.WeeklyDay) - int(now.Weekday()) + 7) % 7
		scheduled = scheduled.AddDate(0, 0, days)
		if !scheduled.After(now) {
			scheduled = scheduled.AddDate(0, 0, 7)
		}
	default:
		return time.Time{}, fmt.Errorf("unsupported delivery frequency %q", preferences.Frequency)
	}
	return scheduled, nil
}

func DeliveryGroupKey(userID int64, competitionID int64, frequency model.DeliveryFrequency, dueAt time.Time, nonce string) string {
	switch frequency {
	case model.DeliveryDaily, model.DeliveryWeekly:
		return fmt.Sprintf("user:%d:%s:%d", userID, frequency, dueAt.Unix())
	default:
		return fmt.Sprintf("user:%d:immediate:%d:%s", userID, competitionID, nonce)
	}
}

func NormalizeKeywords(values []string) ([]string, error) {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, raw := range values {
		for _, value := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == '\n' || r == '\r' || r == ',' || r == '，' || r == ';' || r == '；'
		}) {
			value = strings.TrimSpace(value)
			key := strings.ToLower(value)
			if key == "" || seen[key] {
				continue
			}
			if utf8.RuneCountInString(value) > 40 {
				return nil, fmt.Errorf("关键词 %q 超过 40 个字符", value)
			}
			seen[key] = true
			result = append(result, value)
			if len(result) > 20 {
				return nil, fmt.Errorf("关键词最多 20 个")
			}
		}
	}
	sort.Strings(result)
	return result, nil
}

func eventEnabled(preferences model.UserPreferences, event string) bool {
	switch event {
	case "competition_discovered":
		return preferences.NotifyImportantUpdate
	case "preview_detected":
		return preferences.NotifyPreview
	case "registration_opened":
		return preferences.NotifyRegistration
	case "competition_upcoming":
		return preferences.NotifyUpcoming
	case "competition_started":
		return preferences.NotifyStarted
	case "problem_released":
		return preferences.NotifyProblemRelease
	case "deadline_7d":
		return preferences.NotifyDeadline7Days
	case "deadline_1d":
		return preferences.NotifyDeadline1Day
	case "important_update":
		return preferences.NotifyImportantUpdate
	default:
		return false
	}
}

func parseClock(value string) (int, int, error) {
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid delivery time %q", value)
	}
	return parsed.Hour(), parsed.Minute(), nil
}

func intersects(left, right []string) bool {
	set := make(map[string]bool, len(left))
	for _, value := range left {
		set[value] = true
	}
	for _, value := range right {
		if set[value] {
			return true
		}
	}
	return false
}

func containsAny(text string, keywords []string) bool {
	for _, keyword := range keywords {
		if keyword != "" && strings.Contains(text, strings.ToLower(keyword)) {
			return true
		}
	}
	return false
}

func trustRank(trust model.Trust) int {
	switch trust {
	case model.TrustHigh:
		return 3
	case model.TrustMedium:
		return 2
	default:
		return 1
	}
}
