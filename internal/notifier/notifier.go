package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"competition-assistant/internal/model"
)

type Sender interface {
	Send(context.Context, string, string) error
}

type RecipientSender interface {
	Sender
	SendTo(context.Context, string, string, string) error
}

type Apprise struct {
	url       string
	senderURL string
	client    *http.Client
}

func NewApprise(apiURL string, senderURLs ...string) *Apprise {
	senderURL := ""
	if len(senderURLs) > 0 {
		senderURL = strings.TrimSpace(senderURLs[0])
	}
	return &Apprise{url: apiURL, senderURL: senderURL, client: &http.Client{Timeout: 20 * time.Second}}
}

func (a *Apprise) Send(ctx context.Context, subject, body string) error {
	return a.send(ctx, "", subject, body)
}

func (a *Apprise) SendTo(ctx context.Context, recipient, subject, body string) error {
	if a.senderURL == "" {
		return errors.New("APPRISE_SENDER_URL is required for per-user delivery")
	}
	address, err := mail.ParseAddress(strings.TrimSpace(recipient))
	if err != nil || !strings.EqualFold(address.Address, strings.TrimSpace(recipient)) {
		return fmt.Errorf("invalid recipient email")
	}
	parsed, err := url.Parse(a.senderURL)
	if err != nil {
		return fmt.Errorf("parse APPRISE_SENDER_URL: %w", err)
	}
	query := parsed.Query()
	query.Set("to", address.Address)
	parsed.RawQuery = query.Encode()
	return a.send(ctx, parsed.String(), subject, body)
}

func (a *Apprise) send(ctx context.Context, targetURL, subject, body string) error {
	payloadData := map[string]string{"title": subject, "body": body, "format": "html", "type": "info"}
	if targetURL != "" {
		payloadData["urls"] = targetURL
	}
	payload, err := json.Marshal(payloadData)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return fmt.Errorf("apprise has no notification target configured (HTTP 204)")
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("apprise returned %s", resp.Status)
	}
	return nil
}

type deliveryCompetition struct {
	Name             string
	Organizer        string
	Status           string
	Start            string
	End              string
	CompetitionStart string
	CompetitionEnd   string
	Team             string
	Fee              string
	Content          string
	Reason           string
	Eligibility      string
	OfficialURL      string
	Trust            string
	Keywords         string
	Analysis         model.CompetitionAnalysis
	AnalysisTrust    string
	PreviewNotice    string
	Events           []string
	ParticipateURL   string
	DeclineURL       string
}

type CompetitionChoiceLinks struct {
	ParticipateURL string
	DeclineURL     string
}

type deliveryTemplateData struct {
	Competitions   []deliveryCompetition
	ManageURL      string
	UnsubscribeURL string
}

