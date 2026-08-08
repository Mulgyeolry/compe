package analyzer

import (
	"testing"
	"time"
)

// loc is the Asia/Shanghai timezone used by the production scheduler.
var shanghai = time.FixedZone("Asia/Shanghai", 8*3600)

func TestDateRangeRightHandSideInheritsYear(t *testing.T) {
	// Huawei Cup real registration format: the right-hand date omits the year
	// and must inherit it from the explicitly-dated left-hand side.
	start, startRaw, end, endRaw := extractDates("参赛团队报名时间：2026年6月1日8:00至9月19日17:00", shanghai)
	if start == nil || end == nil {
		t.Fatalf("expected start and end parsed, got start=%v end=%v", start, end)
	}
	wantStart := time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)
	wantEnd := time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", end, wantEnd)
	}
	// Raw evidence must stay faithful to the page; the right side is not
	// rewritten to a fabricated year.
	if startRaw != "2026年6月1日8:00" {
		t.Errorf("startRaw = %q, want %q", startRaw, "2026年6月1日8:00")
	}
	if endRaw != "9月19日17:00" {
		t.Errorf("endRaw = %q, want %q", endRaw, "9月19日17:00")
	}
}

func TestCompetitionDateRangeRightHandSideInheritsYear(t *testing.T) {
	// Huawei Cup real competition-time format ("竞赛时间").
	start, startRaw, end, endRaw := extractCompetitionDates("竞赛时间：2026年9月23日8:00至9月27日12:00", shanghai)
	if start == nil || end == nil {
		t.Fatalf("expected start and end parsed, got start=%v end=%v", start, end)
	}
	wantStart := time.Date(2026, 9, 23, 8, 0, 0, 0, shanghai)
	wantEnd := time.Date(2026, 9, 27, 12, 0, 0, 0, shanghai)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", end, wantEnd)
	}
	if startRaw != "2026年9月23日8:00" {
		t.Errorf("startRaw = %q, want %q", startRaw, "2026年9月23日8:00")
	}
	if endRaw != "9月27日12:00" {
		t.Errorf("endRaw = %q, want %q", endRaw, "9月27日12:00")
	}
}

func TestDateRangeBothSidesWithYear(t *testing.T) {
	// Both ends carry an explicit year: existing behaviour must be preserved.
	start, _, end, _ := extractDates("报名时间：2026年6月1日8:00至2026年9月19日17:00", shanghai)
	if start == nil || end == nil {
		t.Fatalf("expected start and end parsed, got start=%v end=%v", start, end)
	}
	if !start.Equal(time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)) {
		t.Errorf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)) {
		t.Errorf("end = %v", end)
	}
}

func TestCompetitionDateRangeBothSidesWithYear(t *testing.T) {
	start, _, end, _ := extractCompetitionDates("竞赛时间：2026年9月23日8:00至2026年9月27日12:00", shanghai)
	if start == nil || end == nil {
		t.Fatalf("expected start and end parsed, got start=%v end=%v", start, end)
	}
	if !start.Equal(time.Date(2026, 9, 23, 8, 0, 0, 0, shanghai)) {
		t.Errorf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 9, 27, 12, 0, 0, 0, shanghai)) {
		t.Errorf("end = %v", end)
	}
}

func TestDateRangeYearlessEndEarlierThanStartIsRejected(t *testing.T) {
	// Inheriting the year would place the end before the start (a cross-year
	// year-less range). The range must be refused rather than guessing a
	// wrapped year. The text mimics a year-less end that belongs to a later
	// edition but sorts earlier in the same year.
	start, _, end, _ := extractDates("报名时间：2026年9月19日8:00至6月1日17:00", shanghai)
	if start != nil || end != nil {
		t.Fatalf("cross-year range must be rejected, got start=%v end=%v", start, end)
	}
}

func TestStandaloneDeadlineWithoutYearIsNotGuessed(t *testing.T) {
	// A standalone "报名截止时间：9月19日17:00" carries no year and must NOT
	// be parsed; only range expressions may inherit the year.
	start, startRaw, end, endRaw := extractDates("报名截止时间：9月19日17:00", shanghai)
	if start != nil || end != nil {
		t.Fatalf("year-less standalone deadline must not be guessed, got start=%v end=%v", start, end)
	}
	if startRaw != "" || endRaw != "" {
		t.Fatalf("expected empty raw evidence, got startRaw=%q endRaw=%q", startRaw, endRaw)
	}
}

