# 比赛资讯助手

一个可供多人使用的轻量级计算机比赛雷达。它每天检查官方页面、RSS 和搜索结果，先向匹配用户发送赛事预告和正式报名；用户明确选择参加后，再发送该赛事的正式开赛提醒。每封邮件都带可回查的来源链接。

普通用户只需要验证自己的收件邮箱，不需要提交邮箱授权码或 AI API Key。发件邮箱和模型由部署者在服务器环境变量中统一配置，避免在网页和数据库中保存用户的高敏感凭据。赛事、用户偏好、事实证据、网页观察记录和通知去重记录保存在 SQLite 中。

## 能做什么

- 重点跟踪 CCF CSP/CCSP/CAT、CCPC、ICPC、GPLT、蓝桥杯、计算机设计/系统能力/信息安全赛事、中国研究生创新实践系列，以及华为、天池、百度之星等可信企业赛事；各省程序设计、计算机设计和网络安全赛事通过分层搜索发现。
- 使用 SearXNG 搜索 AI Agent、RAG、大模型应用、Go 后端、云计算、云原生、软件开发和黑客松，也会主动发现企业、全国程序设计以及省市级地方赛事。
- 报名阶段与比赛阶段分别维护：报名可以处于预告、开放或截止，比赛可以同时处于即将开赛、进行中或已结束；页面仍展示一个便于阅读的综合标签。
- 预告变成正式报名时再次通知。用户可在网站或邮件确认页选择参加/不参加并随时修改；只有已选择参加的用户会继续收到赛题发布、即将开赛和正式开赛提醒。
- 同一赛事只维护一条规范记录，后续预告、报名和开赛页面直接更新该记录及报名/比赛日期；同一轮的重复来源不会产生重复通知，发送失败会保留并重试。
- 大模型先识别文档类型与来源角色，再提取赛事身份、届次和事件，不直接决定最终状态。日期有效性、状态推导、去重、持久化和发送记录均由普通程序完成。
- 所有日期、主办方、组队规则和比赛内容必须绑定同一届赛事，并带可回查的连续原文证据。列表页、校内转发、赛后新闻、届次冲突或无法回查的模型结论不会更新正式赛事记录。
- HTML 抓取会优先定位 `article`、正文容器和发布日期，导航栏及链接密集列表只用于发现详情链接，不参与赛事状态判断；完整页面文本仍保留在观察快照中供审计。
- `competitions.facts_json` 按字段保存规范值、原文、证据、届次、来源 URL、可信度和观察时间；同级来源冲突时清空对应事实，不用任意一方覆盖。
- 长 HTML 和 PDF 会按段落或页码切成证据块，最多选择 4 个与报名、日期、资格、费用和开赛相关的高价值块进行分析，模型并发上限为 2；PDF 证据保留页码。
- 字段可信度由程序根据来源等级、原文证据、届次和发布日期计算，不采用模型自报的置信度。不同分块给出冲突值时字段会清空并记录拒绝原因。
- `observations.analysis_result_json` 保存分析器版本、模型名称、输入哈希、分块编号、受限长度的原始 JSON、接受字段和拒绝原因，便于定位误判；它随网页观察快照使用同一保留策略清理。
- 分析协议带版本号；判断规则升级后，即使网页内容没有变化也会重新分析一次，并可清除旧版本留下的错误状态或日期。
- 提供邮箱验证码登录和响应式网页，可选择算法、AI 与数据、软件开发、云原生、黑客松、安全、硬件与物联网、创新创业等类型。
- 每位用户还可按企业/政府学协会/高校公开赛事/技术社区、国际/全国/地方/线上范围和省级地区筛选，并设置包含/排除关键词、最低可信度、资格风险、事件与投递频率。
- 学校学院仅面向本校学生的转发通知，以及落幕、收官、获奖名单、赛后回顾、选手专访等新闻在采集阶段直接过滤；历史存量也不会展示或触发提醒。
- 全站只扫描和分析一次，再对用户做确定性匹配；不会因为用户数量增加而重复抓网页或重复调用大模型。
- 用户通知使用独立唯一键，比赛事件入库和通知入队在同一个 SQLite 事务中完成；真正发送前还会重新检查用户选择、报名截止和比赛结束时间，过期队列会取消而不是发出。
- 新用户或用户修改偏好后，系统会在不重新抓取网页的情况下匹配数据库中仍然有效的赛事，并补建该用户从未收到过的通知；重复保存设置不会重复入队。
- 每项赛事保存结构化关键词。自定义关键词会同时搜索正文、标签和 AI 分析，并对“后端/服务端/backend”“智能体/AI Agent”等有限同义词做可解释匹配，不使用难以审计的向量阈值强行推荐。
- 仅对首次入库的新赛事执行定性研究：可通过 SearXNG 查找牛客、论坛、技术社区和视频平台公开材料，分析适合人群、技能、难度与简历实践价值。二手资料永远不能覆盖报名时间、费用、资格、主办方和规则。