var deliveryTemplate = template.Must(template.New("delivery").Parse(`<!doctype html>
<html lang="zh-CN"><body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Microsoft YaHei',sans-serif;color:#1f2937;line-height:1.65;max-width:760px;margin:auto;background:#f8fafc;padding:20px">
<div style="background:#ffffff;border-radius:14px;padding:24px"><h2 style="margin-top:0;color:#0f766e">比赛资讯助手</h2>
<p>以下比赛符合你设置的关注方向：</p>
{{range .Competitions}}<section style="border:1px solid #e2e8f0;border-radius:10px;padding:18px;margin:16px 0">
<h3 style="margin:0 0 8px;color:#0f172a">{{.Name}}</h3>
{{if .PreviewNotice}}<p style="padding:10px;background:#fff7ed;border-left:4px solid #f97316"><strong>{{.PreviewNotice}}</strong></p>{{end}}
<p><strong>本次提醒：</strong>{{range $i,$v := .Events}}{{if $i}}、{{end}}{{$v}}{{end}}</p>
<p><strong>主办方：</strong>{{.Organizer}}<br><strong>当前状态：</strong>{{.Status}}<br><strong>报名开始：</strong>{{.Start}}<br><strong>报名截止：</strong>{{.End}}<br><strong>比赛开始：</strong>{{.CompetitionStart}}<br><strong>比赛结束：</strong>{{.CompetitionEnd}}<br><strong>是否组队：</strong>{{.Team}}<br><strong>比赛费用：</strong>{{.Fee}}<br><strong>主要内容：</strong>{{.Content}}<br><strong>推荐原因：</strong>{{.Reason}}<br><strong>参赛资格：</strong>{{.Eligibility}}<br><strong>信息可信度：</strong>{{.Trust}}</p>
{{if .Analysis.Summary}}<div style="padding:12px;background:#f0f7f5;border-radius:6px"><strong>AI 赛事分析（{{.AnalysisTrust}}）</strong><p>{{.Analysis.Summary}}</p>{{if .Analysis.SuitableFor}}<p><strong>适合人群：</strong>{{.Analysis.SuitableFor}}</p>{{end}}{{if .Analysis.Difficulty}}<p><strong>难度判断：</strong>{{.Analysis.Difficulty}}</p>{{end}}{{if .Analysis.ResumeValue}}<p><strong>简历价值判断：</strong>{{.Analysis.ResumeValue}}</p>{{end}}{{if .Analysis.Caveats}}<p><strong>注意事项：</strong>{{.Analysis.Caveats}}</p>{{end}}{{if .Keywords}}<p><strong>关键词：</strong>{{.Keywords}}</p>{{end}}{{if .Analysis.References}}<p><strong>分析依据：</strong>{{range $i,$v := .Analysis.References}}{{if $i}} · {{end}}<a href="{{$v.URL}}">{{$v.Title}}</a>{{end}}</p>{{end}}</div>{{end}}
<p><a href="{{.OfficialURL}}" style="display:inline-block;padding:8px 13px;background:#0f766e;color:white;text-decoration:none;border-radius:5px">查看官方来源</a></p>
{{if .ParticipateURL}}<p><strong>你准备参加吗？</strong><br><a href="{{.ParticipateURL}}" style="display:inline-block;margin:6px 8px 0 0;padding:8px 13px;background:#0f766e;color:white;text-decoration:none;border-radius:5px">参加比赛</a><a href="{{.DeclineURL}}" style="display:inline-block;margin-top:6px;padding:8px 13px;border:1px solid #94a3b8;color:#334155;text-decoration:none;border-radius:5px">不参加</a></p>
<p style="font-size:12px;color:#64748b">选择参加后才会收到正式开赛提醒。链接会先进入确认页，选择可在网站中修改。</p>{{else}}<p style="padding:10px;background:#f1f5f9;color:#475569"><strong>参赛选择：</strong>当前是本地或内网部署，邮箱无法安全跳转到网站。请打开比赛资讯助手，在赛事卡片中选择“参加”或“不参加”。</p>{{end}}
</section>{{end}}
<p style="font-size:12px;color:#64748b">时间和规则仅采用官方页面中能找到原文证据的内容；未公布或存在冲突的字段会显示“暂未公布”。</p>
{{if .ManageURL}}<p style="font-size:12px"><a href="{{.ManageURL}}">管理订阅偏好</a> · <a href="{{.UnsubscribeURL}}">停止接收提醒</a></p>{{else}}<p style="font-size:12px;color:#64748b">订阅偏好和退订操作请在比赛资讯助手网站中完成。</p>{{end}}
</div></body></html>`))

var verificationTemplate = template.Must(template.New("verification").Parse(`<!doctype html>
<html lang="zh-CN"><body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Microsoft YaHei',sans-serif;color:#1f2937;line-height:1.65;max-width:560px;margin:auto">
<h2 style="color:#0f766e">登录比赛资讯助手</h2>
<p>你的邮箱验证码是：</p><p style="font-size:32px;font-weight:700;letter-spacing:6px">{{.Code}}</p>
<p>验证码在 {{.Minutes}} 分钟内有效。若不是你本人操作，请忽略本邮件。</p>
</body></html>`))

var testMailTemplate = template.Must(template.New("test-mail").Parse(`<!doctype html>
<html lang="zh-CN"><body style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Microsoft YaHei',sans-serif;color:#1f2937;line-height:1.65;max-width:560px;margin:auto">
<h2 style="color:#0f766e">比赛资讯助手测试邮件</h2>
<p>这封邮件说明你的收件邮箱和平台发件服务连接正常。</p>
<p><strong>测试时间：</strong>{{.Time}}</p>
<p style="font-size:12px;color:#6b7280">测试邮件不会创建赛事通知记录，也不会影响正式比赛的去重状态。</p>
</body></html>`))