func TestCompetitionDayOnlyRightEndInheritsYearMonth(t *testing.T) {
	// CCPC real case: "2026年4月25~26" — right end states only a day and must
	// inherit year+month from the explicit left end.
	start, startRaw, end, endRaw := extractCompetitionDates("第11届中国大学生程序设计竞赛（CCPC）总决赛将于2026年4月25~26在南阳举行", shanghai)
	if start == nil || end == nil {
		t.Fatalf("expected competition range parsed, got start=%v end=%v", start, end)
	}
	wantStart := time.Date(2026, 4, 25, 0, 0, 0, 0, shanghai)
	wantEnd := time.Date(2026, 4, 26, 0, 0, 0, 0, shanghai)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", end, wantEnd)
	}
	if startRaw != "2026年4月25" {
		t.Errorf("startRaw = %q", startRaw)
	}
	if endRaw != "26" {
		t.Errorf("endRaw = %q", endRaw)
	}
}

func TestCompetitionDayOnlyRightEndWithSeparatorZhi(t *testing.T) {
	// "2026年4月25日至26日" uses the 至 separator and a 日 suffix on the right.
	start, _, end, _ := extractCompetitionDates("比赛时间：2026年4月25日至26日", shanghai)
	if start == nil || end == nil {
		t.Fatalf("expected competition range parsed, got start=%v end=%v", start, end)
	}
	if !start.Equal(time.Date(2026, 4, 25, 0, 0, 0, 0, shanghai)) {
		t.Errorf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 4, 26, 0, 0, 0, 0, shanghai)) {
		t.Errorf("end = %v", end)
	}
}

func TestCompetitionDayOnlyRightEndRejectsCrossMonth(t *testing.T) {
	// "2026年4月30日~2日" — the right day (2) sorts before the start day (30).
	// We refuse to guess a cross-month range rather than fabricate it.
	start, _, end, _ := extractCompetitionDates("比赛时间：2026年4月30日~2日", shanghai)
	if start != nil || end != nil {
		t.Fatalf("cross-month day-only range must be rejected, got start=%v end=%v", start, end)
	}
}

func TestCompetitionDayOnlyRightEndRejectsInvalidCalendarDate(t *testing.T) {
	// "2026年4月31~32" — April 31 is not a real calendar date and April 32 is
	// invalid too; the whole range must be refused.
	start, _, end, _ := extractCompetitionDates("比赛时间：2026年4月31~32", shanghai)
	if start != nil || end != nil {
		t.Fatalf("invalid-calendar day-only range must be rejected, got start=%v end=%v", start, end)
	}
}

func TestCompetitionFullYearMonthDayRangeStillParses(t *testing.T) {
	// A full year+month+day range (existing behaviour) must keep working.
	start, startRaw, end, endRaw := extractCompetitionDates("竞赛时间：2026年9月23日8:00至2026年9月27日12:00", shanghai)
	if start == nil || end == nil {
		t.Fatalf("expected full range parsed, got start=%v end=%v", start, end)
	}
	if !start.Equal(time.Date(2026, 9, 23, 8, 0, 0, 0, shanghai)) {
		t.Errorf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 9, 27, 12, 0, 0, 0, shanghai)) {
		t.Errorf("end = %v", end)
	}
	if startRaw != "2026年9月23日8:00" {
		t.Errorf("startRaw = %q", startRaw)
	}
	if endRaw != "2026年9月27日12:00" {
		t.Errorf("endRaw = %q", endRaw)
	}
}

func TestCompetitionDayOnlyRightEndWithExplicitTimeIsRejected(t *testing.T) {
	// "比赛时间：2026年4月25日8:00~26日18:00" — the right end writes its own
	// explicit time. The day-only fallback must refuse it rather than capture
	// "26日" and wrongly inherit the left-hand 08:00 as 26日08:00.
	start, _, end, _ := extractCompetitionDates("比赛时间：2026年4月25日8:00~26日18:00", shanghai)
	if start != nil || end != nil {
		t.Fatalf("day-only end with explicit right time must be rejected, got start=%v end=%v", start, end)
	}
}

