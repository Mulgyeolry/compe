package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

// upsertComp is a small helper to insert/update a competition through the real
// Store so the identity merge path is exercised end to end. The EntityKey is
// derived from the name (mirroring how the analyzer derives it) so that a
// different edition naturally yields a different key, as it would in production.
func upsertComp(t *testing.T, db *Store, name, organizer, url string, trust model.Trust) (model.Competition, bool) {
	t.Helper()
	comp := model.Competition{
		EntityKey:   "auto-" + name,
		Name:        name,
		Organizer:   organizer,
		OfficialURL: url,
		Trust:       trust,
	}
	old, isNew, err := db.UpsertCompetition(context.Background(), comp, "src", time.Now())
	if err != nil {
		t.Fatalf("UpsertCompetition(%q) err=%v", name, err)
	}
	return old, isNew
}

func newIdentityDB(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Same edition written as Chinese numeral and digits must collapse to one row.
func TestIdentitySameEditionChineseAndDigitsMerge(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第十届CCPC总决赛", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第10届CCPC总决赛", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if isNew {
		t.Fatal("第十届 and 第10届 must merge into one row")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 1)
}

// Different editions must never merge, even on the same URL.
func TestIdentityDifferentEditionsSeparate(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第10届CCPC总决赛", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC总决赛", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if !isNew {
		t.Fatal("第10届 and 第11届 must be separate rows")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 2)
}

// Different stations in the same edition must be separate rows.
func TestIdentityDifferentStationsSeparate(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第11届CCPC郑州站", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC济南站", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if !isNew {
		t.Fatal("郑州站 and 济南站 must be separate rows")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 2)
}

// 分站 vs 总决赛 in same edition must be separate rows.
func TestIdentityStationVsFinalSeparate(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第11届CCPC分站赛", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC总决赛", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if !isNew {
		t.Fatal("分站 and 总决赛 must be separate rows")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 2)
}

// Same station, same edition: preview and formal signup merge.
func TestIdentityPreviewAndFormalMerge(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第11届CCPC郑州站报名预告", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC郑州站正式报名通知", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if isNew {
		t.Fatal("preview and formal signup for same station must merge")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 1)
}

// Rule update for the same station must merge into the existing row.
func TestIdentityRuleUpdateMerges(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第11届CCPC郑州站", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	first, err := db.GetCompetition(context.Background(), "auto-第11届CCPC郑州站")
	if err != nil {
		t.Fatalf("load first row: %v", err)
	}
	_, isNew = upsertComp(t, db, "第11届CCPC郑州站比赛规则更新", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if isNew {
		t.Fatal("rule update must merge into existing station row")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 1)
	// The existing row must still be reachable by the first entity's key and ID.
	saved, err := db.GetCompetitionByID(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("existing row disappeared after rule update: %v", err)
	}
	if saved.Name == "" {
		t.Fatal("expected the merged row to survive")
	}
}

// Facts must not cross entity boundaries: a station deadline must not leak into
// a different station or the final.
func TestIdentityFactsDoNotCrossBoundaries(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第11届CCPC郑州站", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC济南站", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if !isNew {
		t.Fatal("济南 must be a separate row from 郑州")
	}
	compA, err := db.GetCompetition(context.Background(), "auto-第11届CCPC郑州站")
	if err != nil {
		t.Fatalf("load 郑州: %v", err)
	}
	compB, err := db.GetCompetition(context.Background(), "auto-第11届CCPC济南站")
	if err != nil {
		t.Fatalf("load 济南: %v", err)
	}
	if compA.ID == compB.ID {
		t.Fatal("郑州 and 济南 must be distinct rows")
	}
	// Give 郑州 a deadline.
	deadline := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	compA.RegistrationEnd = &deadline
	compA.RegistrationEndRaw = "2026年9月1日"
	compA.Status = model.StatusRegistrationOpen
	compA.StatusEvidence = "报名中"
	if _, isNew, err := db.UpsertCompetition(context.Background(), compA, "src", time.Now()); err != nil || isNew {
		t.Fatalf("update 郑州 isNew=%v err=%v", isNew, err)
	}
	reloadedA, _ := db.GetCompetitionByID(context.Background(), compA.ID)
	reloadedB, _ := db.GetCompetitionByID(context.Background(), compB.ID)
	if reloadedA.RegistrationEnd == nil {
		t.Fatal("郑州 deadline should be set")
	}
	if reloadedB.RegistrationEnd != nil {
		t.Fatalf("济南 deadline must not be polluted by 郑州: %v", reloadedB.RegistrationEnd)
	}
}

// Reusing an official URL for a new edition must create a new row.
func TestIdentityURLReuseNewEditionNewRow(t *testing.T) {
	db := newIdentityDB(t)
	const url = "https://ccpc.io/a/377.html"
	_, isNew := upsertComp(t, db, "第10届CCPC总决赛", "CCPC", url, model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC总决赛", "CCPC", url, model.TrustHigh)
	if !isNew {
		t.Fatal("reused URL with a new edition must create a new row")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 2)
}

// Same series+edition+station with a changed announcement URL still merges via
// strong identity evidence.
func TestIdentityURLChangeSameEditionMerges(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第11届CCPC郑州站", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC郑州站正式报名", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if isNew {
		t.Fatal("same edition+station with changed URL must merge via identity")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 1)
}

// Re-scanning the exact same announcement URL must be idempotent: no new row and
// no change to the existing entity.
func TestIdentityRescanSameArticleIsIdempotent(t *testing.T) {
	db := newIdentityDB(t)
	const url = "https://ccpc.io/a/377.html"
	_, isNew := upsertComp(t, db, "第11届CCPC总决赛", "CCPC", url, model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	first, err := db.GetCompetition(context.Background(), "auto-第11届CCPC总决赛")
	if err != nil {
		t.Fatal(err)
	}
	// Re-upsert the exact same announcement (same URL, same title).
	old, isNew, err := db.UpsertCompetition(context.Background(), model.Competition{
		EntityKey:   "auto-第11届CCPC总决赛",
		Name:        "第11届CCPC总决赛",
		Organizer:   "CCPC",
		OfficialURL: url,
		Trust:       model.TrustHigh,
	}, "src", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Fatal("re-scan of the same article must not create a new row")
	}
	if old.ID != first.ID {
		t.Fatalf("re-scan changed the entity id: got %d want %d", old.ID, first.ID)
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 1)
}

// Conservative merge when edition/stage are absent: two competitions that only
// share a generic name but differ in an explicit year must stay separate.
func TestIdentityConservativeMergeMissingEdition(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "2025 CCPC总决赛", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "2026 CCPC总决赛", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if !isNew {
		t.Fatal("different years must not merge even with similar names")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 2)
}

// A generic stage (分站) vs the 总决赛 in the same year must stay separate.
func TestIdentityGenericStationVsFinalSeparate(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "2026 CCPC分站赛", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "2026 CCPC总决赛", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if !isNew {
		t.Fatal("分站 and 总决赛 must be separate rows in the same year")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 2)
}

// Real-world CCPC regression: the exact titles that previously polluted the
// canonical (different stations, different editions, cross-site merges) must now
// each become or stay their own entity.
func TestIdentityRealWorldCCPCTitles(t *testing.T) {
	db := newIdentityDB(t)
	rows := []string{
		"第11届中国大学生程序设计竞赛（CCPC）总决赛",
		"第11届中国大学生程序设计竞赛（CCPC）重庆站",
		"第11届中国大学生程序设计竞赛（CCPC）济南站",
		"第10届中国大学生程序设计竞赛（CCPC）总决赛",
	}
	for i, name := range rows {
		_, isNew := upsertComp(t, db, name, "CCPC", "https://ccpc.io/a/"+string(rune('0'+i+1))+".html", model.TrustHigh)
		if !isNew {
			t.Fatalf("%q should be a new distinct row", name)
		}
	}
	// Four distinct entities: final vs station, 重庆 vs 济南, 第10 vs 第11.
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 4)
	// The final and the two stations and the older edition must not collapse.
	all, err := db.ListCompetitions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, r := range all {
		ids[r.Name] = r.ID
	}
	if ids["第11届中国大学生程序设计竞赛（CCPC）重庆站"] == ids["第11届中国大学生程序设计竞赛（CCPC）济南站"] {
		t.Fatal("重庆站 and 济南站 must be separate")
	}
	if ids["第11届中国大学生程序设计竞赛（CCPC）总决赛"] == ids["第10届中国大学生程序设计竞赛（CCPC）总决赛"] {
		t.Fatal("第11届 and 第10届 finals must be separate")
	}
	if ids["第11届中国大学生程序设计竞赛（CCPC）总决赛"] == ids["第11届中国大学生程序设计竞赛（CCPC）重庆站"] {
		t.Fatal("总决赛 and 重庆站 must be separate")
	}
}

// Chinese-numeral and digit edition forms for the same 总决赛 must collapse.
func TestIdentityChineseAndDigitsEditionCollapse(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第十届中国大学生程序设计竞赛总决赛", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第10届中国大学生程序设计竞赛总决赛", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if isNew {
		t.Fatal("第十届 and 第10届 must merge into one row")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 1)
}

// Blocker C: the same URL reused for a DIFFERENT station in the same edition must
// create a new row (URL fast path must not bypass the station boundary).
func TestIdentityURLReuseDifferentStationNewRow(t *testing.T) {
	db := newIdentityDB(t)
	const url = "https://ccpc.io/a/1.html"
	_, isNew := upsertComp(t, db, "第11届CCPC郑州站", "CCPC", url, model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC济南站", "CCPC", url, model.TrustHigh)
	if !isNew {
		t.Fatal("same URL + different station must create a new row")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 2)
}

// Blocker C: same URL + 分站 vs 总决赛 in the same edition must be separate rows.
func TestIdentityURLReuseStationVsFinalNewRow(t *testing.T) {
	db := newIdentityDB(t)
	const url = "https://ccpc.io/a/2.html"
	_, isNew := upsertComp(t, db, "第11届CCPC分站赛", "CCPC", url, model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC总决赛", "CCPC", url, model.TrustHigh)
	if !isNew {
		t.Fatal("same URL + 分站 vs 总决赛 must be separate rows")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 2)
}

// Blocker C: same URL + different edition must create a new row.
func TestIdentityURLReuseDifferentEditionNewRow(t *testing.T) {
	db := newIdentityDB(t)
	const url = "https://ccpc.io/a/3.html"
	_, isNew := upsertComp(t, db, "第10届CCPC总决赛", "CCPC", url, model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC总决赛", "CCPC", url, model.TrustHigh)
	if !isNew {
		t.Fatal("same URL + different edition must be separate rows")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 2)
}

// Blocker C: same URL + same entity with a minor title change must merge.
func TestIdentityURLSameEntityMinorTitleChangeMerges(t *testing.T) {
	db := newIdentityDB(t)
	const url = "https://ccpc.io/a/4.html"
	_, isNew := upsertComp(t, db, "第11届CCPC总决赛", "CCPC", url, model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	old, isNew, err := db.UpsertCompetition(context.Background(), model.Competition{
		EntityKey:   "auto-第11届CCPC总决赛",
		Name:        "第11届中国大学生程序设计竞赛（CCPC）总决赛",
		Organizer:   "CCPC",
		OfficialURL: url,
		Trust:       model.TrustHigh,
	}, "src", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Fatal("same URL + same entity minor title change must merge")
	}
	if old.Name == "" {
		t.Fatal("expected the existing row to be found")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 1)
}

// Blocker A at merge level: 缩写 vs 中文全称 for the same final collapse.
func TestIdentityAbbreviationVsFullNameMerge(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第11届中国大学生程序设计竞赛（CCPC）总决赛", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第十一届中国大学生程序设计竞赛总决赛", "中国大学生程序设计竞赛", "https://ccpc.io/a/2.html", model.TrustHigh)
	if isNew {
		t.Fatal("缩写 and 中文全称 of the same series must merge")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 1)
}

// Blocker B at merge level: a generic series announcement must not merge into a
// specific station.
func TestIdentityGenericDoesNotMergeIntoSpecific(t *testing.T) {
	db := newIdentityDB(t)
	_, isNew := upsertComp(t, db, "第11届CCPC", "CCPC", "https://ccpc.io/a/1.html", model.TrustHigh)
	if !isNew {
		t.Fatal("first insert should be new")
	}
	_, isNew = upsertComp(t, db, "第11届CCPC郑州站", "CCPC", "https://ccpc.io/a/2.html", model.TrustHigh)
	if !isNew {
		t.Fatal("generic series announcement must not merge into a specific station")
	}
	assertCount(t, db, "SELECT COUNT(*) FROM competitions", 2)
}
