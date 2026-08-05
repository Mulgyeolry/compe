package analyzer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"competition-assistant/internal/model"
)

// AnalyzeResearch builds a qualitative profile without changing official facts.
// Every accepted model claim must quote one of the supplied source documents.
func (a *Analyzer) AnalyzeResearch(ctx context.Context, competition model.Competition, official model.Document, secondary []model.ResearchSource, now time.Time) (model.CompetitionAnalysis, []string, error) {
	sources := []model.ResearchSource{{Title: official.Title, URL: official.URL, Text: official.Text, Kind: "official"}}
	sources = append(sources, secondary...)
	keywords := unique(append([]string{}, competition.Keywords...))
	fallback := fallbackAnalysis(competition, official, now)
	if !a.llm.Enabled() {
		return fallback, keywords, nil
	}
	result, err := a.llm.AnalyzeResearch(ctx, competition, sources)
	if err != nil {
		return fallback, keywords, err
	}
	sourceByURL := make(map[string]model.ResearchSource, len(sources))
	for _, source := range sources {
		sourceByURL[source.URL] = source
	}
	analysis := model.CompetitionAnalysis{AnalyzedAt: now, Confidence: normalizeConfidence(result.Confidence)}
	used := make(map[string]bool)
	accept := func(fact SourcedFact) string {
		source, ok := sourceByURL[strings.TrimSpace(fact.SourceURL)]
		if !ok || strings.TrimSpace(fact.Value) == "" || strings.TrimSpace(fact.Evidence) == "" ||
			!strings.Contains(normalize(source.Text), normalize(fact.Evidence)) {
			return ""
		}
		key := source.URL + "\x00" + fact.Evidence
		if !used[key] && len(analysis.References) < 8 {
			used[key] = true
			analysis.References = append(analysis.References, model.AnalysisReference{Title: source.Title, URL: source.URL, Evidence: fact.Evidence})
		}
		return strings.TrimSpace(fact.Value)
	}
	analysis.Summary = accept(result.Summary)
	analysis.SuitableFor = accept(result.SuitableFor)
	analysis.Difficulty = accept(result.Difficulty)
	analysis.ResumeValue = accept(result.ResumeValue)
	analysis.Caveats = accept(result.Caveats)
	for _, skill := range result.Skills {
		if value := accept(skill); value != "" {
			analysis.Skills = append(analysis.Skills, value)
		}
	}
	for _, keyword := range result.Keywords {
		if value := accept(keyword); value != "" && len([]rune(value)) <= 30 {
			keywords = append(keywords, value)
		}
	}
	keywords = unique(keywords)
	if analysis.Summary == "" {
		analysis.Summary = fallback.Summary
	}
	if analysis.SuitableFor == "" {
		analysis.SuitableFor = fallback.SuitableFor
	}
	if len(analysis.Skills) == 0 {
		analysis.Skills = fallback.Skills
	}
	if analysis.Caveats == "" {
		analysis.Caveats = fallback.Caveats
	}
	if analysis.Confidence == "" {
		analysis.Confidence = fallback.Confidence
	}
	if len(analysis.References) == 0 {
		analysis.References = fallback.References
	}
	if hasSecondaryReference(analysis.References, official.URL) && analysis.Confidence == "high" {
		analysis.Confidence = "medium"
	}
	analysis.AnalysisHash = researchHash(sources)
	return analysis, keywords, nil
}

func fallbackAnalysis(competition model.Competition, official model.Document, now time.Time) model.CompetitionAnalysis {
	analysis := model.CompetitionAnalysis{
		Summary:     competition.Content,
		SuitableFor: competition.FitReason,
		Caveats:     competition.EligibilityNote,
		Confidence:  "medium",
		AnalyzedAt:  now,
		AnalysisHash: researchHash([]model.ResearchSource{{
			Title: official.Title, URL: official.URL, Text: official.Text, Kind: "official",
		}}),
	}
	analysis.Skills = append(analysis.Skills, competition.Keywords...)
	if competition.Trust == model.TrustHigh {
		analysis.Confidence = "high"
	}
	if competition.Content != "" && strings.Contains(normalize(official.Text), normalize(competition.Content)) {
		analysis.References = []model.AnalysisReference{{Title: official.Title, URL: official.URL, Evidence: competition.Content}}
	}
	return analysis
}

func normalizeConfidence(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func hasSecondaryReference(references []model.AnalysisReference, officialURL string) bool {
	for _, reference := range references {
		if reference.URL != officialURL {
			return true
		}
	}
	return false
}

func researchHash(sources []model.ResearchSource) string {
	var parts []string
	for _, source := range sources {
		parts = append(parts, source.URL, normalize(source.Text))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}
