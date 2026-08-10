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
		name      string
		series    string
		edition   string
		stage     string
		station   string
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
	}
	for _, c := range cases {
		got := sameIdentityText(c.a, c.b)
		if got != c.want {
			t.Errorf("sameIdentityText(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
