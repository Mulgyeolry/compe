package store

import "testing"

func TestNormalizeEditionOrdinal(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" means unknown
	}{
		{"第十届", "10"},
		{"第10届", "10"},
		{"第十一届", "11"},
		{"第11届", "11"},
		{"第二十届", "20"},
		{"第九十九届", "99"},
		{"第三十届", "30"},
		{"第一届", "1"},
		{"第五届", "5"},
		{"第十四届", "14"},
		{"第二十四届", "24"},
		// mixed / invalid / ambiguous must be unknown, never guessed
		{"第〇届", ""},
		{"", ""},
		{"2026年", ""},
		{"第十届x", "10"},
	}
	for _, c := range cases {
		got := normalizeEditionOrdinal(c.in)
		if got != c.want {
			t.Errorf("normalizeEditionOrdinal(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseCompetitionIdentity(t *testing.T) {
	cases := []struct {
		name    string
		series  string
		edition string
		stage   string
		station string
	}{
		{"第11届中国大学生程序设计竞赛（CCPC）总决赛", "ccpc", "11", "总决赛", ""},
		{"第11届中国大学生程序设计竞赛（CCPC）郑州站", "ccpc", "11", "分站", "郑州"},
		{"第11届中国大学生程序设计竞赛（CCPC）济南站", "ccpc", "11", "分站", "济南"},
		{"第十届CCPC总决赛", "ccpc", "10", "总决赛", ""},
		{"2026 CCPC 网络赛通知", "ccpc", "", "网络赛", ""},
		{"第11届CCPC郑州站报名预告", "ccpc", "11", "分站", "郑州"},
	}
	for _, c := range cases {
		got := parseCompetitionIdentity(c.name)
		if got.series != c.series {
			t.Errorf("parseCompetitionIdentity(%q).series = %q, want %q", c.name, got.series, c.series)
		}
		if got.edition != c.edition {
			t.Errorf("parseCompetitionIdentity(%q).edition = %q, want %q", c.name, got.edition, c.edition)
		}
		if got.stage != c.stage {
			t.Errorf("parseCompetitionIdentity(%q).stage = %q, want %q", c.name, got.stage, c.stage)
		}
		if got.station != c.station {
			t.Errorf("parseCompetitionIdentity(%q).station = %q, want %q", c.name, got.station, c.station)
		}
	}
}

func TestIdentityBoundaries(t *testing.T) {
	cases := []struct {
		a, b string
		want bool // want == true means "same entity, may merge"
	}{
		// 1. 第十届 vs 第10届 merge
		{"第十届CCPC总决赛", "第10届CCPC总决赛", true},
		// 2. 第10届 vs 第11届 separate
		{"第10届CCPC总决赛", "第11届CCPC总决赛", false},
		// 3. 郑州 vs 济南 separate
		{"第11届CCPC郑州站", "第11届CCPC济南站", false},
		// 4. 分站 vs 总决赛 separate
		{"第11届CCPC分站赛", "第11届CCPC总决赛", false},
		// 5. 同届同站预告/报名 merge
		{"第11届CCPC郑州站报名预告", "第11届CCPC郑州站正式报名", true},
		// 6. 同届同站规则更新 merge
		{"第11届CCPC郑州站", "第11届CCPC郑州站比赛规则", true},
		// 7. 网络赛 vs 总决赛 separate
		{"2026 CCPC 网络赛通知", "第11届CCPC总决赛", false},
		// Blocker A: 缩写 vs 中文全称 merge into same series
		{"第11届中国大学生程序设计竞赛（CCPC）总决赛", "第十一届中国大学生程序设计竞赛总决赛", true},
		// Blocker A: 明确不同 series 拒绝合并
		{"第11届CCPC总决赛", "第11届ICPC总决赛", false},
		// Blocker B: 泛化系列公告 vs 具体分站 separate
		{"第11届CCPC", "第11届CCPC郑州站", false},
		// Blocker B: 泛化分站公告 vs 具体分站 separate
		{"第11届CCPC分站赛", "第11届CCPC郑州站", false},
		// Blocker B: 泛化公告 vs 总决赛 separate
		{"第11届CCPC", "第11届CCPC总决赛", false},
		// 决赛/总决赛 归一为同一 stage 后 merge
		{"第十一届CCPC总决赛", "第十一届CCPC决赛", true},
		// 未收录地点也识别为 station-level stage
		{"第11届CCPC蚌埠站", "第11届CCPC芜湖站", false},
		{"第11届CCPC蚌埠站", "第11届CCPC蚌埠站正式报名", true},
	}
	for _, c := range cases {
		got := sameIdentityText(c.a, c.b)
		if got != c.want {
			t.Errorf("sameIdentityText(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// Blocker A: series alias normalization maps the latin acronym and the Chinese
// full name to a single canonical series.
func TestSeriesAliasNormalization(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"第11届中国大学生程序设计竞赛（CCPC）总决赛", "ccpc"},
		{"第十一届中国大学生程序设计竞赛总决赛", "ccpc"},
		{"中国大学生程序设计大赛总决赛", "ccpc"},
		{"第11届CCPC总决赛", "ccpc"},
		{"第11届ICPC总决赛", "icpc"},
	}
	for _, c := range cases {
		got := identitySeries(c.name)
		if got != c.want {
			t.Errorf("identitySeries(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// 决赛 and 总决赛 must collapse into one canonical stage.
func TestStageFinalNormalization(t *testing.T) {
	if identityStage("第十一届CCPC总决赛") != "总决赛" {
		t.Fatal("总决赛 stage expected")
	}
	if identityStage("第十一届CCPC决赛") != "总决赛" {
		t.Fatal("决赛 must normalize to 总决赛")
	}
}

// Station-level stage must be detected even for cities not in the lexicon.
func TestStationDetectionOutsideLexicon(t *testing.T) {
	if identityStation("第11届CCPC蚌埠站") != "蚌埠" {
		t.Fatalf("station 蚌埠 expected, got %q", identityStation("第11届CCPC蚌埠站"))
	}
	if identityStage("第11届CCPC蚌埠站") != "分站" {
		t.Fatalf("stage 分站 expected for unlisted-city 站, got %q", identityStage("第11届CCPC蚌埠站"))
	}
	// A place that cannot be reliably extracted must be unknown, never guessed.
	if identityStation("第11届CCPC分站赛") != "" {
		t.Fatalf("分站赛 must not fabricate a station, got %q", identityStation("第11届CCPC分站赛"))
	}
}
