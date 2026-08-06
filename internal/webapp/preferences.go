package webapp

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"competition-assistant/internal/model"
	"competition-assistant/internal/subscription"
)

func (s *Server) preferences(w http.ResponseWriter, request *http.Request) {
	user, sessionToken, ok := s.requireUser(w, request)
	if !ok {
		return
	}
	preferences, err := s.store.GetUserPreferences(request.Context(), user.ID)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	message := ""
	if request.URL.Query().Get("welcome") == "1" {
		message = "邮箱验证成功。请确认你想关注的比赛类型和提醒频率。"
	}
	s.renderPreferences(w, http.StatusOK, user, sessionToken, preferences, message, "")
}

func (s *Server) savePreferences(w http.ResponseWriter, request *http.Request) {
	user, sessionToken, ok := s.requireUser(w, request)
	if !ok {
		return
	}
	if !s.parseAndVerifyCSRF(w, request, sessionToken) {
		return
	}
	preferences, err := parsePreferences(request, user.ID)
	if err != nil {
		s.renderPreferences(w, http.StatusBadRequest, user, sessionToken, preferences, "", err.Error())
		return
	}
	now := s.now()
	if err := s.store.SaveUserPreferences(request.Context(), preferences, now); err != nil {
		s.internalError(w, request, err)
		return
	}
	pending, err := s.store.ListUserPendingItems(request.Context(), user.ID)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	var cancelled []int64
	for _, item := range pending {
		classification := subscription.Profile(item.Competition)
		if len(subscription.MatchingEventsForUser(preferences, item.Competition, classification, []model.Event{item.Event}, item.Decision, s.now())) == 0 {
			cancelled = append(cancelled, item.NotificationID)
		}
	}
	if err := s.store.CancelUserNotifications(request.Context(), user.ID, cancelled); err != nil {
		s.internalError(w, request, err)
		return
	}
	dueAt, err := subscription.NextDelivery(now, preferences)
	if err == nil {
		group := subscription.DeliveryGroupKey(user.ID, 0, preferences.Frequency, dueAt, "rescheduled")
		err = s.store.RescheduleUserPending(request.Context(), user.ID, dueAt, group)
	}
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	backfilled := 0
	if s.backfill != nil {
		backfilled, err = s.backfill(request.Context(), user.ID)
		if err != nil {
			s.log.Error("user competition backfill failed", "user_id", user.ID, "error", err)
			s.renderPreferences(w, http.StatusInternalServerError, user, sessionToken, preferences, "设置已保存。", "赛事匹配刷新失败，请稍后重新保存设置。")
			return
		}
	}
	message := "设置已保存。"
	if backfilled > 0 {
		message = fmt.Sprintf("设置已保存，已补充 %d 条尚未向你推送的有效赛事动态。", backfilled)
	}
	s.renderPreferences(w, http.StatusOK, user, sessionToken, preferences, message, "")
}

func (s *Server) renderPreferences(w http.ResponseWriter, status int, user model.User, sessionToken string, preferences model.UserPreferences, message, errorMessage string) {
	selectedCategories := make(map[string]bool, len(preferences.Categories))
	for _, category := range preferences.Categories {
		selectedCategories[category] = true
	}
	selectedOrganizers := selectedOptions(preferences.OrganizerTypes, subscription.OrganizerTypeIDs())
	selectedScopes := selectedOptions(preferences.CompetitionScopes, subscription.CompetitionScopeIDs())
	if len(preferences.Categories) == 0 {
		for _, category := range subscription.CategoryIDs() {
			selectedCategories[category] = true
		}
	}
	s.render(w, status, "preferences.html", pageData{
		Title:              "提醒偏好",
		CSRF:               s.auth.CSRFToken(sessionToken),
		User:               user,
		Preferences:        preferences,
		Categories:         subscription.Categories(),
		OrganizerTypes:     subscription.OrganizerTypes(),
		Scopes:             subscription.CompetitionScopes(),
		SelectedCategories: selectedCategories,
		SelectedOrganizers: selectedOrganizers,
		SelectedScopes:     selectedScopes,
		IncludeText:        strings.Join(preferences.IncludeKeywords, "，"),
		ExcludeText:        strings.Join(preferences.ExcludeKeywords, "，"),
		RegionsText:        strings.Join(preferences.Regions, "，"),
		Message:            message,
		Error:              errorMessage,
	})
}

