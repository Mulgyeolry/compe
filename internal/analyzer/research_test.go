package analyzer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
)

func TestResearchAcceptsOnlyTraceableClaims(t *testing.T) {
	officialURL := "https://contest.example.com/official"
	communityURL := "https://www.nowcoder.com/discuss/123"
	result := ResearchResult{
		Summary:     SourcedFact{Value: "需要完成云原生后端作品", Evidence: "使用Go和Kubernetes构建云原生后端服务", SourceURL: officialURL},
		SuitableFor: SourcedFact{Value: "适合后端和云原生方向学生", Evidence: "使用Go和Kubernetes构建云原生后端服务", SourceURL: officialURL},
		Skills:      []SourcedFact{{Value: "Go", Evidence: "使用Go和Kubernetes构建云原生后端服务", SourceURL: officialURL}},
		Difficulty:  SourcedFact{Value: "决赛偏工程实践", Evidence: "决赛需要较强工程能力", SourceURL: communityURL},
		ResumeValue: SourcedFact{Value: "保证获得大厂录用", Evidence: "不存在的证据", SourceURL: communityURL},
		Keywords: []SourcedFact{
			{Value: "后端开发", Evidence: "云原生后端服务", SourceURL: officialURL},
			{Value: "虚假标签", Evidence: "不存在的证据", SourceURL: communityURL},
		},
		Confidence: "high",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		encoded, _ := json.Marshal(result)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "test", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": string(encoded)}, "finish_reason": "stop"}},
		})
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	analysisEngine := New(config.Config{Location: time.FixedZone("CST", 8*3600)})
	competition := model.Competition{Name: "2026云原生开发赛", Keywords: []string{"Go"}, Trust: model.TrustHigh}
	official := model.Document{Title: "官方赛题", URL: officialURL, Text: "本赛题要求参赛者使用Go和Kubernetes构建云原生后端服务。"}
	secondary := []model.ResearchSource{{Title: "参赛复盘", URL: communityURL, Text: "往届参赛者认为初赛上手较快，但决赛需要较强工程能力。", Kind: "community"}}
	analysis, keywords, err := analysisEngine.AnalyzeResearch(context.Background(), competition, official, secondary, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Summary == "" || analysis.Difficulty == "" || analysis.ResumeValue != "" || analysis.Confidence != "medium" {
		t.Fatalf("unexpected validated analysis: %#v", analysis)
	}
	if !containsAnyKeyword(keywords, "Go") || !containsAnyKeyword(keywords, "后端开发") || containsAnyKeyword(keywords, "虚假标签") {
		t.Fatalf("unexpected keywords: %#v", keywords)
	}
}

func containsAnyKeyword(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
