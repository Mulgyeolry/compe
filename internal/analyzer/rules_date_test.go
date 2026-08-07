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