func TestCompetitionDayOnlyRightEndWithShiTimeIsRejected(t *testing.T) {
	// Same rejection for the "18时" variant.
	start, _, end, _ := extractCompetitionDates("比赛时间：2026年4月25日8:00~26日18时", shanghai)
	if start != nil || end != nil {
		t.Fatalf("day-only end with explicit 时 time must be rejected, got start=%v end=%v", start, end)
	}
}

func TestCompetitionDayOnlyRangeRejectsRegistrationClause(t *testing.T) {
	// "报名将于2026年4月25~26日进行" — 报名 is registration semantics, so the
	// day-only fallback must not treat it as competition dates even though the
	// shape matches.
	start, _, end, _ := extractCompetitionDates("报名将于2026年4月25~26日进行", shanghai)
	if start != nil || end != nil {
		t.Fatalf("registration clause must not yield competition dates, got start=%v end=%v", start, end)
	}
}

func TestCompetitionDayOnlyRangeRejectsRegistrationClauseWithCompetitionWord(t *testing.T) {
	// "本次比赛报名将于2026年4月25~26日进行，比赛时间另行通知" — the word 比赛
	// appears but "比赛报名" is still registration. Competition time is announced
	// separately. Must NOT be parsed as competition dates.
	start, _, end, _ := extractCompetitionDates("本次比赛报名将于2026年4月25~26日进行，比赛时间另行通知", shanghai)
	if start != nil || end != nil {
		t.Fatalf("registration clause must not yield competition dates, got start=%v end=%v", start, end)
	}
}

func TestRegistrationRangeUnaffectedByDayOnlySupport(t *testing.T) {
	// Registration dates must NOT gain the day-only right-end behaviour: a
	// "报名时间：2026年4月25~26" must stay unparsed rather than inherit a month.
	start, _, end, _ := extractDates("报名时间：2026年4月25~26", shanghai)
	if start != nil || end != nil {
		t.Fatalf("registration day-only range must NOT be parsed, got start=%v end=%v", start, end)
	}
	// The existing month+day right-end registration range still works.
	s2, _, e2, _ := extractDates("参赛团队报名时间：2026年6月1日8:00至9月19日17:00", shanghai)
	if s2 == nil || e2 == nil {
		t.Fatalf("existing registration range must keep parsing, got start=%v end=%v", s2, e2)
	}
	if !s2.Equal(time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)) {
		t.Errorf("registration start = %v", s2)
	}
	if !e2.Equal(time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)) {
		t.Errorf("registration end = %v", e2)
	}
}

func TestPaymentLineDoesNotOverrideRegistrationRange(t *testing.T) {
	// "参赛缴费时间" is a payment deadline, not a registration deadline. It
	// must not be recognised as the registration start/end and must not
	// override the actual registration range on the same page.
	text := "参赛团队报名时间：2026年6月1日8:00至9月19日17:00\n参赛缴费时间：2026年6月1日8:00至9月21日17:00"
	start, startRaw, end, endRaw := extractDates(text, shanghai)
	if start == nil || end == nil {
		t.Fatalf("expected registration range parsed, got start=%v end=%v", start, end)
	}
	if !start.Equal(time.Date(2026, 6, 1, 8, 0, 0, 0, shanghai)) {
		t.Errorf("start = %v", start)
	}
	// The end must stay the registration end (9月19日), NOT the payment end
	// (9月21日).
	if !end.Equal(time.Date(2026, 9, 19, 17, 0, 0, 0, shanghai)) {
		t.Errorf("end = %v, want 2026-09-19 17:00 (payment deadline must not override)", end)
	}
	if endRaw != "9月19日17:00" {
		t.Errorf("endRaw = %q, want %q", endRaw, "9月19日17:00")
	}
	if startRaw != "2026年6月1日8:00" {
		t.Errorf("startRaw = %q", startRaw)
	}
}
