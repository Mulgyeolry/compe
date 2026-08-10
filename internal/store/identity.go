package store

import (
	"strings"
	"unicode"
)

// This file centralises the multi-stage competition identity rules used by the
// store's canonical-merge logic. The goal is to give a stable identity boundary
// for series/edition/stage/station so that:
//   - 第十届CCPC总决赛 and 第10届CCPC总决赛 collapse into one entity;
//   - 第10届 and 第11届 never merge;
//   - 郑州站 and 济南站 never merge;
//   - 分站 and 总决赛 never merge;
//   - a preview and the formal signup for the SAME series+edition+station merge.
//
// The rules are deliberately conservative: whenever a boundary component is
// unknown on one side we fall back to the existing name-similarity merge rather
// than guessing a value. They are also generic (no CCPC-specific strings), so
// they serve ICPC regionals, provincial contests, BlueBridge Cup sub-sites and
// other multi-stage competitions as well.

// editionDigitRunes maps a Chinese numeral rune to its digit value.
var chineseNumeral = map[rune]int{
	'一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
	'零': 0, '〇': 0,
}

// normalizeEditionOrdinal converts a Chinese-numeral or digit "第X届" fragment
// (or any text that contains it) into the canonical decimal ordinal string, or
// "" when the edition is absent / cannot be parsed unambiguously. It never
// guesses: "第〇届", mixed invalid forms and years alone all yield "".
func normalizeEditionOrdinal(text string) string {
	open := strings.Index(text, "第")
	closeIdx := strings.Index(text, "届")
	if open < 0 || closeIdx <= open {
		return ""
	}
	inside := text[open+len("第") : closeIdx]
	inside = strings.TrimSpace(inside)
	if inside == "" {
		return ""
	}
	// Pure digits.
	allDigits := true
	for _, r := range inside {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		// strip leading zeros
		v := strings.TrimLeft(inside, "0")
		if v == "" {
			return ""
		}
		return v
	}
	// Chinese numeral -> int, supporting 1..99.
	val, ok := parseChineseNumber(inside)
	if !ok {
		return ""
	}
	return itoa(val)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// parseChineseNumber parses a Chinese numeral between 一 and 九十九. Returns ok
// false for any ambiguous or out-of-range form (including 零/〇, over 99, or a
// bare 十 with no tens digit).
func parseChineseNumber(s string) (int, bool) {
	runes := []rune(s)
	n := len(runes)
	if n == 0 || n > 3 {
		return 0, false
	}
	// n==1: single digit 一..九, or 十 (10) in ordinal context.
	if n == 1 {
		if runes[0] == '十' {
			return 10, true
		}
		d, ok := chineseNumeral[runes[0]]
		if !ok {
			return 0, false
		}
		if d == 0 {
			return 0, false
		}
		return d, true
	}
	// n==2: forms 十X (10..19) or X十 (10,20,..90).
	if n == 2 {
		if runes[0] == '十' {
			d, ok := chineseNumeral[runes[1]]
			if !ok || d == 0 {
				return 0, false
			}
			return 10 + d, true
		}
		if runes[1] == '十' {
			d, ok := chineseNumeral[runes[0]]
			if !ok || d == 0 {
				return 0, false
			}
			return d * 10, true
		}
		return 0, false
	}
	// n==3: X十Y (21..99).
	if runes[1] != '十' {
		return 0, false
	}
	tens, ok1 := chineseNumeral[runes[0]]
	ones, ok2 := chineseNumeral[runes[2]]
	if !ok1 || !ok2 || tens == 0 || ones == 0 {
		return 0, false
	}
	return tens*10 + ones, true
}

// normalizeEditionInName rewrites the raw "第X届" fragment in a name to its
// canonical ordinal form so that 第十届 and 第10届 compare equal downstream.
func normalizeEditionInName(name string) string {
	open := strings.Index(name, "第")
	closeIdx := strings.Index(name, "届")
	if open < 0 || closeIdx <= open {
		return name
	}
	ord := normalizeEditionOrdinal(name)
	if ord == "" {
		// Leave unknown editions as-is (no guessing).
		return name
	}
	return name[:open] + "第" + ord + "届" + name[closeIdx+len("届"):]
}

// stageLexicon maps a competition stage token to its canonical stage. Only
// tokens that mark a genuinely separate competition entity are listed.
var stageLexicon = []struct {
	token   string
	canon   string
}{
	{"总决赛", "总决赛"},
	{"决赛", "决赛"},
	{"区域赛", "区域"},
	{"分站赛", "分站"},
	{"分站", "分站"},
	{"网络赛", "网络赛"},
	{"线上赛", "网络赛"},
	{"省赛", "省赛"},
	{"校内选拔", "校内"},
	{"校内赛", "校内"},
	{"校级赛", "校内"},
	{"预选赛", "预选"},
	{"预赛", "预选"},
	{"邀请赛", "邀请"},
}

// stationLexicon is a small, conservative set of site / regional tokens used to
// split same-series competitions into separate station entities. It is generic
// (cities and geographic regions) and only used for boundary detection: if two
// competitions both expose a station and they differ, they are not merged.
var stationLexicon = []string{
	"北京", "上海", "天津", "重庆", "广州", "深圳", "杭州", "南京", "苏州", "武汉",
	"成都", "西安", "郑州", "济南", "青岛", "大连", "沈阳", "哈尔滨", "长春",
	"长沙", "合肥", "福州", "厦门", "南昌", "昆明", "贵阳", "南宁", "兰州", "银川",
	"太原", "石家庄", "呼和浩特", "乌鲁木齐", "拉萨", "海口", "三亚", "无锡", "宁波",
	"常州", "徐州", "南通", "东莞", "佛山", "珠海", "洛阳", "南阳", "开封", "扬州",
	"镇江", "绍兴", "嘉兴", "金华", "台州", "温州", "泉州", "漳州", "湘潭", "株洲",
	"华东", "华北", "华南", "华中", "西南", "西北", "东北",
}

// identityStage returns the canonical stage of a competition name, or "" when no
// explicit stage marker is present. A "<city>站" or "<region>赛区" form implies a
// station-level stage (分站).
func identityStage(name string) string {
	for _, s := range stageLexicon {
		if strings.Contains(name, s.token) {
			return s.canon
		}
	}
	// <city>站 / <city>赛区 implies a station-level round.
	if identityStation(name) != "" && (strings.Contains(name, "站") || strings.Contains(name, "赛区")) {
		return "分站"
	}
	return ""
}

// identityStation returns a normalized station/site token for a competition name,
// or "" when none is present. A station word alone (without 站/赛区) does not
// imply a station; it only contributes once a station marker is present.
func identityStation(name string) string {
	if !strings.Contains(name, "站") && !strings.Contains(name, "赛区") {
		return ""
	}
	for _, city := range stationLexicon {
		if strings.Contains(name, city) {
			return city
		}
	}
	return ""
}

// seriesAcronymPattern matches a parenthesised or bare latin acronym token used
// as the series shorthand (e.g. （CCPC）, (ICPC), CCPC).
var seriesAcronymPattern = `[a-z]{2,12}`

// identitySeries derives a canonical series token from the competition name. It
// prefers the latin acronym (parenthesised or bare) such as ccpc / icpc, else
// falls back to the core Chinese entity after stripping edition/stage/station and
// announcement noise. The result is used only for compatibility checking: two
// competitions with explicit different series must not merge.
func identitySeries(name string) string {
	lower := strings.ToLower(name)
	// Prefer a parenthesised acronym: （CCPC） / (ICPC).
	for _, opener := range []string{"（", "("} {
		if closeIdx := strings.Index(lower, opener); closeIdx >= 0 {
			rest := lower[closeIdx+len(opener):]
			if end := strings.IndexAny(rest, "）)"); end > 0 {
				acronym := rest[:end]
				if isSeriesAcronym(acronym) {
					return acronym
				}
			}
		}
	}
	// Prefer a bare latin acronym token (e.g. "ccpc" in "ccpc郑州站").
	var out strings.Builder
	for _, r := range lower {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
		} else {
			out.WriteRune(' ')
		}
	}
	for _, token := range strings.Fields(out.String()) {
		if isSeriesAcronym(token) {
			return token
		}
	}
	// Fall back to the core Chinese entity name.
	s := lower
	for _, s2 := range stageLexicon {
		s = strings.ReplaceAll(s, s2.token, "")
	}
	// Remove the explicit edition fragment "第..届" as a whole (handles both
	// Chinese-numeral and digit forms without leaking residue).
	s = stripEditionFragment(s)
	for _, city := range stationLexicon {
		s = strings.ReplaceAll(s, city, "")
	}
	s = strings.ReplaceAll(s, "站", "")
	s = strings.ReplaceAll(s, "赛区", "")
	for _, noise := range seriesNoiseWords {
		s = strings.ReplaceAll(s, noise, "")
	}
	s = identityYearPattern.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripEditionFragment removes the whole "第..届" span (digits or Chinese
// numerals) from a name.
func stripEditionFragment(name string) string {
	open := strings.Index(name, "第")
	closeIdx := strings.Index(name, "届")
	if open < 0 || closeIdx <= open {
		return name
	}
	return name[:open] + name[closeIdx+len("届"):]
}

// seriesNoiseWords are announcement/convention words stripped when deriving a
// series token. They must not be confused with the entity name. Ordered so more
// specific words come before their substrings (e.g. 报名通知 before 报名).
var seriesNoiseWords = []string{
	"正式报名通知", "报名通知", "报名预告", "通知公告", "正式报名", "开始报名",
	"通知", "公告", "预告", "报名", "正式", "即将", "启动", "规则", "更新", "事宜",
	"安排", "关于", "组织", "学生", "参加", "我校", "举办",
	"比赛", "赛事", "参赛",
}

// isSeriesAcronym reports whether a latin token is a plausible competition series
// acronym (e.g. ccpc, icpc, gplt). Only pure ASCII a-z tokens count; CJK letters
// are never treated as an acronym so Chinese-name series never trigger the series
// gate. A token that is purely digits or a common word is also excluded.
func isSeriesAcronym(token string) bool {
	if len(token) < 2 || len(token) > 12 {
		return false
	}
	for _, r := range token {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	for _, w := range []string{"vip", "www", "http", "com", "cn", "org", "api", "news", "home", "the", "and", "for"} {
		if token == w {
			return false
		}
	}
	return true
}

// competitionIdentity is the structured identity derived from a competition name.
type competitionIdentity struct {
	series  string
	edition string
	stage   string
	station string
}

func parseCompetitionIdentity(name string) competitionIdentity {
	return competitionIdentity{
		series:  identitySeries(name),
		edition: normalizeEditionOrdinal(name),
		stage:   identityStage(name),
		station: identityStation(name),
	}
}

// identityCompatible reports whether the two structured identities do NOT
// disagree on any boundary component. Components that are unknown on either side
// are ignored (conservative: no guessing). The series boundary is only enforced
// when at least one side carries a clear latin acronym (e.g. ccpc vs icpc); for
// Chinese-only names we defer to name similarity, which is stable across the
// announcement phrasing (preview vs formal signup).
func identityCompatible(a, b competitionIdentity) bool {
	if a.series != "" && b.series != "" && a.series != b.series &&
		(isSeriesAcronym(a.series) || isSeriesAcronym(b.series)) {
		return false
	}
	if a.edition != "" && b.edition != "" && a.edition != b.edition {
		return false
	}
	if a.stage != "" && b.stage != "" && a.stage != b.stage {
		return false
	}
	if a.station != "" && b.station != "" && a.station != b.station {
		return false
	}
	return true
}

// sameIdentityText is the pure-string form of the identity merge decision. It is
// exported for table-driven tests and mirrors sameCompetitionIdentity.
func sameIdentityText(a, b string) bool {
	ia, ib := parseCompetitionIdentity(a), parseCompetitionIdentity(b)
	if !identityCompatible(ia, ib) {
		return false
	}
	// Name similarity on edition-normalized names.
	na := normalizedCompetitionName(normalizeEditionInName(a))
	nb := normalizedCompetitionName(normalizeEditionInName(b))
	if len([]rune(na)) < 6 || len([]rune(nb)) < 6 {
		return false
	}
	similarity := bigramDice(na, nb)
	contained := strings.Contains(na, nb) || strings.Contains(nb, na)
	return contained || similarity >= 0.78
}