## 采用的开源组件

- [SearXNG](https://github.com/searxng/searxng)：发现未知比赛。
- [Apprise API](https://github.com/caronc/apprise-api)：发送邮件，后续可换成其他 Apprise 通知渠道。
- [OpenAI Go SDK](https://github.com/openai/openai-go)：调用 OpenAI 兼容的 Chat Completions 接口。
- [gofeed](https://github.com/mmcdole/gofeed)、[goquery](https://github.com/PuerkitoBio/goquery)、[robfig/cron](https://github.com/robfig/cron)：RSS、HTML 和定时任务。
- [modernc SQLite](https://pkg.go.dev/modernc.org/sqlite)：无需 CGO 的 SQLite 驱动。
- [Poppler](https://poppler.freedesktop.org/) `pdftotext`：从官方 PDF 通知提取文字。

[RSSHub](https://github.com/DIYgod/RSSHub) 和 [changedetection.io](https://github.com/dgtlmoon/changedetection.io) 不随默认 Compose 启动，但它们产生的 RSS 可以直接配置为本项目的 `rss` 来源。n8n、Huginn、TrendRadar 和 Horizon 已评估，第一版没有引入，避免重复的调度器、数据库和新闻简报逻辑。

## 快速启动

要求：Docker 和 Docker Compose。

1. 创建本地配置：

   ```powershell
   Copy-Item .env.example .env
   Copy-Item sources.example.yaml sources.yaml
   ```

2. 编辑 `.env`：

   - 配置发件邮箱。QQ 邮箱通常使用 SMTP 授权码而不是登录密码。多人网页模式使用 `APPRISE_SENDER_URL`，程序会在发送时替换收件人：

     ```dotenv
     APPRISE_SENDER_URL=mailtos://sender%40qq.com:授权码@smtp.qq.com:465/?from=sender%40qq.com&mode=ssl
     ```

     `APPRISE_STATELESS_URLS` 只用于兼容原来的单收件人模式；启用网页后可以留空。不要把授权码发给普通用户，也不要提交 `.env`。

   - 启用多人网页，并生成独立随机密钥：

     ```dotenv
     WEB_LISTEN_ADDR=:8080
     WEB_PORT=8080
     PUBLIC_BASE_URL=http://你的服务器公网IP:8080
     APP_SECRET=至少32个随机字符
     ```

     Linux 可用 `openssl rand -hex 32` 生成 `APP_SECRET`。有域名时应在 Caddy/Nginx 后启用 HTTPS，并把 `PUBLIC_BASE_URL` 改成真实的 `https://` 地址。

   - 如需大模型，必须同时填写：

     ```dotenv
     OPENAI_BASE_URL=https://api.example.com/v1
     OPENAI_API_KEY=your-key
     OPENAI_MODEL=your-model-name
     ```

     三项任一为空时会自动使用纯规则模式。项目不内置模型名称。

   - 将 `SEARXNG_SECRET` 换成随机长字符串。

3. 启动：

   ```powershell
   docker compose up -d --build
   ```

   应用启动后只启动网页和调度器，不会因用户访问而扫描。系统在 `Asia/Shanghai` 每天 20:00 统一扫描一次；用户通知队列每分钟检查一次，以支持即时投递和自定义汇总时间。第一次扫描只通知仍有效的预告或报名；已选择参加的赛事后续进入进行中状态时才会发送正式开赛提醒。历史已截止、已结束或校内公告只建立观察基线或直接过滤。

   云服务器还需要在安全组/防火墙中放行 `WEB_PORT`（默认 8080）。然后访问 `http://服务器公网IP:8080`，输入邮箱验证码并设置提醒偏好。

4. 查看日志：

   ```powershell
   docker compose logs -f app
   docker compose logs -f apprise
   ```

5. 停止服务：

   ```powershell
   docker compose down
   ```

   SQLite 位于 Docker 命名卷 `competition_data`，普通 `docker compose down` 不会删除它。不要使用 `docker compose down -v`，除非确认要删除全部赛事和通知记录。

## 网页与多人订阅

网页没有复杂管理后台，只有完成目标所需的界面：邮箱验证码登录、订阅概览、提醒偏好、参赛选择确认和退订确认。

订阅概览会显示当前待发送通知数量，并提供两个受登录和 CSRF 保护的操作：

- `发送测试邮件`：向当前已验证邮箱发送独立测试邮件，验证 Apprise 与 SMTP 配置；一分钟内不能重复发送，且不会创建赛事事件或影响去重记录；
- `立即推送`：只把当前用户已经入队但尚未发送的新增通知立即投递，不抓取网页、不调用模型，也不会影响其他用户。

每个赛事卡片和每封预告/报名邮件都提供“参加”和“不参加”。邮件链接先进入确认页，避免邮箱安全扫描器访问链接时直接修改状态。选择可随时更改；选择不参加会取消该赛事尚未发送的通知，选择参加后才会跟进正式开赛。

邮件操作按钮只在 `PUBLIC_BASE_URL` 是公网域名或公网 IP 时启用。若配置为 `localhost`、`127.0.0.1` 或内网 IP，QQ 等邮箱的安全跳转服务无法访问该地址，邮件会改为提示用户在本地网站中选择，不再生成无效按钮。部署到云服务器后将 `PUBLIC_BASE_URL` 改成实际公网地址（推荐 HTTPS）并重启即可启用邮件按钮。

每位用户可以设置：

- 比赛类型：算法、AI 与数据、软件开发、云计算与云原生、黑客松、网络安全、硬件与物联网、创新创业、综合计算机；
- 主办方类型：企业、政府与学协会、高校公开赛事、开源与技术社区；
- 赛事范围：国际、全国、地方、线上开放，并可只关注重庆、四川等指定省级地区；
- 必须包含和需要排除的自定义关键词；
- 来源最低可信度，以及是否保留可能存在学历资格风险的重点赛事；
- 是否接收预告和正式报名；正式开赛提醒由用户对具体赛事选择“参加”后自动启用；
- 新通知发现后尽快发送、每天按时发送或每周按时发送，以及北京时间的投递时刻。该设置只控制用户邮件投递，不控制系统扫描。

扫描频率仍由部署者的 `APP_SCHEDULE` 统一控制，默认是每天 20:00。地区偏好只约束地方赛事：例如填写“重庆”会排除其他省市的地方赛，但不会排除用户已勾选的全国、国际或线上开放赛事。用户选择的频率是“邮件何时投递”，不会为每位用户重复运行爬虫或模型。修改筛选条件后，已经排队但不再匹配的邮件会被取消；仍匹配的待发事件会按新时间重新调度。

保存偏好时还会执行一次用户级回填：只读取 SQLite 中仍有效的赛事，按新偏好补充该用户从未接收过的状态事件，不触发联网扫描或模型调用。回填事件仍受 `(user_id, competition_id, event_type, event_key)` 唯一约束保护。

邮箱登录使用 6 位一次性验证码，10 分钟过期、最多尝试 5 次，同一邮箱 60 秒内不能重复发送。会话 Cookie 为 `HttpOnly`、`SameSite=Lax`，HTTPS 部署时自动加 `Secure`；设置修改和退出均校验 CSRF。邮件底部提供带 HMAC 签名的一键退订链接。

公开提供服务时还建议：

- 使用 Caddy 或 Nginx 提供 HTTPS，不直接长期暴露明文 HTTP；
- 在反向代理增加按 IP 的验证码接口限速；
- 仅向公网暴露网页端口，不暴露 Apprise、SearXNG 和 SQLite；
- 定期备份 `competition_data` 卷，并妥善保管 `.env`；
- 若更换 `APP_SECRET`，现有会话和旧退订链接会失效，但用户数据不会丢失。

## 管理员手动扫描一次

网页不向普通用户提供扫描权限。下面的命令只供部署者排查来源或验收配置时使用。

为避免与常驻调度任务并发写 SQLite，先停止应用容器，再运行一次性命令：

```powershell
docker compose stop app
docker compose run --rm app run-once
docker compose start app
```

Go 程序提供三个命令：

- `serve`：启动网页、通知投递器和系统定时扫描任务，不在启动时额外扫描。
- `run-once`：管理员扫描一次，完成后退出。
- `reset-competition-data`：仅清空赛事、来源观察、事件和相关通知，保留用户、会话与偏好；必须同时设置 `CONFIRM_RESET_COMPETITION_DATA=YES`，防止误操作。

重建赛事基线前应先备份 SQLite，再执行：

```powershell
docker compose stop app
docker compose run --rm -e CONFIRM_RESET_COMPETITION_DATA=YES app reset-competition-data
docker compose start app
```

网页模式可以关闭：把 `WEB_LISTEN_ADDR` 留空即可恢复原来的无网页单收件人模式，此时使用 `APPRISE_STATELESS_URLS` 投递。数据库迁移是向前兼容的，已有比赛和旧通知记录会保留。

## 来源配置

默认配置见 [`sources.example.yaml`](sources.example.yaml)。每个来源至少需要：

```yaml
sources:
  - id: example-official
    name: 示例比赛官网
    kind: page       # page、rss 或 search
    url: https://example.edu.cn/competitions
    trust: medium    # 非 search 来源可显式设置 high/medium
    limit: 30
```

RSSHub 或 changedetection.io：

```yaml
sources:
  - id: example-rsshub
    name: RSSHub 转换的活动源
    kind: rss
    url: http://rsshub:1200/your/route
    trust: medium
```

新赛事定性研究在 `enrichment` 中配置。建议只加入允许公开检索的社区域名，并限制数量：

```yaml
enrichment:
  enabled: true
  max_sources: 5
  allowed_domains:
    - nowcoder.com
    - tieba.baidu.com
    - zhihu.com
    - juejin.cn
    - bilibili.com
    - douyin.com
```

系统优先抓取结果正文，抓取失败时只使用搜索摘要。模型输出的每项分析都必须提供输入材料中的连续证据和原始 URL；程序会验证证据确实存在，否则丢弃该结论。社区内容只用于定性参考，邮件和页面会明确标为“AI 赛事分析”，并展示可回查来源。

搜索来源：

```yaml
sources:
  - id: discover-agent
    name: AI Agent 比赛发现
    kind: search
    query: '({year} OR 新一届) (AI Agent OR RAG) (比赛 OR 黑客松) (报名 OR 预告)'
    limit: 20
    allowed_domains:
      - edu.cn
      - gov.cn
```

`{year}` 会由普通程序替换为当前年份。`allowed_domains` 为空时可以发现新域名，但只有下列来源会发邮件：

- `high_trust_domains` 中的赛事、主办方或企业官方域名；
- `medium_trust_domains`、`.edu.cn`、`.gov.cn` 的官方承办/高校/政府页面。

其他页面仅保存为低可信候选，不触发邮件。要让新发现的企业官网触发提醒，应先确认其归属，再把域名加入高或中可信列表。

默认搜索同时包含已确认的大型技术企业域名，以及不限定域名的中小企业赛事候选搜索。后者先以低可信观察记录保存；部署者确认主办方和官网归属后再加入可信域名，避免仅凭公司名称向用户发送伪造或转载信息。

## 抓取健壮性与来源告警

抓取请求对临时网络错误和 `429/5xx` 响应使用指数退避重试（默认额外 `2` 次，可通过 `fetch.max_retries` 调整），并识别验证码、JavaScript 挑战等反爬页面——这类页面会被标记为 `ErrAntiBot`，不会进入 AI 分析，避免浪费算力。请求统一携带描述性的 `User-Agent` 和 `Accept-Language` 头。

```yaml
fetch:
  timeout_seconds: 20
  max_bytes: 5242880
  max_candidates_per_source: 40
  max_retries: 2   # 指数退避重试次数
```

当某个数据源在连续多次扫描中都失败时，系统会向部署者发送一封来源健康告警邮件，列出所有失效来源及其连续失败次数。单次偶发失败不会触发；来源恢复成功后计数自动清零。

```yaml
alert:
  enabled: true
  consecutive_failure_limit: 3   # 连续失败多少次后告警
```

同一来源的同一轮故障只会告警一次，恢复后再次故障才会重新告警。

### 无年份赛事的推送

有些比赛官网的公告标题不标注年份（例如"腾讯云黑客松官网"）。为兼顾"新赛事能推送"与"往届残留不误报"，系统使用**新鲜度窗口**判断：若页面在最近 `discovery.announcement_freshness_days` 天内**首次**被系统看到，则视为当前届公告并推送；超过窗口的页面视为往届残留，不推送。标题或内容已明确标注当前/未来年份的比赛不受此窗口影响。

```yaml
discovery:
  announcement_freshness_days: 90
```

## 通知字段与去重规则

每封邮件包含：比赛名称、主办方、当前状态、报名开始/截止、比赛开始/结束、是否组队、比赛费用、主要内容、推荐原因、资格提示、结构化关键词、AI 赛事分析、分析依据链接、官方链接和可信度。费用和日期只接受带原文证据的明确内容；原文没有发布的字段显示“暂未公布”。

赛事先按实体键和已见官方 URL 精确匹配，再按年份、届次、规范化名称及主办方做谨慎合并。全局事件使用 `(competition_id, event_type, event_key)` 唯一约束；用户通知再使用 `(user_id, competition_id, event_type, event_key)` 唯一约束，所以：

- 相同页面重复抓取不会重发；
- 同一比赛的预告页、报名页和开赛页可以归并为一条赛事记录，但不同年份或不同届次不会合并；
- 预告和正式报名属于两个事件，会分别通知；
- 两个同级官方来源给出不同日期时，该日期会清空为“暂未公布”，也不会用来判断有效期；
- 邮件发送失败保持为 `failed`，下一次扫描先重试，不会另建重复事件。
- 同一赛事可以分别提醒不同偏好的用户，但同一用户永远不会重复收到同一个事件。
- 全局事件记录与所有匹配用户的待发送记录在一个事务内提交，异常退出不会造成“记录了事件却没有入队”的空窗。
- 未选择参加的用户在报名截止后不再接收该赛事通知；已选择参加的用户可接收开赛提醒，但比赛结束后任何用户都不再接收。

## 数据库自动清洗

系统在每次管理员扫描结束后自动执行保守清洗，默认保留策略如下：

- 网页变化快照保留 30 天，但每个来源 URL 的最新一份快照始终保留；
- 已截止或已结束超过 180 天的赛事清空大段原始正文，但保留赛事名称、状态、日期、官方链接、分析和事实证据；
- 已发送超过 180 天的旧版单用户邮件清空 HTML 正文，但保留事件唯一键；
- 已过期超过 7 天的验证码和登录会话直接删除；
- `competition_events`、`user_notifications`、赛事来源和官方事实不删除，以继续保证通知去重和信息可追溯。

保留期可在 `sources.yaml` 的 `retention` 段或环境变量中调整。允许的环境变量为
`CLEANUP_ENABLED`、`OBSERVATION_RETENTION_DAYS`、
`CLOSED_COMPETITION_CONTENT_RETENTION_DAYS` 和
`EXPIRED_AUTH_RETENTION_DAYS`。清洗释放的 SQLite 页面会自动供后续写入复用；程序不会在在线服务中自动执行可能长时间锁库的 `VACUUM`。

清洗结果会写入容器日志：

```powershell
docker compose logs app | Select-String "database cleanup completed"
```

## 本地开发与测试

要求 Go 1.25。运行：

```powershell
go test ./...
go vet ./...
go build ./cmd/competition-assistant
docker compose config
```

测试全部使用 `httptest` 和内存通知器，不访问真实比赛网站、不调用真实模型、不发送真实邮件。覆盖：

- CSP 新报名；
- 华为赛事预告；
- 预告变正式报名；
- 重复运行不重复通知；
- 搜索发现新的 AI Agent 比赛；
- 过滤非计算机比赛；
- 过滤学院内部转发、落幕回顾和通用栏目页；
- 企业/全国/地方赛事分类和省级地区匹配；
- 报名截止后的队列发送拦截，以及比赛结束后的全量拦截；
- 邮件失败重试；
- OpenAI 兼容接口故障降级；
- 同级官方来源日期冲突；
- PDF 调用 `pdftotext`。
- 邮箱验证码登录、CSRF 和偏好保存；
- 不同比赛分类只投递给匹配用户；
- 多用户通知去重与预告固定提示语。
- 网站和邮件确认页的参加/不参加选择，以及只有参加者收到正式开赛提醒。
- 赛事数据重置保留用户账号和偏好。

## 运行边界

- Go 采集器本身不执行网页 JavaScript。对必须渲染、登录或点击后才能读取的页面，使用 changedetection.io/Playwright 生成变化 RSS，再交给本项目判断。
- SearXNG 的搜索覆盖取决于启用的搜索引擎及其可用性；固定官方页面监控不依赖搜索结果。
- 本项目只根据公开页面发送提醒，不代替报名系统中的最终资格确认。收到邮件后应点击官方链接复核。
- `.env` 包含邮件授权码和模型密钥，已被 `.gitignore` 排除；不要在日志、配置 YAML 或提交记录中保存真实凭据。

## 许可证

项目使用 [MIT License](LICENSE)，可以自由使用、修改和分发，但请保留许可证与版权声明。