func parsePreferences(request *http.Request, userID int64) (model.UserPreferences, error) {
	categories := request.Form["categories"]
	seenCategories := map[string]bool{}
	validCategories := make([]string, 0, len(categories))
	for _, category := range categories {
		if !subscription.ValidCategory(category) || seenCategories[category] {
			continue
		}
		seenCategories[category] = true
		validCategories = append(validCategories, category)
	}
	if len(validCategories) == 0 {
		return model.UserPreferences{UserID: userID}, errors.New("请至少选择一种比赛类型。")
	}
	organizerTypes := validFormOptions(request.Form["organizer_types"], subscription.ValidOrganizerType)
	if len(organizerTypes) == 0 {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, errors.New("请至少选择一种主办方类型。")
	}
	competitionScopes := validFormOptions(request.Form["competition_scopes"], subscription.ValidCompetitionScope)
	if len(competitionScopes) == 0 {
		return model.UserPreferences{UserID: userID, Categories: validCategories, OrganizerTypes: organizerTypes}, errors.New("请至少选择一种赛事范围。")
	}
	regions, err := subscription.NormalizeRegions([]string{request.FormValue("regions")})
	if err != nil {
		return model.UserPreferences{UserID: userID, Categories: validCategories, OrganizerTypes: organizerTypes, CompetitionScopes: competitionScopes}, err
	}
	frequency := model.DeliveryFrequency(request.FormValue("frequency"))
	if frequency != model.DeliveryImmediate && frequency != model.DeliveryDaily && frequency != model.DeliveryWeekly {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, errors.New("提醒频率无效。")
	}
	deliveryTime := "08:00"
	if frequency != model.DeliveryImmediate {
		deliveryTime = strings.TrimSpace(request.FormValue("delivery_time"))
		if _, err := time.Parse("15:04", deliveryTime); err != nil {
			return model.UserPreferences{UserID: userID, Categories: validCategories, Frequency: frequency}, errors.New("提醒时间无效。")
		}
	}
	weeklyDay := 1
	if frequency == model.DeliveryWeekly {
		parsedWeeklyDay, err := strconv.Atoi(request.FormValue("weekly_day"))
		if err != nil || parsedWeeklyDay < 0 || parsedWeeklyDay > 6 {
			return model.UserPreferences{UserID: userID, Categories: validCategories, Frequency: frequency, DeliveryTime: deliveryTime}, errors.New("每周投递日期无效。")
		}
		weeklyDay = parsedWeeklyDay
	}
	timezone := strings.TrimSpace(request.FormValue("timezone"))
	if _, err := time.LoadLocation(timezone); err != nil || len(timezone) > 64 {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, errors.New("时区无效。")
	}
	trust := model.Trust(request.FormValue("min_trust"))
	if trust != model.TrustHigh && trust != model.TrustMedium {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, errors.New("可信度设置无效。")
	}
	includeKeywords, err := subscription.NormalizeKeywords([]string{request.FormValue("include_keywords")})
	if err != nil {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, err
	}
	excludeKeywords, err := subscription.NormalizeKeywords([]string{request.FormValue("exclude_keywords")})
	if err != nil {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, err
	}
	preferences := model.UserPreferences{
		UserID:                userID,
		Frequency:             frequency,
		DeliveryTime:          deliveryTime,
		WeeklyDay:             time.Weekday(weeklyDay),
		Timezone:              timezone,
		MinTrust:              trust,
		AllowEligibilityRisk:  request.FormValue("allow_eligibility_risk") == "1",
		NotifyPreview:         request.FormValue("notify_preview") == "1",
		NotifyRegistration:    request.FormValue("notify_registration") == "1",
		NotifyUpcoming:        request.FormValue("notify_upcoming") == "1",
		NotifyStarted:         request.FormValue("notify_started") == "1",
		NotifyProblemRelease:  request.FormValue("notify_problem_release") == "1",
		NotifyDeadline7Days:   request.FormValue("notify_deadline_7d") == "1",
		NotifyDeadline1Day:    request.FormValue("notify_deadline_1d") == "1",
		NotifyImportantUpdate: request.FormValue("notify_important_update") == "1",
		Categories:            validCategories,
		OrganizerTypes:        organizerTypes,
		CompetitionScopes:     competitionScopes,
		Regions:               regions,
		IncludeKeywords:       includeKeywords,
		ExcludeKeywords:       excludeKeywords,
	}
	if !preferences.NotifyPreview && !preferences.NotifyRegistration && !preferences.NotifyUpcoming && !preferences.NotifyStarted && !preferences.NotifyProblemRelease &&
		!preferences.NotifyDeadline7Days && !preferences.NotifyDeadline1Day && !preferences.NotifyImportantUpdate {
		return preferences, errors.New("请至少选择一种需要提醒的赛事动态。")
	}
	return preferences, nil
}

func selectedOptions(selected, defaults []string) map[string]bool {
	if len(selected) == 0 {
		selected = defaults
	}
	result := make(map[string]bool, len(selected))
	for _, value := range selected {
		result[value] = true
	}
	return result
}

func validFormOptions(values []string, valid func(string) bool) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if valid(value) && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
