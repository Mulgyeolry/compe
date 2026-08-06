package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"competition-assistant/internal/model"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.runMigrations(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// RecordObservationVersioned treats a newer analyzer version as a meaningful
// change even when the fetched bytes are identical. This lets corrected
// extraction logic revisit previously classified pages without deleting the
// crawler baseline or fabricating a content change.
func (s *Store) RecordObservationVersioned(ctx context.Context, sourceID string, doc model.Document, hash string, trust model.Trust, analyzerVersion string, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var previousHash, previousVersion string
	err = tx.QueryRowContext(ctx, `SELECT last_hash,analyzer_version FROM source_documents WHERE source_id=? AND url=?`, sourceID, doc.URL).Scan(&previousHash, &previousVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if previousHash == hash && previousVersion == analyzerVersion {
		if _, err := tx.ExecContext(ctx, `UPDATE source_documents SET last_seen=? WHERE source_id=? AND url=?`, now.Unix(), sourceID, doc.URL); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO source_documents(source_id,url,last_hash,analyzer_version,last_seen) VALUES(?,?,?,?,?)
ON CONFLICT(source_id,url) DO UPDATE SET last_hash=excluded.last_hash,analyzer_version=excluded.analyzer_version,last_seen=excluded.last_seen`, sourceID, doc.URL, hash, analyzerVersion, now.Unix()); err != nil {
		return false, err
	}
	content := doc.RawText
	if content == "" {
		content = doc.Text
	}
	if runes := []rune(content); len(runes) > 65536 {
		content = string(runes[:65536])
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO observations(source_id,url,title,content_hash,content,trust,analyzer_version,seen_at) VALUES(?,?,?,?,?,?,?,?)`, sourceID, doc.URL, doc.Title, hash, content, string(trust), analyzerVersion, now.Unix()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// RetryDocumentOnNextScan removes only the change-detection baseline. The
// observation snapshot remains available for auditing, while the same page is
// allowed through analysis again after a transient model failure.
func (s *Store) RetryDocumentOnNextScan(ctx context.Context, sourceID, documentURL string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM source_documents WHERE source_id=? AND url=?`, sourceID, documentURL)
	return err
}

func (s *Store) RecordAnalysisAudit(ctx context.Context, sourceID, documentURL, contentHash string, audit model.AnalysisAudit) error {
	if strings.TrimSpace(sourceID) == "" || strings.TrimSpace(documentURL) == "" || strings.TrimSpace(contentHash) == "" {
		return errors.New("analysis audit requires source, URL and content hash")
	}
	encoded := encodeJSON(audit, "{}")
	result, err := s.db.ExecContext(ctx, `UPDATE observations SET analysis_result_json=? WHERE id=(
SELECT id FROM observations WHERE source_id=? AND url=? AND content_hash=? ORDER BY seen_at DESC,id DESC LIMIT 1
)`, encoded, sourceID, documentURL, contentHash)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// FirstObservedAt returns the earliest seen_at recorded for a document URL
// across every source. It is used as the authoritative "when was this page
// first discovered" signal, which for a brand-new competition may predate the
// moment analysis finally succeeds. An empty URL is rejected with an error;
// a URL with no observation rows returns sql.ErrNoRows.
func (s *Store) FirstObservedAt(ctx context.Context, documentURL string) (time.Time, error) {
	if strings.TrimSpace(documentURL) == "" {
		return time.Time{}, errors.New("document URL must not be empty")
	}
	var seenAt *int64
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(seen_at) FROM observations WHERE url = ?`, documentURL).Scan(&seenAt)
	if err != nil {
		return time.Time{}, err
	}
	if seenAt == nil {
		// MIN() over an empty set yields NULL, which must not be coerced into
		// Unix time 0. Surface it as "not found" so callers can distinguish a
		// never-seen page from a page observed at the epoch.
		return time.Time{}, sql.ErrNoRows
	}
	return time.Unix(*seenAt, 0), nil
}

func (s *Store) UpsertCompetition(ctx context.Context, value model.Competition, sourceID string, now time.Time) (model.Competition, bool, error) {
	model.NormalizeLifecycle(&value)
	sourceURL, sourceTrust := value.OfficialURL, value.Trust
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Competition{}, false, err
	}
	defer tx.Rollback()
	old, err := findExistingCompetition(ctx, tx, value)
	isNew := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNew {
		return model.Competition{}, false, err
	}
	if isNew {
		// Preserve a FirstSeen that the caller already determined (e.g. the
		// page's earliest observation time) so a page whose analysis succeeded
		// days after it was first seen still records the original discovery
		// date rather than the analysis date. Fall back to now only when the
		// caller left it zero.
		if value.FirstSeen.IsZero() {
			value.FirstSeen = now
		}
		value.LastSeen = now
		err := tx.QueryRowContext(ctx, `INSERT INTO competitions(
entity_key,name,organizer,status,status_evidence,registration_phase,competition_phase,registration_start,registration_start_raw,registration_end,registration_end_raw,
competition_start,competition_start_raw,competition_end,competition_end_raw,team_requirement,fee,fee_evidence,keywords_json,analysis_json,facts_json,content,fit_score,fit_reason,eligibility_note,official_url,trust,problem_released,analyzer_version,content_hash,first_seen,last_seen)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`, competitionArgs(value)...).Scan(&value.ID)
		if err != nil {
			return model.Competition{}, false, err
		}
	} else {
		value.EntityKey = old.EntityKey
		value = resolveSourceConflicts(old, value)
		value.ID = old.ID
		value.FirstSeen = old.FirstSeen
		value.LastSeen = now
		_, err = tx.ExecContext(ctx, `UPDATE competitions SET name=?,organizer=?,status=?,status_evidence=?,registration_phase=?,competition_phase=?,registration_start=?,registration_start_raw=?,
registration_end=?,registration_end_raw=?,competition_start=?,competition_start_raw=?,competition_end=?,competition_end_raw=?,team_requirement=?,fee=?,fee_evidence=?,keywords_json=?,analysis_json=?,facts_json=?,content=?,fit_score=?,fit_reason=?,eligibility_note=?,official_url=?,trust=?,problem_released=?,analyzer_version=?,content_hash=?,last_seen=? WHERE id=?`,
			value.Name, value.Organizer, string(value.Status), value.StatusEvidence, string(value.RegistrationPhase), string(value.CompetitionPhase), nullableTime(value.RegistrationStart), value.RegistrationStartRaw,
			nullableTime(value.RegistrationEnd), value.RegistrationEndRaw, nullableTime(value.CompetitionStart), value.CompetitionStartRaw, nullableTime(value.CompetitionEnd), value.CompetitionEndRaw, value.TeamRequirement, value.Fee, value.FeeEvidence, encodeJSON(value.Keywords, "[]"), encodeJSON(value.Analysis, "{}"), encodeJSON(value.Facts, "{}"), value.Content, value.FitScore, value.FitReason,
			value.EligibilityNote, value.OfficialURL, string(value.Trust), boolInt(value.ProblemReleased), value.AnalyzerVersion, value.ContentHash, now.Unix(), value.ID)
		if err != nil {
			return model.Competition{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO competition_sources(competition_id,source_id,url,trust,last_seen) VALUES(?,?,?,?,?)
ON CONFLICT(competition_id,url) DO UPDATE SET source_id=excluded.source_id,trust=excluded.trust,last_seen=excluded.last_seen`, value.ID, sourceID, sourceURL, string(sourceTrust), now.Unix()); err != nil {
		return model.Competition{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.Competition{}, false, err
	}
	return old, isNew, nil
}

func findExistingCompetition(ctx context.Context, tx *sql.Tx, value model.Competition) (model.Competition, error) {
	if existing, err := loadCompetition(tx.QueryRowContext(ctx, competitionSelect+` WHERE entity_key=?`, value.EntityKey)); err == nil || !errors.Is(err, sql.ErrNoRows) {
		return existing, err
	}
	var competitionID int64
	err := tx.QueryRowContext(ctx, `SELECT competition_id FROM competition_sources WHERE url=? LIMIT 1`, value.OfficialURL).Scan(&competitionID)
	if err == nil {
		existing, loadErr := loadCompetition(tx.QueryRowContext(ctx, competitionSelect+` WHERE id=?`, competitionID))
		if loadErr != nil {
			return model.Competition{}, loadErr
		}
		// The URL is reused year after year for the same official site. If the
		// newly crawled announcement is an explicitly different year or edition,
		// it is a brand-new competition, not an update of the existing one. We
		// do not return it here and instead fall through to the identity match
		// below; if nothing else matches, sql.ErrNoRows lets the caller create a
		// fresh row rather than silently merging across editions.
		if explicitCompetitionEditionConflict(existing, value) {
			err = sql.ErrNoRows
		} else {
			return existing, nil
		}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Competition{}, err
	}
	rows, err := tx.QueryContext(ctx, competitionSelect+` ORDER BY last_seen DESC`)
	if err != nil {
		return model.Competition{}, err
	}
	defer rows.Close()
	for rows.Next() {
		candidate, err := loadCompetition(rows)
		if err != nil {
			return model.Competition{}, err
		}
		if sameCompetitionIdentity(candidate, value) {
			return candidate, nil
		}
	}
	if err := rows.Err(); err != nil {
		return model.Competition{}, err
	}
	return model.Competition{}, sql.ErrNoRows
}

var (
	identityYearPattern    = regexp.MustCompile(`20\d{2}`)
	identityEditionPattern = regexp.MustCompile(`第[一二三四五六七八九十百零〇\d]+届`)
)

func sameCompetitionIdentity(left, right model.Competition) bool {
	leftYear, rightYear := competitionYear(left.Name+" "+left.StatusEvidence), competitionYear(right.Name+" "+right.StatusEvidence)
	if leftYear != 0 && rightYear != 0 && leftYear != rightYear {
		return false
	}
	leftEdition, rightEdition := competitionEdition(left.Name), competitionEdition(right.Name)
	if leftEdition != "" && rightEdition != "" && leftEdition != rightEdition {
		return false
	}
	leftName, rightName := normalizedCompetitionName(left.Name), normalizedCompetitionName(right.Name)
	if len([]rune(leftName)) < 6 || len([]rune(rightName)) < 6 {
		return false
	}
	nameSimilarity := bigramDice(leftName, rightName)
	contained := strings.Contains(leftName, rightName) || strings.Contains(rightName, leftName)
	if !contained && nameSimilarity < 0.78 {
		return false
	}
	leftOrganizer, rightOrganizer := normalizedOrganizer(left.Organizer), normalizedOrganizer(right.Organizer)
	if leftOrganizer != "" && rightOrganizer != "" && leftOrganizer != rightOrganizer &&
		!strings.Contains(leftOrganizer, rightOrganizer) && !strings.Contains(rightOrganizer, leftOrganizer) && nameSimilarity < 0.92 {
		return false
	}
	return true
}

// explicitCompetitionEditionConflict reports whether two competitions are
// unmistakably different editions: either both carry a four-digit year that
// differs, or both carry an explicit "第X届" ordinal that differs. It is used
// to prevent a reused official URL from silently merging a new edition into an
// existing one. When only one side has a year or edition, there is no explicit
// conflict and the URL match is allowed to stand.
func explicitCompetitionEditionConflict(left, right model.Competition) bool {
	leftYear, rightYear := competitionYear(left.Name+" "+left.StatusEvidence), competitionYear(right.Name+" "+right.StatusEvidence)
	if leftYear != 0 && rightYear != 0 && leftYear != rightYear {
		return true
	}
	leftEdition, rightEdition := competitionEdition(left.Name), competitionEdition(right.Name)
	return leftEdition != "" && rightEdition != "" && leftEdition != rightEdition
}

func competitionEdition(text string) string {
	return identityEditionPattern.FindString(text)
}

func competitionYear(text string) int {
	match := identityYearPattern.FindString(text)
	if match == "" {
		return 0
	}
	var year int
	_, _ = fmt.Sscanf(match, "%d", &year)
	return year
}

func normalizedCompetitionName(value string) string {
	value = strings.ToLower(value)
	for _, noise := range []string{
		"关于组织学生参加", "关于组织我校学生参加", "关于组织参加", "关于举办", "关于开展", "报名通知", "延期通知",
		"正式开放报名", "开放报名", "开始报名", "报名预告", "即将启动", "敬请期待", "赛事预告", "赛题发布会",
		"正式开赛", "即将开赛", "启动仪式", "通知", "公告", "预告", "报名", "正式", "即将", "启动",
	} {
		value = strings.ReplaceAll(value, noise, "")
	}
	value = identityYearPattern.ReplaceAllString(value, "")
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func normalizedOrganizer(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func bigramDice(left, right string) float64 {
	leftPairs, rightPairs := runeBigrams(left), runeBigrams(right)
	if len(leftPairs) == 0 || len(rightPairs) == 0 {
		return 0
	}
	counts := make(map[string]int, len(leftPairs))
	for _, pair := range leftPairs {
		counts[pair]++
	}
	intersection := 0
	for _, pair := range rightPairs {
		if counts[pair] > 0 {
			intersection++
			counts[pair]--
		}
	}
	return 2 * float64(intersection) / float64(len(leftPairs)+len(rightPairs))
}

func runeBigrams(value string) []string {
	runes := []rune(value)
	if len(runes) < 2 {
		return nil
	}
	result := make([]string, 0, len(runes)-1)
	for index := 0; index < len(runes)-1; index++ {
		result = append(result, string(runes[index:index+2]))
	}
	return result
}

const competitionSelect = `SELECT id,entity_key,name,organizer,status,status_evidence,registration_phase,competition_phase,registration_start,registration_start_raw,
registration_end,registration_end_raw,competition_start,competition_start_raw,competition_end,competition_end_raw,team_requirement,fee,fee_evidence,content,fit_score,fit_reason,eligibility_note,official_url,trust,
keywords_json,analysis_json,facts_json,problem_released,analyzer_version,content_hash,first_seen,last_seen FROM competitions`

type scanner interface{ Scan(...any) error }

func loadCompetition(row scanner) (model.Competition, error) {
	var value model.Competition
	var status, registrationPhase, competitionPhase, trust string
	var keywordsJSON, analysisJSON, factsJSON string
	var start, end, competitionStart, competitionEnd sql.NullInt64
	var problem int
	var firstSeen, lastSeen int64
	err := row.Scan(&value.ID, &value.EntityKey, &value.Name, &value.Organizer, &status, &value.StatusEvidence, &registrationPhase, &competitionPhase, &start, &value.RegistrationStartRaw,
		&end, &value.RegistrationEndRaw, &competitionStart, &value.CompetitionStartRaw, &competitionEnd, &value.CompetitionEndRaw, &value.TeamRequirement, &value.Fee, &value.FeeEvidence, &value.Content, &value.FitScore, &value.FitReason, &value.EligibilityNote,
		&value.OfficialURL, &trust, &keywordsJSON, &analysisJSON, &factsJSON, &problem, &value.AnalyzerVersion, &value.ContentHash, &firstSeen, &lastSeen)
	if err != nil {
		return model.Competition{}, err
	}
	value.Status = model.Status(status)
	value.RegistrationPhase = model.RegistrationPhase(registrationPhase)
	value.CompetitionPhase = model.CompetitionPhase(competitionPhase)
	value.Trust = model.Trust(trust)
	value.ProblemReleased = problem == 1
	if err := json.Unmarshal([]byte(keywordsJSON), &value.Keywords); err != nil {
		return model.Competition{}, fmt.Errorf("decode competition keywords: %w", err)
	}
	if err := json.Unmarshal([]byte(analysisJSON), &value.Analysis); err != nil {
		return model.Competition{}, fmt.Errorf("decode competition analysis: %w", err)
	}
	if err := json.Unmarshal([]byte(factsJSON), &value.Facts); err != nil {
		return model.Competition{}, fmt.Errorf("decode competition facts: %w", err)
	}
	if start.Valid {
		parsed := time.Unix(start.Int64, 0)
		value.RegistrationStart = &parsed
	}
	if end.Valid {
		parsed := time.Unix(end.Int64, 0)
		value.RegistrationEnd = &parsed
	}
	if competitionStart.Valid {
		parsed := time.Unix(competitionStart.Int64, 0)
		value.CompetitionStart = &parsed
	}
	if competitionEnd.Valid {
		parsed := time.Unix(competitionEnd.Int64, 0)
		value.CompetitionEnd = &parsed
	}
	value.FirstSeen = time.Unix(firstSeen, 0)
	value.LastSeen = time.Unix(lastSeen, 0)
	model.NormalizeLifecycle(&value)
	return value, nil
}

func (s *Store) ListActiveCompetitions(ctx context.Context) ([]model.Competition, error) {
	rows, err := s.db.QueryContext(ctx, competitionSelect+` WHERE status IN ('preview','upcoming','registration_open','ongoing')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.Competition
	for rows.Next() {
		value, err := loadCompetition(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) GetCompetition(ctx context.Context, entityKey string) (model.Competition, error) {
	return loadCompetition(s.db.QueryRowContext(ctx, competitionSelect+` WHERE entity_key=?`, entityKey))
}

func (s *Store) GetCompetitionByID(ctx context.Context, competitionID int64) (model.Competition, error) {
	return loadCompetition(s.db.QueryRowContext(ctx, competitionSelect+` WHERE id=?`, competitionID))
}

func (s *Store) DeleteCompetition(ctx context.Context, competitionID int64) error {
	if competitionID < 1 {
		return errors.New("invalid competition id")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM competitions WHERE id=?`, competitionID)
	return err
}

func (s *Store) UpdateCompetitionEnrichment(ctx context.Context, competitionID int64, keywords []string, analysis model.CompetitionAnalysis) error {
	if competitionID < 1 {
		return errors.New("invalid competition id")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE competitions SET keywords_json=?,analysis_json=? WHERE id=?`,
		encodeJSON(keywords, "[]"), encodeJSON(analysis, "{}"), competitionID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ListCompetitions(ctx context.Context) ([]model.Competition, error) {
	rows, err := s.db.QueryContext(ctx, competitionSelect+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.Competition
	for rows.Next() {
		value, err := loadCompetition(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) IsBootstrapped(ctx context.Context) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='bootstrapped'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value == "true", err
}

func (s *Store) MarkBootstrapped(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('bootstrapped','true') ON CONFLICT(key) DO UPDATE SET value='true'`)
	return err
}

func competitionArgs(value model.Competition) []any {
	model.NormalizeLifecycle(&value)
	return []any{value.EntityKey, value.Name, value.Organizer, string(value.Status), value.StatusEvidence, string(value.RegistrationPhase), string(value.CompetitionPhase),
		nullableTime(value.RegistrationStart), value.RegistrationStartRaw, nullableTime(value.RegistrationEnd), value.RegistrationEndRaw,
		nullableTime(value.CompetitionStart), value.CompetitionStartRaw, nullableTime(value.CompetitionEnd), value.CompetitionEndRaw, value.TeamRequirement, value.Fee, value.FeeEvidence, encodeJSON(value.Keywords, "[]"), encodeJSON(value.Analysis, "{}"), encodeJSON(value.Facts, "{}"), value.Content, value.FitScore,
		value.FitReason, value.EligibilityNote, value.OfficialURL, string(value.Trust), boolInt(value.ProblemReleased), value.AnalyzerVersion, value.ContentHash, value.FirstSeen.Unix(), value.LastSeen.Unix()}
}

func encodeJSON(value any, fallback string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fallback
	}
	return string(encoded)
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.Unix()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func resolveSourceConflicts(old, current model.Competition) model.Competition {
	model.NormalizeLifecycle(&old)
	model.NormalizeLifecycle(&current)
	oldRank, currentRank := trustRank(old.Trust), trustRank(current.Trust)
	sameSource := old.OfficialURL == current.OfficialURL
	analyzerUpgraded := sameSource && current.AnalyzerVersion != "" && current.AnalyzerVersion != old.AnalyzerVersion
	if preferredCompetitionName(old.Name, current.Name) == old.Name {
		current.Name = old.Name
	}
	if current.Organizer == "" && !analyzerUpgraded {
		current.Organizer = old.Organizer
	}
	if current.StatusEvidence == "" && !analyzerUpgraded {
		current.Status, current.StatusEvidence = old.Status, old.StatusEvidence
	}
	if current.RegistrationPhase == model.RegistrationUnknown && !analyzerUpgraded {
		current.RegistrationPhase = old.RegistrationPhase
	}
	if current.CompetitionPhase == model.CompetitionUnknown && !analyzerUpgraded {
		current.CompetitionPhase = old.CompetitionPhase
	}
	if current.RegistrationStartRaw == "" && !analyzerUpgraded {
		current.RegistrationStart, current.RegistrationStartRaw = old.RegistrationStart, old.RegistrationStartRaw
	}
	if current.RegistrationEndRaw == "" && !analyzerUpgraded {
		current.RegistrationEnd, current.RegistrationEndRaw = old.RegistrationEnd, old.RegistrationEndRaw
	}
	if current.CompetitionStartRaw == "" && !analyzerUpgraded {
		current.CompetitionStart, current.CompetitionStartRaw = old.CompetitionStart, old.CompetitionStartRaw
	}
	if current.CompetitionEndRaw == "" && !analyzerUpgraded {
		current.CompetitionEnd, current.CompetitionEndRaw = old.CompetitionEnd, old.CompetitionEndRaw
	}
	if current.TeamRequirement == "" && !analyzerUpgraded {
		current.TeamRequirement = old.TeamRequirement
	}
	if current.Fee == "" && !analyzerUpgraded {
		current.Fee, current.FeeEvidence = old.Fee, old.FeeEvidence
	}
	current.Keywords = mergeKeywords(old.Keywords, current.Keywords)
	if !analysisPopulated(current.Analysis) {
		current.Analysis = old.Analysis
	}
	if current.Content == "" {
		current.Content = old.Content
	}
	if current.FitReason == "" {
		current.FitReason = old.FitReason
	}
	if current.EligibilityNote == "" && !analyzerUpgraded {
		current.EligibilityNote = old.EligibilityNote
	}
	if current.AnalyzerVersion == "" {
		current.AnalyzerVersion = old.AnalyzerVersion
	}
	if analyzerUpgraded {
		current.Facts = cloneFacts(current.Facts)
	} else {
		current.Facts = mergeFacts(old.Facts, current.Facts, sameSource || currentRank >= oldRank)
	}
	model.NormalizeLifecycle(&current)
	if !sameSource && lifecycleRank(old.Status) > lifecycleRank(current.Status) && oldRank >= currentRank {
		current.Status, current.StatusEvidence = old.Status, old.StatusEvidence
		current.RegistrationPhase, current.CompetitionPhase = old.RegistrationPhase, old.CompetitionPhase
	}
	if oldRank > currentRank {
		current.OfficialURL, current.Trust = old.OfficialURL, old.Trust
	}
	if sameSource {
		model.NormalizeLifecycle(&current)
		return current
	}
	if old.RegistrationStartRaw != "" && current.RegistrationStartRaw != "" && old.RegistrationStartRaw != current.RegistrationStartRaw {
		switch {
		case oldRank > currentRank:
			current.RegistrationStart, current.RegistrationStartRaw = old.RegistrationStart, old.RegistrationStartRaw
		case oldRank == currentRank:
			current.RegistrationStart, current.RegistrationStartRaw = nil, ""
			delete(current.Facts, model.FactRegistrationStart)
		}
	}
	if old.RegistrationEndRaw != "" && current.RegistrationEndRaw != "" && old.RegistrationEndRaw != current.RegistrationEndRaw {
		switch {
		case oldRank > currentRank:
			current.RegistrationEnd, current.RegistrationEndRaw = old.RegistrationEnd, old.RegistrationEndRaw
		case oldRank == currentRank:
			current.RegistrationEnd, current.RegistrationEndRaw = nil, ""
			delete(current.Facts, model.FactRegistrationEnd)
		}
	}
	if old.CompetitionStartRaw != "" && current.CompetitionStartRaw != "" && old.CompetitionStartRaw != current.CompetitionStartRaw {
		switch {
		case oldRank > currentRank:
			current.CompetitionStart, current.CompetitionStartRaw = old.CompetitionStart, old.CompetitionStartRaw
		case oldRank == currentRank:
			current.CompetitionStart, current.CompetitionStartRaw = nil, ""
			delete(current.Facts, model.FactCompetitionStart)
		}
	}
	if old.CompetitionEndRaw != "" && current.CompetitionEndRaw != "" && old.CompetitionEndRaw != current.CompetitionEndRaw {
		switch {
		case oldRank > currentRank:
			current.CompetitionEnd, current.CompetitionEndRaw = old.CompetitionEnd, old.CompetitionEndRaw
		case oldRank == currentRank:
			current.CompetitionEnd, current.CompetitionEndRaw = nil, ""
			delete(current.Facts, model.FactCompetitionEnd)
		}
	}
	if old.TeamRequirement != "" && current.TeamRequirement != "" && old.TeamRequirement != current.TeamRequirement {
		switch {
		case oldRank > currentRank:
			current.TeamRequirement = old.TeamRequirement
		case oldRank == currentRank:
			current.TeamRequirement = ""
			delete(current.Facts, model.FactTeamRequirement)
		}
	}
	if old.Fee != "" && current.Fee != "" && old.Fee != current.Fee {
		switch {
		case oldRank > currentRank:
			current.Fee, current.FeeEvidence = old.Fee, old.FeeEvidence
		case oldRank == currentRank:
			current.Fee, current.FeeEvidence = "", ""
			delete(current.Facts, model.FactFee)
		}
	}
	model.NormalizeLifecycle(&current)
	return current
}

func cloneFacts(values map[string]model.FactEvidence) map[string]model.FactEvidence {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]model.FactEvidence, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func mergeFacts(old, current map[string]model.FactEvidence, preferCurrent bool) map[string]model.FactEvidence {
	result := cloneFacts(old)
	if result == nil && len(current) > 0 {
		result = make(map[string]model.FactEvidence, len(current))
	}
	for key, value := range current {
		if _, exists := result[key]; !exists || preferCurrent {
			result[key] = value
		}
	}
	return result
}

func lifecycleRank(status model.Status) int {
	switch status {
	case model.StatusPreview:
		return 10
	case model.StatusRegistrationOpen:
		return 20
	case model.StatusRegistrationClosed:
		return 30
	case model.StatusUpcoming:
		return 40
	case model.StatusOngoing:
		return 50
	case model.StatusFinished:
		return 60
	default:
		return 0
	}
}

func analysisPopulated(value model.CompetitionAnalysis) bool {
	return value.Summary != "" || value.SuitableFor != "" || len(value.Skills) > 0 || len(value.References) > 0
}

func mergeKeywords(groups ...[]string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, group := range groups {
		for _, keyword := range group {
			keyword = strings.TrimSpace(keyword)
			key := strings.ToLower(keyword)
			if keyword == "" || seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, keyword)
		}
	}
	return result
}

func preferredCompetitionName(oldName, currentName string) string {
	oldNoise := strings.Contains(oldName, "关于") || strings.Contains(oldName, "通知") || strings.Contains(oldName, "敬请期待")
	currentNoise := strings.Contains(currentName, "关于") || strings.Contains(currentName, "通知") || strings.Contains(currentName, "敬请期待")
	if oldNoise != currentNoise {
		if oldNoise {
			return currentName
		}
		return oldName
	}
	if len([]rune(oldName)) <= len([]rune(currentName)) {
		return oldName
	}
	return currentName
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
