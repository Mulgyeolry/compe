package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestAnalyzerVersionReprocessesUnchangedDocumentOnce(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	document := model.Document{Title: "contest", URL: "https://example.com/contest", Text: "same content"}
	changed, err := database.RecordObservationVersioned(ctx, "official", document, "same-hash", model.TrustHigh, "v1", now)
	if err != nil || !changed {
		t.Fatalf("first observation changed=%v err=%v", changed, err)
	}
	changed, err = database.RecordObservationVersioned(ctx, "official", document, "same-hash", model.TrustHigh, "v1", now.Add(time.Hour))
	if err != nil || changed {
		t.Fatalf("same version changed=%v err=%v", changed, err)
	}
	changed, err = database.RecordObservationVersioned(ctx, "official", document, "same-hash", model.TrustHigh, "v2", now.Add(2*time.Hour))
	if err != nil || !changed {
		t.Fatalf("new analyzer version changed=%v err=%v", changed, err)
	}
	changed, err = database.RecordObservationVersioned(ctx, "official", document, "same-hash", model.TrustHigh, "v2", now.Add(3*time.Hour))
	if err != nil || changed {
		t.Fatalf("repeated v2 changed=%v err=%v", changed, err)
	}
}

func TestAnalysisAuditIsStoredOnMatchingObservation(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	document := model.Document{Title: "contest", URL: "https://example.com/audit", Text: "content", RawText: "raw content"}
	if changed, err := database.RecordObservationVersioned(ctx, "official", document, "audit-hash", model.TrustHigh, "v3", now); err != nil || !changed {
		t.Fatalf("observation changed=%v err=%v", changed, err)
	}
	audit := model.AnalysisAudit{
		AnalyzerVersion: "v3", Model: "test-model", InputHash: "input-hash", SegmentIDs: []string{"html-1"},
		RawResponses: []string{"{}"}, Rejections: []model.AnalysisRejection{{Field: "fee", Reason: "missing evidence"}}, AnalyzedAt: now,
	}
	if err := database.RecordAnalysisAudit(ctx, "official", document.URL, "audit-hash", audit); err != nil {
		t.Fatal(err)
	}
	var encoded string
	if err := database.db.QueryRowContext(ctx, `SELECT analysis_result_json FROM observations WHERE source_id=? AND url=?`, "official", document.URL).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var saved model.AnalysisAudit
	if err := json.Unmarshal([]byte(encoded), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Model != "test-model" || len(saved.Rejections) != 1 || len(saved.RawResponses) != 1 {
		t.Fatalf("unexpected stored audit: %#v", saved)
	}
}