func RenderUserDelivery(group model.UserDeliveryGroup, manageURL, unsubscribeURL string, choiceLinks map[int64]CompetitionChoiceLinks) (string, string, error) {
	byCompetition := make(map[int64]int)
	data := deliveryTemplateData{ManageURL: manageURL, UnsubscribeURL: unsubscribeURL}
	for _, item := range group.Items {
		index, exists := byCompetition[item.Competition.ID]
		if !exists {
			links := choiceLinks[item.Competition.ID]
			competition := deliveryCompetition{
				Name:             missing(item.Competition.Name),
				Organizer:        missing(item.Competition.Organizer),
				Status:           statusLabel(item.Competition.Status),
				Start:            missing(item.Competition.RegistrationStartRaw),
				End:              missing(item.Competition.RegistrationEndRaw),
				CompetitionStart: missing(item.Competition.CompetitionStartRaw),
				CompetitionEnd:   missing(item.Competition.CompetitionEndRaw),
				Team:             missing(item.Competition.TeamRequirement),
				Fee:              missing(item.Competition.Fee),
				Content:          missing(item.Competition.Content),
				Reason:           missing(item.Competition.FitReason),
				Eligibility:      missing(item.Competition.EligibilityNote),
				OfficialURL:      item.Competition.OfficialURL,
				Trust:            trustLabel(item.Competition.Trust),
				Keywords:         strings.Join(item.Competition.Keywords, "、"),
				Analysis:         item.Competition.Analysis,
				AnalysisTrust:    analysisConfidenceLabel(item.Competition.Analysis.Confidence),
				ParticipateURL:   links.ParticipateURL,
				DeclineURL:       links.DeclineURL,
			}
			if item.Competition.Status == model.StatusPreview {
				competition.PreviewNotice = "目前是预告，尚未正式开放报名。"
			}
			data.Competitions = append(data.Competitions, competition)
			index = len(data.Competitions) - 1
			byCompetition[item.Competition.ID] = index
		}
		if item.Event.Type == "preview_detected" {
			data.Competitions[index].PreviewNotice = "目前是预告，尚未正式开放报名。"
		}
		data.Competitions[index].Events = append(data.Competitions[index].Events, eventLabel(item.Event.Type))
	}
	if len(data.Competitions) == 0 {
		return "", "", errors.New("cannot render an empty user delivery group")
	}
	var body bytes.Buffer
	if err := deliveryTemplate.Execute(&body, data); err != nil {
		return "", "", err
	}
	subject := fmt.Sprintf("[比赛提醒] 发现 %d 个符合你偏好的比赛动态", len(data.Competitions))
	return subject, body.String(), nil
}

func RenderVerification(code string, ttl time.Duration) (string, string, error) {
	minutes := int(ttl.Round(time.Minute) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	var body bytes.Buffer
	if err := verificationTemplate.Execute(&body, struct {
		Code    string
		Minutes int
	}{Code: code, Minutes: minutes}); err != nil {
		return "", "", err
	}
	return "[比赛资讯助手] 邮箱验证码", body.String(), nil
}

func RenderTest(now time.Time) (string, string, error) {
	var body bytes.Buffer
	if err := testMailTemplate.Execute(&body, struct{ Time string }{Time: now.Format("2006-01-02 15:04:05 MST")}); err != nil {
		return "", "", err
	}
	return "[比赛资讯助手] 测试邮件", body.String(), nil
}

func statusLabel(status model.Status) string {
	switch status {
	case model.StatusPreview:
		return "预告"
	case model.StatusUpcoming:
		return "即将开赛"
	case model.StatusRegistrationOpen:
		return "报名中"
	case model.StatusRegistrationClosed:
		return "报名已截止"
	case model.StatusOngoing:
		return "进行中"
	case model.StatusFinished:
		return "已结束"
	default:
		return "暂未公布"
	}
}

func eventLabel(event string) string {
	switch event {
	case "competition_discovered":
		return "发现新赛事（报名状态待确认）"
	case "preview_detected":
		return "发现赛事预告"
	case "registration_opened":
		return "正式开放报名"
	case "competition_upcoming":
		return "即将开赛"
	case "competition_started":
		return "正式开赛"
	case "problem_released":
		return "赛题发布"
	case "deadline_7d":
		return "报名截止进入 7 天窗口"
	case "deadline_1d":
		return "报名截止进入 1 天窗口"
	case "important_update":
		return "重要报名信息更新"
	default:
		return event
	}
}

func trustLabel(trust model.Trust) string {
	switch trust {
	case model.TrustHigh:
		return "高（赛事或主办方官方来源）"
	case model.TrustMedium:
		return "中（高校、政府、官方承办方或官方赛事平台）"
	default:
		return "低（仅作为候选保存，不应触发本邮件）"
	}
}

func analysisConfidenceLabel(confidence string) string {
	switch confidence {
	case "high":
		return "较高可信，仅依据可回查材料"
	case "medium":
		return "中等可信，包含有依据的 AI 判断"
	case "low":
		return "低可信，仅供参考"
	default:
		return "尚未评估"
	}
}

func missing(value string) string {
	if strings.TrimSpace(value) == "" {
		return "暂未公布"
	}
	return value
}
