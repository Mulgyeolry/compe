package webapp

import (
	"net/http"
	"strconv"
	"strings"

	"competition-assistant/internal/model"
)

func (s *Server) setWebsiteCompetitionChoice(w http.ResponseWriter, request *http.Request) {
	user, sessionToken, ok := s.requireUser(w, request)
	if !ok {
		return
	}
	if !s.parseAndVerifyCSRF(w, request, sessionToken) {
		return
	}
	competitionID, err := strconv.ParseInt(request.FormValue("competition_id"), 10, 64)
	decision := model.ParticipationDecision(request.FormValue("decision"))
	if err != nil || competitionID < 1 || !validParticipationDecision(decision) {
		http.Error(w, "参赛选择无效。", http.StatusBadRequest)
		return
	}
	if s.setChoice == nil {
		http.Redirect(w, request, "/dashboard?result=choice-unavailable", http.StatusSeeOther)
		return
	}
	if err := s.setChoice(request.Context(), user.ID, competitionID, decision); err != nil {
		s.log.Warn("website competition choice failed", "user_id", user.ID, "competition_id", competitionID, "error", err)
		http.Redirect(w, request, "/dashboard?result=choice-failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, request, "/dashboard?result=choice-saved", http.StatusSeeOther)
}

func (s *Server) competitionChoicePage(w http.ResponseWriter, request *http.Request) {
	token := strings.TrimSpace(request.URL.Query().Get("token"))
	userID, competitionID, decisionRaw, valid := s.auth.VerifyCompetitionChoiceToken(token)
	if !valid {
		http.Error(w, "链接无效或已损坏。", http.StatusBadRequest)
		return
	}
	competition, err := s.store.GetCompetitionByID(request.Context(), competitionID)
	if err != nil {
		http.Error(w, "比赛不存在或已清理。", http.StatusNotFound)
		return
	}
	if _, err := s.store.GetUserPreferences(request.Context(), userID); err != nil {
		http.Error(w, "用户不存在。", http.StatusNotFound)
		return
	}
	s.render(w, http.StatusOK, "competition-choice.html", pageData{Title: "确认参赛选择", ChoiceCompetition: competition.Name, ChoiceDecision: participationDecisionLabel(model.ParticipationDecision(decisionRaw)), ChoiceToken: token})
}

func (s *Server) confirmCompetitionChoice(w http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(w, request.Body, maxFormBytes)
	if err := request.ParseForm(); err != nil {
		http.Error(w, "请求无效。", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(request.FormValue("token"))
	userID, competitionID, decisionRaw, valid := s.auth.VerifyCompetitionChoiceToken(token)
	decision := model.ParticipationDecision(decisionRaw)
	if !valid || !validParticipationDecision(decision) {
		http.Error(w, "链接无效或已损坏。", http.StatusBadRequest)
		return
	}
	if s.setChoice == nil {
		http.Error(w, "当前无法保存参赛选择。", http.StatusServiceUnavailable)
		return
	}
	if err := s.setChoice(request.Context(), userID, competitionID, decision); err != nil {
		s.log.Warn("email competition choice failed", "user_id", userID, "competition_id", competitionID, "error", err)
		http.Error(w, "比赛已经结束，或当前无法保存选择。", http.StatusConflict)
		return
	}
	s.render(w, http.StatusOK, "competition-choice-saved.html", pageData{Title: "参赛选择已保存", ChoiceDecision: participationDecisionLabel(decision)})
}

func validParticipationDecision(decision model.ParticipationDecision) bool {
	return decision == model.ParticipationParticipating || decision == model.ParticipationDeclined
}

func participationDecisionLabel(decision model.ParticipationDecision) string {
	if decision == model.ParticipationParticipating {
		return "参加比赛"
	}
	return "不参加比赛"
}
