package workbuddy

// tasks_cn.go P4 国内版运营自动化：成长/积分任务（growth）、夜猫子（night）、
// 猫猫旅行（cat）。
//
// 上游事实来源：wb2a 的 wb_tasks.py（commit 6c2a663）。本文件只做协议搬运：
// 路径、请求体形状、头、业务码判定逐条对齐 wb_tasks.py，代码结构按 KeyGrid 的
// 任务框架重排。
//
// 红线（与 jobs.go / state.go 一致）：
//  1. 不落内容：Message / 日志只允许状态、计数、结构化上游错误码、耗时；绝不包含
//     prompt、事件 payload、响应正文或凭据。
//  2. 失败不污染熔断：本文件不调用 Runtime.Observe / Admit 之外的写路径。
//  3. 每步最多重试一次；最终结果以 (status, message) 返回，不向框架抛 error。
//  4. INTL 直接 skipped，不发起任何请求。
//
// 与 wb_tasks.py 的刻意取舍（写在此处，避免后来者当 bug 修）：
//   - 事件上报按「每个任务一个 /v2/report 批」发送（对齐设计 §3.7「逐类上报」），
//     而不是 wb_tasks.py:527-536 的「一个事件一次请求」；同批内保证
//     (eventCode, id) 互异（wb_tasks.py:78-80 指出上游按此判重）。
//   - growth 收尾不再顺手跑猫猫旅行（wb_tasks.py:573-579）：KeyGrid 把 cat 拆成
//     独立任务，避免一个 job 的失败面耦合到另一个。
//   - 事件 id 池跨重启去重落 WorkbuddyChannelState.Meta（wb_tasks.py 只按进度取模，
//     不持久化），满足设计 §3.7「事件去重池」。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"
)

// ---- 端点常量（上游路径逐条对齐 wb_tasks.py） ----

const (
	// cnWebBase 国内版 web 域，仅用于领奖 400 降级（wb_tasks.py:19 WEB_BASE）。
	cnWebBase = "https://www.workbuddy.cn"

	// cnWebUA web 域降级请求的 User-Agent（wb_tasks.py:21 WEB_UA）。
	cnWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

	// growthTasksPath 成长任务清单（wb_tasks.py:104）。
	growthTasksPath = "/v2/activity/growth/tasks"
	// growthAcceptPath 批量接取（wb_tasks.py:177）。body {"task_codes":[...]}，每批 ≤20。
	growthAcceptPath = "/v2/activity/growth/tasks/accept"
	// growthClaimPathFmt 领奖（wb_tasks.py:228）。★注意上游无 /v2 前缀★
	growthClaimPathFmt = "/activity/growth/tasks/%s/claim"
	// reportPath 行为事件上报（wb_tasks.py:356）。body 为事件对象数组。
	reportPath = "/v2/report"

	// 猫猫旅行四个端点（wb_tasks.py:373/383/400/411）。
	travelStatusPath = "/activity/growth/buddy/travel/status"
	travelClaimPath  = "/activity/growth/buddy/travel/claim"
	travelConfigPath = "/activity/growth/buddy/travel/config"
	travelDepartPath = "/activity/growth/buddy/travel/depart"

	// nightTaskCode 夜猫子任务码（wb_tasks.py:72 NIGHT_TASK_CODES）。
	nightTaskCode = "black_cat"

	// growthGap 上报/领奖步骤之间的防风控间隔（wb_tasks.py:428 gap=1.0）。
	growthGap = time.Second
	// growthReportSettle 上报后等上游落账再领奖的固定间隔（wb_tasks.py:539 1.5s）。
	growthReportSettle = 1500 * time.Millisecond
	// maxReportEvents 单次 /v2/report 的事件数上限（防止异常大的批）。
	maxReportEvents = 20
	// maxStoredEventIDs Meta 去重池长度上限（只影响可复用 id 的条数，超出丢最旧）。
	maxStoredEventIDs = 256
	// metaKeyGrowthUsedEventIDs 事件 id 去重池的 Meta 键（跨重启去重）。
	metaKeyGrowthUsedEventIDs = "growthUsedEventIDs"
)

// ---- 测试接缝（package-level；生产等价于真实基址） ----

var (
	// cnTaskBaseURL 成长/夜猫子/猫猫旅行请求的 chat 侧基址（生产 copilot.tencent.com，wb_tasks.py:17）。
	cnTaskBaseURL = func(realm Realm) string { return defaultChatBase(realm) }
	// cnClaimWebURL 领奖 400 降级到 web 域的基址（生产 www.workbuddy.cn，wb_tasks.py:19）。
	cnClaimWebURL = func() string { return cnWebBase }
)

// ---- 任务规格与事件表（对齐 wb_tasks.py:37-99） ----

// growthTaskSpec 单个成长任务的静态规格。
type growthTaskSpec struct {
	Kind        string // 事件种类（buildEvent 的 kind）
	Target      int    // 默认目标次数（上游未回 target 时回落）
	Name        string // 展示名
	Unforgeable bool   // 不可伪造（真实动作），不接取、不上报
}

// growthTaskSpecs 任务码 → 规格（wb_tasks.py:37-56 TASK_SPECS，仅保留本移植用到的字段）。
var growthTaskSpecs = map[string]growthTaskSpec{
	"create_canvas":       {Kind: "canvas", Target: 1, Name: "创建设计任务"},
	"template_5":          {Kind: "template", Target: 5, Name: "模板创建任务"},
	"expert_5":            {Kind: "expert", Target: 5, Name: "使用专家助手"},
	"Expert_team_use_3":   {Kind: "team", Target: 3, Name: "使用专家团队"},
	"skill_1":             {Kind: "skill", Target: 1, Name: "体验技能"},
	"automation_1":        {Kind: "automation", Target: 1, Name: "创建自动化任务"},
	"playbook_prompt":     {Kind: "playbook", Target: 1, Name: "灵感案例使用"},
	"Expert_lighthouse":   {Kind: "lighthouse", Target: 1, Name: "轻量云专家使用"},
	"Buddy_App":           {Kind: "buddy5", Target: 1, Name: "进入 Buddy 应用"},
	"Buddy_App_QQ":        {Kind: "buddy5", Target: 1, Name: "企鹅教师助手"},
	"Hp_Appearance":       {Kind: "skin", Target: 1, Name: "应用主题外观"},
	"chat_5":              {Kind: "chat", Target: 5, Name: "发起 5 次对话"},
	"Model_chat_GLM5.2":   {Kind: "glmchat", Target: 1, Name: "体验 GLM-5.2"},
	"black_cat":           {Kind: "cat", Target: 3, Name: "夜猫子任务 (23:00-08:00)"},
	"RichMeow_Chat":       {Kind: "richmeow", Target: 1, Name: "桌面对话事件链"},
	"Library_read":        {Kind: "library", Target: 1, Name: "浏览资料库"},
	"first_buddy":         {Kind: "buddy_first", Target: 1, Name: "领养首只猫猫"},
	"Expert_Philanthropy": {Unforgeable: true, Name: "公益爱心捐赠"},
}

// desktopOnlyTasks 只认桌面端真实点击的任务（wb_tasks.py:58-69 注释：伪造 /v2/report
// 事件会被忽略或落到 heartbeat，进度永远是 0/1，claim 必然 400 task not completed）。
var desktopOnlyTasks = map[string]string{
	"RichMeow_Chat": "在桌面端发起 1 次对话",
	"Library_read":  "在桌面端打开「资料库」并读完介绍文档",
	"Buddy_App":     "在桌面端左上角「发现应用」进入任意一个 Buddy 应用",
	"Buddy_App_QQ":  "在桌面端「发现应用」进入「企鹅教师助手」",
}

// expertID 专家/团队事件 id（wb_tasks.py:81-99 实测可推进进度的 id 池）。
type expertID struct {
	ID   string
	Name string
}

// expertIDPool 专家 id 池（wb_tasks.py:81-91）。
var expertIDPool = []expertID{
	{"ex_PZw8Gu81HfN4", "运维工程师"}, {"ex_ROsDtJbzADFV", "产品经理"},
	{"ex_SMUnl0nJbPix", "UI设计师"}, {"ex_ZTR062oVBOCW", "数据分析师"},
	{"ex_a3sSSFBy8qaC", "后端架构师"}, {"ex_aG1kvKbq8lPx", "文案策划"},
	{"ex_al1vxtUOYQ10", "测试专家"}, {"ex_cZfiyuET9UQP", "安全顾问"},
	{"ex_eggOvQuVP0hq", "算法工程师"}, {"ex_hSwsQjkSKnkX", "前端工程师"},
	{"ex_mMbwwmFA9n9P", "项目管理专家"}, {"ex_uAQE5POfk7Zh", "增长运营专家"},
	{"ex_uZzSAScSy7FZ", "行业研究员"}, {"ex_LHywGrZOtG7G", "数据分析师"},
	{"ex_NX5C8GBciVed", "测试架构师"}, {"ex_DdCsaoq4AtcO", "云端运维专家"},
	{"ex_KzqKQguubrNQ", "内容创作专家"}, {"ex_2cvvUZQhDyeJ", "腾讯轻量云专家"},
}

// teamIDPool 团队 id 池（wb_tasks.py:94-99）。
var teamIDPool = []expertID{
	{"CloudOpsTeam", "运维专家团队"}, {"CloudContentTeam", "内容专家团队"},
	{"CloudDevTeam", "研发专家团队"}, {"ProductStrategyTeam", "产品战略团队"},
	{"MarketingCampaignTeam", "营销活动团队"}, {"SalesBattleTeam", "销售作战团队"},
	{"DesignEngineTeam", "设计引擎团队"}, {"HrOperationsTeam", "人力运营团队"},
}

// ---- 持久化去重池 ----

// cnMetaStore 成长任务事件 id 去重池的持久化能力（*op.Op 满足）。
type cnMetaStore interface {
	UpdateWorkbuddyMeta(userID, providerID int64, key string, value any, now time.Time) error
}

// 编译期断言：op 层签名变化会在这里立刻暴露。
var _ cnMetaStore = (*op.Op)(nil)

// loadUsedEventIDs 从运行态 Meta 读出已用事件 id（兼容 []string 与 JSON 反序列化的 []any）。
func loadUsedEventIDs(state *model.WorkbuddyChannelState) ([]string, map[string]bool) {
	set := map[string]bool{}
	var list []string
	if state == nil || state.Meta == nil {
		return list, set
	}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || set[s] {
			return
		}
		set[s] = true
		list = append(list, s)
	}
	switch v := state.Meta[metaKeyGrowthUsedEventIDs].(type) {
	case []string:
		for _, s := range v {
			add(s)
		}
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok {
				add(s)
			}
		}
	}
	return list, set
}

// ---- 运行器 ----

// growthRunner 一次 growth/night/cat 执行共享的上下文与状态。
type growthRunner struct {
	ctx   context.Context
	jc    *JobContext
	op    *op.Op
	rt    *Runtime
	http  *http.Client
	prov  *model.Provider
	tok   *oauth.TokenSet
	hdr   http.Header
	now   time.Time
	realm Realm

	usedList   []string        // 已用事件 id（有序，跨重启）
	usedSet    map[string]bool // 已用事件 id（判重）
	newUsedIDs []string        // 本轮新增，收尾写回 Meta
	rotate     int             // 池子用尽后的轮转计数
}

// newGrowthRunner 组装运行器并构建 chat 侧请求头。
func newGrowthRunner(ctx context.Context, jc *JobContext, realm Realm) (*growthRunner, error) {
	hdr, err := cnChatHeaders(jc.Provider, jc.Tok)
	if err != nil {
		return nil, fmt.Errorf("workbuddy: 构建成长任务请求头: %w", err)
	}
	now := jc.Now
	if now.IsZero() {
		now = time.Now()
	}
	usedList, usedSet := loadUsedEventIDs(jc.State)
	return &growthRunner{
		ctx:      ctx,
		jc:       jc,
		op:       jc.Op,
		rt:       jc.Runtime,
		http:     jc.httpClient(),
		prov:     jc.Provider,
		tok:      jc.Tok,
		hdr:      hdr,
		now:      now,
		realm:    realm,
		usedList: usedList,
		usedSet:  usedSet,
	}, nil
}

// base chat 侧基址（成长/夜猫子/猫猫旅行都用它，wb_tasks.py:104 等）。
func (g *growthRunner) base() string { return cnTaskBaseURL(g.realm) }

// metaStore 返回去重池持久化实现（*op.Op 优先；测试注入的 dbs 其次）。
func (g *growthRunner) metaStore() cnMetaStore {
	if g.op != nil {
		return g.op
	}
	if g.rt != nil && g.rt.dbs != nil {
		if ms, ok := g.rt.dbs.(cnMetaStore); ok {
			return ms
		}
	}
	return nil
}

// markUsed 记录一个已用事件 id。
func (g *growthRunner) markUsed(id string) {
	if id == "" || g.usedSet[id] {
		return
	}
	g.usedSet[id] = true
	g.usedList = append(g.usedList, id)
	g.newUsedIDs = append(g.newUsedIDs, id)
}

// flushUsedEventIDs 把本轮的 id 去重池写回 Meta（失败只告警，不使任务失败）。
func (g *growthRunner) flushUsedEventIDs() {
	if len(g.newUsedIDs) == 0 {
		return
	}
	store := g.metaStore()
	if store == nil {
		log.Printf("[workbuddy] growth 事件去重池无持久化通道 provider=%d（本轮仅内存生效）", g.prov.ID)
		return
	}
	list := g.usedList
	if len(list) > maxStoredEventIDs {
		list = list[len(list)-maxStoredEventIDs:]
	}
	if err := store.UpdateWorkbuddyMeta(g.prov.UserID, g.prov.ID, metaKeyGrowthUsedEventIDs, list, g.now); err != nil {
		log.Printf("[workbuddy] growth 事件去重池写入失败 provider=%d: %v", g.prov.ID, err)
	}
}

// pickExpertID 从池中挑一个未用过的 id；池子用尽则轮转（与 wb_tasks.py 取模一致）。
func (g *growthRunner) pickExpertID(pool []expertID) *expertID {
	for i := range pool {
		if !g.usedSet[pool[i].ID] {
			g.markUsed(pool[i].ID)
			return &pool[i]
		}
	}
	if len(pool) == 0 {
		return nil
	}
	ex := &pool[g.rotate%len(pool)]
	g.rotate++
	return ex
}

// ---- 三个导出任务入口 ----

// runGrowthJob CN 成长/积分任务状态机（设计文档 §3.7）。
//
// 对齐 wb_tasks.py:428-583：清单 → 接取（≤20/批）→ 逐任务上报事件 → 轮询进度 →
// 领奖（400 降级 web 域）。INTL 直接 skipped。
func runGrowthJob(ctx context.Context, jc *JobContext) (status string, message string) {
	if jc == nil || jc.Provider == nil || jc.Runtime == nil {
		return jobStatusFailed, "任务上下文缺失"
	}
	realm, ok := RealmOf(jc.Provider)
	if !ok {
		return jobStatusFailed, "渠道不属于 WorkBuddy"
	}
	if realm != RealmCN {
		return jobStatusSkipped, "国际版无该任务"
	}
	if jc.Tok == nil {
		return jobStatusFailed, "渠道凭据缺失"
	}
	g, err := newGrowthRunner(ctx, jc, realm)
	if err != nil {
		return jobStatusFailed, "构建成长任务请求头失败"
	}
	return g.runGrowth()
}

// runNightJob CN 夜猫子任务：与 growth 同一批行为事件，但只在 23:00–08:00 窗口内执行。
//
// 对齐 wb_tasks.py:586-633：窗口判定 → 清单 → black_cat 进度 → 上报 cat 事件 →
// 轮询 4 次 → 达标领奖。窗口外 skipped 且零请求。
func runNightJob(ctx context.Context, jc *JobContext) (status string, message string) {
	if jc == nil || jc.Provider == nil || jc.Runtime == nil {
		return jobStatusFailed, "任务上下文缺失"
	}
	realm, ok := RealmOf(jc.Provider)
	if !ok {
		return jobStatusFailed, "渠道不属于 WorkBuddy"
	}
	if realm != RealmCN {
		return jobStatusSkipped, "国际版无该任务"
	}
	if jc.Tok == nil {
		return jobStatusFailed, "渠道凭据缺失"
	}
	if !inNightWindow(jc.Now) {
		return jobStatusSkipped, "当前不在 23:00-08:00 夜间窗口"
	}
	g, err := newGrowthRunner(ctx, jc, realm)
	if err != nil {
		return jobStatusFailed, "构建夜猫子请求头失败"
	}
	return g.runNight()
}

// runCatJob CN 猫猫旅行：status/config → depart → claim。
//
// 对齐 wb_tasks.py:368-425。到达领奖、出行中/今日已达上限按 ok 记，不视为失败。
func runCatJob(ctx context.Context, jc *JobContext) (status string, message string) {
	if jc == nil || jc.Provider == nil || jc.Runtime == nil {
		return jobStatusFailed, "任务上下文缺失"
	}
	realm, ok := RealmOf(jc.Provider)
	if !ok {
		return jobStatusFailed, "渠道不属于 WorkBuddy"
	}
	if realm != RealmCN {
		return jobStatusSkipped, "国际版无该任务"
	}
	if jc.Tok == nil {
		return jobStatusFailed, "渠道凭据缺失"
	}
	g, err := newGrowthRunner(ctx, jc, realm)
	if err != nil {
		return jobStatusFailed, "构建猫猫旅行请求头失败"
	}
	return g.runCat()
}

// inNightWindow 夜猫子窗口：本地时间 23:00–08:00（wb_tasks.py:74-76）。
func inNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// ---- growth 状态机 ----

// runGrowth 执行成长任务全流程。
func (g *growthRunner) runGrowth() (string, string) {
	tasks, err := g.fetchTasks()
	if err != nil {
		return jobStatusFailed, truncateRunes("获取成长任务清单失败: "+err.Error(), maxJobMessageLen)
	}
	if len(tasks) == 0 {
		return jobStatusFailed, "成长任务清单为空"
	}

	out := g.acceptPending(tasks)
	tasks = out.tasks

	var reported, claimed, skipped, failed int
	failReason := ""
	setFail := func(reason string) {
		failed++
		if failReason == "" {
			failReason = reason
		}
	}

	for i := range tasks {
		t := tasks[i]
		spec, ok := growthTaskSpecs[t.Code]
		if !ok || spec.Unforgeable {
			continue
		}
		if t.Status == "claimed" {
			continue
		}
		// 已完成的任务先领奖（wb_tasks.py:489-498）。
		if t.Status == "completed" || t.Current >= t.Target {
			if okClaim, _, reason := g.claimTask(t.Code); okClaim {
				claimed++
			} else {
				setFail(reason)
			}
			sleep(growthGap)
			continue
		}
		// 只认桌面端真实点击的任务（wb_tasks.py:501-505）。
		if _, desktop := desktopOnlyTasks[t.Code]; desktop {
			skipped++
			continue
		}
		// 夜猫子任务白天不计数（wb_tasks.py:508-511）。
		if t.Code == nightTaskCode && !inNightWindow(g.now) {
			skipped++
			continue
		}
		// 接取失败就上报等于白费（wb_tasks.py:513-516）。
		if t.Status == "not_accepted" {
			setFail("任务未接取")
			continue
		}

		need := t.Target - t.Current
		if need < 1 {
			need = 1
		}
		if g.reportTask(t, spec, need) {
			reported++
		} else {
			setFail("事件上报失败")
		}
		sleep(growthReportSettle)

		prog := g.pollProgress(t.Code, t.Current, t.Target)
		if prog < t.Target {
			sleep(growthGap)
			continue
		}
		if okClaim, _, reason := g.claimTask(t.Code); okClaim {
			claimed++
		} else {
			setFail(reason)
		}
		sleep(growthGap)
	}

	g.flushUsedEventIDs()

	msg := fmt.Sprintf("成长任务完成: 接取=%d 上报=%d 领奖=%d 跳过=%d 失败=%d",
		out.accepted, reported, claimed, skipped, failed)
	if failed > 0 && failReason != "" {
		msg += " 首因=" + truncateRunes(failReason, 120)
	}
	if failed > 0 && claimed == 0 && reported == 0 {
		return jobStatusFailed, truncateRunes(msg, maxJobMessageLen)
	}
	return jobStatusOK, truncateRunes(msg, maxJobMessageLen)
}

// acceptOutcome 接取阶段的结果。
type acceptOutcome struct {
	tasks    []growthTask
	accepted int
}

// acceptPending 批量接取未接任务；仅当上游报告失败时才复核并补一次（wb_tasks.py:441-469）。
func (g *growthRunner) acceptPending(tasks []growthTask) acceptOutcome {
	codes := pendingCodes(tasks)
	if len(codes) == 0 {
		return acceptOutcome{tasks: tasks}
	}
	acc, failed := g.acceptTasks(codes)
	out := acceptOutcome{tasks: markAccepted(tasks, acc), accepted: len(acc)}
	if len(failed) == 0 {
		return out
	}
	// 上游偶尔瞬时拒绝整批：重列一次，仍待接取的再补一次（wb_tasks.py:452-462）。
	fresh, err := g.fetchTasks()
	if err != nil {
		return out
	}
	out.tasks = fresh
	still := pendingCodes(fresh)
	if len(still) == 0 {
		return out
	}
	acc2, _ := g.acceptTasks(still)
	out.accepted += len(acc2)
	if fresh2, err2 := g.fetchTasks(); err2 == nil {
		out.tasks = fresh2
	} else {
		out.tasks = markAccepted(fresh, acc2)
	}
	return out
}

// pendingCodes 需要接取的任务码（排除不可伪造任务）。
func pendingCodes(tasks []growthTask) []string {
	var codes []string
	for _, t := range tasks {
		if t.Status != "not_accepted" {
			continue
		}
		if spec, ok := growthTaskSpecs[t.Code]; ok && spec.Unforgeable {
			continue
		}
		codes = append(codes, t.Code)
	}
	return codes
}

// markAccepted 把已接取任务在内存中标记为 accepted（避免后续误判为未接取）。
func markAccepted(tasks []growthTask, codes []string) []growthTask {
	if len(codes) == 0 {
		return tasks
	}
	set := make(map[string]bool, len(codes))
	for _, c := range codes {
		set[c] = true
	}
	for i := range tasks {
		if set[tasks[i].Code] && tasks[i].Status == "not_accepted" {
			tasks[i].Status = "accepted"
		}
	}
	return tasks
}

// reportTask 为一个任务构造并上报事件批（wb_tasks.py:518-539）。
//
// 同一批内 (eventCode, id) 必须互异，否则被上游判重（wb_tasks.py:78-80）。
func (g *growthRunner) reportTask(t growthTask, spec growthTaskSpec, need int) bool {
	events := make([]map[string]any, 0, need)
	for i := 0; i < need; i++ {
		var ex *expertID
		switch spec.Kind {
		case "expert":
			ex = g.pickExpertID(expertIDPool)
		case "team":
			ex = g.pickExpertID(teamIDPool)
		}
		events = append(events, g.buildEvent(spec.Kind, i, ex))
	}
	ok := true
	for start := 0; start < len(events); start += maxReportEvents {
		end := start + maxReportEvents
		if end > len(events) {
			end = len(events)
		}
		batch := dedupeEventBatch(events[start:end])
		if len(batch) == 0 {
			continue
		}
		if !g.reportEvents(batch) {
			ok = false
		}
		if end < len(events) {
			sleep(growthGap)
		}
	}
	return ok
}

// dedupeEventBatch 去除同一批内重复的 (eventCode, id)（无 id 的事件不参与判重）。
func dedupeEventBatch(events []map[string]any) []map[string]any {
	seen := make(map[string]bool, len(events))
	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		id := toText(ev["id"])
		if id == "" {
			out = append(out, ev)
			continue
		}
		key := toText(ev["eventCode"]) + "\x00" + id
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ev)
	}
	return out
}

// pollProgress 轮询任务进度直到达标/完成或重试用尽（wb_tasks.py:543-561）。
func (g *growthRunner) pollProgress(code string, cur, tgt int) int {
	prog := cur
	for attempt := 0; attempt < 6; attempt++ {
		tasks, err := g.fetchTasks()
		if err != nil {
			break
		}
		if fresh, ok := findGrowthTask(tasks, code); ok {
			prog = fresh.Current
			if prog >= tgt || fresh.Status == "completed" || fresh.Status == "claimed" {
				break
			}
			if prog > cur {
				sleep(2500 * time.Millisecond)
				continue
			}
		}
		if attempt < 2 {
			sleep(1500 * time.Millisecond)
		} else {
			break
		}
	}
	return prog
}

// runNight 夜猫子任务（wb_tasks.py:586-633）。
func (g *growthRunner) runNight() (string, string) {
	tasks, err := g.fetchTasks()
	if err != nil {
		return jobStatusFailed, truncateRunes("获取夜猫子任务清单失败: "+err.Error(), maxJobMessageLen)
	}
	t, ok := findGrowthTask(tasks, nightTaskCode)
	if !ok {
		return jobStatusFailed, "未获取到夜猫子任务清单"
	}
	if t.Status == "claimed" {
		return jobStatusOK, "夜猫子任务已领奖"
	}
	cur, tgt := t.Current, t.Target
	if tgt < 1 {
		tgt = 3
	}
	if cur >= tgt {
		if okClaim, _, _ := g.claimTask(nightTaskCode); okClaim {
			return jobStatusOK, "夜猫子任务达标，领奖成功"
		}
		return jobStatusFailed, "夜猫子任务达标，领奖失败"
	}

	ev := g.buildEvent("cat", 0, nil)
	reportOK := g.reportEvents([]map[string]any{ev})

	// 上报后轮询 4 次、每次间隔 4s（wb_tasks.py:613-617）。
	newCur := cur
	for i := 0; i < 4; i++ {
		sleep(4 * time.Second)
		fresh, ferr := g.fetchTasks()
		if ferr != nil {
			continue
		}
		ft, ok := findGrowthTask(fresh, nightTaskCode)
		if !ok {
			continue
		}
		newCur = ft.Current
		if newCur > cur {
			break
		}
	}
	if newCur >= tgt {
		if okClaim, _, _ := g.claimTask(nightTaskCode); okClaim {
			return jobStatusOK, fmt.Sprintf("夜猫子任务完成 %d/%d，领奖成功", newCur, tgt)
		}
		return jobStatusFailed, fmt.Sprintf("夜猫子任务完成 %d/%d，领奖失败", newCur, tgt)
	}
	if !reportOK && newCur <= cur {
		return jobStatusFailed, fmt.Sprintf("夜猫子事件上报失败 (进度 %d/%d)", newCur, tgt)
	}
	return jobStatusOK, fmt.Sprintf("夜猫子今晚 +1 (进度 %d/%d)", newCur, tgt)
}

// runCat 猫猫旅行（wb_tasks.py:368-425）。
func (g *growthRunner) runCat() (string, string) {
	status, body, err := g.doJSONRetry(http.MethodGet, g.base()+travelStatusPath, g.hdr, nil)
	if err != nil {
		return jobStatusFailed, truncateRunes("查询猫猫旅行状态失败: "+jobTransportErrMessage(g.ctx, err), maxJobMessageLen)
	}
	if status < 200 || status >= 300 {
		return jobStatusFailed, truncateRunes("查询猫猫旅行状态失败: "+cnHTTPErrText(status, body), maxJobMessageLen)
	}
	st := rootData(body)
	state := toText(st["state"])

	switch state {
	case "arrived":
		// 领奖（wb_tasks.py:381-391）。
		cstatus, cbody, cerr := g.doJSONRetry(http.MethodPost, g.base()+travelClaimPath, g.hdr, []byte("{}"))
		if cerr != nil {
			return jobStatusFailed, "猫猫旅行领奖请求失败"
		}
		if cstatus < 200 || cstatus >= 300 {
			return jobStatusFailed, truncateRunes("猫猫旅行领奖失败: "+cnHTTPErrText(cstatus, cbody), maxJobMessageLen)
		}
		if !upstreamCodeOK(cbody) {
			return jobStatusFailed, "猫猫旅行领奖被上游拒绝"
		}
		return jobStatusOK, "猫猫旅行归来，领奖成功"

	case "idle":
		// 今日已完成（wb_tasks.py:393-395）。
		if truthy(st["daily_limit_reached"]) {
			return jobStatusOK, "猫猫今日已完成旅行，明日 00:00 刷新"
		}
		// 目的地取自 config，缺省 1（wb_tasks.py:398-409）。
		var lid any = 1
		if cfstatus, cfbody, cferr := g.doJSONRetry(http.MethodGet, g.base()+travelConfigPath, g.hdr, nil); cferr == nil && cfstatus >= 200 && cfstatus < 300 {
			if loc := firstLocationID(cfbody); loc != nil {
				lid = loc
			}
		}
		depBody, _ := json.Marshal(map[string]any{"location_id": lid})
		dstatus, dbody, derr := g.doJSONRetry(http.MethodPost, g.base()+travelDepartPath, g.hdr, depBody)
		if derr != nil {
			return jobStatusFailed, "猫猫旅行派出请求失败"
		}
		if dstatus < 200 || dstatus >= 300 {
			return jobStatusFailed, truncateRunes("猫猫旅行派出失败: "+cnHTTPErrText(dstatus, dbody), maxJobMessageLen)
		}
		if !upstreamCodeOK(dbody) {
			return jobStatusFailed, "猫猫旅行派出被上游拒绝"
		}
		return jobStatusOK, "猫猫已出发旅行"

	case "traveling":
		return jobStatusOK, "猫猫正在旅行途中，请稍后再来查看"

	default:
		if state == "" {
			return jobStatusSkipped, "猫猫旅行状态未知"
		}
		return jobStatusOK, "猫猫当前状态: " + truncateRunes(state, 32)
	}
}

// ---- 事件构造（对齐 wb_tasks.py:278-344 build_event） ----

// buildEvent 构造一类行为事件。expert 仅对 expert/team 类有意义（每次用不同 id）。
func (g *growthRunner) buildEvent(kind string, idx int, expert *expertID) map[string]any {
	nowMs := g.now.UnixMilli()
	cid := fmt.Sprintf("wb-task-%d-%d", nowMs, idx)
	rid := cid + "-req"
	uid := UIDFrom(g.tok)

	switch kind {
	case "canvas":
		return map[string]any{
			"eventCode": "wbx_design_canvas_task_create", "timestamp": nowMs,
			"reportDelay": 0, "conversationId": cid, "requestId": rid,
			"source": "summon_keyword", "isCustomModel": false, "name": "",
			"inputLength": 12, "id": fmt.Sprintf("wbx-canvas-%d", nowMs), "cost": 0,
			"isSuccessful": true, "userId": uid,
		}
	case "template":
		return map[string]any{
			"eventCode": "agent_task_created_with_template", "timestamp": nowMs,
			"reportDelay": 0, "isCustomModel": true, "id": strconv.Itoa(idx),
			"name": "幻灯片", "requestId": rid, "conversationId": cid, "userId": uid,
		}
	case "expert", "team", "lighthouse":
		etype := "agent"
		if kind == "team" {
			etype = "team"
		}
		var exID, name string
		switch {
		case expert != nil:
			exID, name = expert.ID, expert.Name
		case kind == "lighthouse":
			exID, name = "ex_2cvvUZQhDyeJ", "腾讯轻量云专家"
		case kind == "team":
			exID, name = "CloudOpsTeam", "运维专家团队"
		default:
			exID, name = "ContentCreator", "内容创作专家"
		}
		return map[string]any{
			"eventCode": "expert_actual_use", "timestamp": nowMs, "reportDelay": 0,
			"mode": "CLOUD", "id": exID, "name": name, "expertTitle": name,
			"type": "02-Engineering", "expertType": etype, "source": "builtin",
			"version": "1.0.2", "cost": 0, "characterCount": 12, "conversationId": cid,
			"requestId": rid, "messageId": rid, "requestModelId": "deepseek-v4-flash",
			"requestModelName": "DeepSeek V4 Flash", "userId": uid,
		}
	case "skill":
		return map[string]any{
			"eventCode": "skill_info", "timestamp": nowMs, "reportDelay": 0,
			"skillId": "skill_2096525080079265792", "name": "pptx", "userId": uid,
		}
	case "automation":
		return map[string]any{
			"eventCode": "automated_task_create_suc", "timestamp": nowMs, "reportDelay": 0,
			"name": "每周工作整理", "type": "cron", "source": "manually",
			"modelId": "deepseek-v4-flash", "modelIsThinking": false,
			"conversationId": cid, "requestId": rid,
			"schedule": map[string]any{"type": "recurring", "rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"},
			"prompt":   "每周五自动整理本周工作", "userId": uid,
		}
	case "playbook":
		return map[string]any{
			"eventCode": "playbook_prompt_send", "timestamp": nowMs, "reportDelay": 0,
			"id": "worker-ledger-freedom-dashboard", "name": "打工人小账本",
			"type": "other", "promptLength": 10, "isOfficial": 1,
			"source": "discover", "conversationId": cid, "requestId": rid, "userId": uid,
		}
	case "skin":
		return map[string]any{
			"eventCode": "appearance_skin_apply", "timestamp": nowMs, "reportDelay": 0,
			"action": "apply", "source": "settings_close", "id": "theme-tkmw7j",
			"vipLevel": "free", "series": "craft", "type": "unknown",
			"name": "和平精英激战金秋", "userId": uid,
		}
	case "chat", "glmchat", "cat":
		mID, mName := "deepseek-v4-flash", "DeepSeek V4 Flash"
		if kind == "glmchat" || kind == "cat" {
			mID, mName = "glm-5.2", "GLM-5.2"
		}
		mode := "craft"
		if kind == "cat" {
			mode = "night"
		}
		return map[string]any{
			"eventCode": "chat_request_send", "timestamp": nowMs, "reportDelay": 0,
			"mode": mode, "conversationId": cid, "requestId": rid,
			"inputLength": 12, "requestModelId": mID, "requestModelName": mName,
			"isPlan": false, "agentName": "default", "agentType": "conversation",
			"userId": uid,
		}
	default:
		// 未识别种类回落 heartbeat（wb_tasks.py:344）。
		return map[string]any{"eventCode": "heartbeat", "timestamp": nowMs, "userId": uid}
	}
}

// ---- 上游调用 ----

// cnChatHeaders 构造 chat 侧请求头（身分头 + Bearer + JSON）。
func cnChatHeaders(p *model.Provider, tok *oauth.TokenSet) (http.Header, error) {
	hdr, err := Headers(p, tok, HeaderOpt{Product: DefaultProduct(), Fingerprint: Fingerprint()})
	if err != nil {
		return nil, err
	}
	hdr.Set("Authorization", "Bearer "+tok.AccessToken)
	hdr.Set("Content-Type", "application/json")
	return hdr, nil
}

// cnWebClaimHeaders web 域降级领奖的请求头（wb_tasks.py:249-259，与 chat 侧不同）。
func cnWebClaimHeaders(tok *oauth.TokenSet) http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+tok.AccessToken)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Content-Type", "application/json")
	h.Set("Origin", cnWebBase)
	h.Set("Referer", cnWebBase+"/profile/growth-center")
	h.Set("x-client-platform", "web")
	h.Set("User-Agent", cnWebUA)
	h.Set("X-User-Id", UIDFrom(tok))
	h.Set("X-Domain", cnWebBase)
	return h
}

// doJSON 发一次请求并读取响应体（限长），返回状态码与正文。
func (g *growthRunner) doJSON(method, target string, hdr http.Header, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(g.ctx, method, target, rdr)
	if err != nil {
		return 0, nil, fmt.Errorf("构建请求失败: %w", err)
	}
	if hdr != nil {
		req.Header = hdr
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxJobBodyBytes))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("读取响应失败: %w", err)
	}
	return resp.StatusCode, rb, nil
}

// doJSONRetry 每步最多一次重试：传输错误或 5xx 才重试（400 等业务错误不重试）。
func (g *growthRunner) doJSONRetry(method, target string, hdr http.Header, body []byte) (int, []byte, error) {
	var (
		status   int
		respBody []byte
		err      error
	)
	for attempt := 0; attempt < 2; attempt++ {
		status, respBody, err = g.doJSON(method, target, hdr, body)
		if err == nil && !retryableStatus(status) {
			return status, respBody, nil
		}
	}
	return status, respBody, err
}

// retryableStatus 可重试的状态码（只重试服务端错误）。
func retryableStatus(status int) bool { return status >= 500 }

// fetchTasks GET 任务清单（wb_tasks.py:102-130）。
func (g *growthRunner) fetchTasks() ([]growthTask, error) {
	status, body, err := g.doJSONRetry(http.MethodGet, g.base()+growthTasksPath, g.hdr, nil)
	if err != nil {
		return nil, fmt.Errorf("%s", jobTransportErrMessage(g.ctx, err))
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%s", cnHTTPErrText(status, body))
	}
	return parseGrowthTasks(body), nil
}

// acceptTasks 批量接取，每批 20（wb_tasks.py:165-223）。返回 accepted / failed 任务码。
func (g *growthRunner) acceptTasks(codes []string) (accepted, failed []string) {
	const chunk = 20
	for i := 0; i < len(codes); i += chunk {
		end := i + chunk
		if end > len(codes) {
			end = len(codes)
		}
		part := codes[i:end]
		body, _ := json.Marshal(map[string]any{"task_codes": part})
		status, respBody, err := g.doJSONRetry(http.MethodPost, g.base()+growthAcceptPath, g.hdr, body)
		if err != nil {
			failed = append(failed, part...)
			continue
		}
		if status < 200 || status >= 300 || !upstreamCodeOK(respBody) {
			failed = append(failed, part...)
			continue
		}
		seen := make(map[string]bool, len(part))
		for _, item := range parseAcceptResults(respBody) {
			seen[item.Code] = true
			if item.Status == "accepted" || item.Status == "already_accepted" {
				accepted = append(accepted, item.Code)
			} else {
				failed = append(failed, item.Code)
			}
		}
		for _, c := range part {
			if !seen[c] {
				failed = append(failed, c)
			}
		}
	}
	return accepted, failed
}

// reportEvents POST /v2/report（wb_tasks.py:347-365）。
func (g *growthRunner) reportEvents(events []map[string]any) bool {
	body, err := json.Marshal(events)
	if err != nil {
		return false
	}
	status, respBody, err := g.doJSONRetry(http.MethodPost, g.base()+reportPath, g.hdr, body)
	if err != nil || status < 200 || status >= 300 {
		return false
	}
	return upstreamCodeOK(respBody)
}

// claimTask 领奖；400 时降级到 web 域（wb_tasks.py:226-275）。
func (g *growthRunner) claimTask(code string) (bool, float64, string) {
	path := fmt.Sprintf(growthClaimPathFmt, url.PathEscape(code))
	status, body, err := g.doJSONRetry(http.MethodPost, g.base()+path, g.hdr, []byte("{}"))
	if err == nil {
		if status >= 200 && status < 300 {
			return claimResult(body)
		}
		if status == http.StatusBadRequest {
			return g.claimTaskWeb(code)
		}
		return false, 0, cnHTTPErrText(status, body)
	}
	return false, 0, jobTransportErrMessage(g.ctx, err)
}

// claimTaskWeb web 域降级领奖（wb_tasks.py:246-270）。
func (g *growthRunner) claimTaskWeb(code string) (bool, float64, string) {
	path := fmt.Sprintf(growthClaimPathFmt, url.PathEscape(code))
	status, body, err := g.doJSONRetry(http.MethodPost, cnClaimWebURL()+path, cnWebClaimHeaders(g.tok), []byte("{}"))
	if err != nil {
		return false, 0, "web 降级领奖请求失败"
	}
	if status < 200 || status >= 300 {
		return false, 0, cnHTTPErrText(status, body)
	}
	return claimResult(body)
}

// claimResult 解析领奖响应（只取 code/credit，绝不回显正文）。
func claimResult(body []byte) (bool, float64, string) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return false, 0, "领奖响应不可解析"
	}
	if toText(root["code"]) == "0" {
		data, _ := dataObject(root)
		credit, _ := toFloat(data["credit"])
		return true, credit, ""
	}
	if s := upstreamErrSummary(body); s != "" {
		return false, 0, s
	}
	return false, 0, "上游拒绝领奖"
}

// ---- 解析与杂项 ----

// growthTask 解析后的任务条目。
type growthTask struct {
	Code    string
	Name    string
	Status  string
	Current int
	Target  int
	JumpURL string
}

// acceptResult 暂存 /tasks/accept 的单个结果。
type acceptResult struct {
	Code   string
	Status string
}

// parseGrowthTasks 解析清单（wb_tasks.py:109-128）。
func parseGrowthTasks(body []byte) []growthTask {
	var root struct {
		Data struct {
			Tasks []struct {
				TaskCode     string `json:"task_code"`
				Title        string `json:"title"`
				JumpURL      string `json:"jump_url"`
				AcceptStatus string `json:"accept_status"`
				Progress     struct {
					Current float64 `json:"current"`
					Target  float64 `json:"target"`
				} `json:"progress"`
			} `json:"tasks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	out := make([]growthTask, 0, len(root.Data.Tasks))
	for _, t := range root.Data.Tasks {
		if t.TaskCode == "" {
			continue
		}
		spec, ok := growthTaskSpecs[t.TaskCode]
		name := t.Title
		if name == "" && ok {
			name = spec.Name
		}
		if name == "" {
			name = t.TaskCode
		}
		tgt := int(t.Progress.Target)
		if tgt <= 0 {
			if ok {
				tgt = spec.Target
			}
			if tgt <= 0 {
				tgt = 1
			}
		}
		st := t.AcceptStatus
		if st == "" {
			st = "not_accepted"
		}
		out = append(out, growthTask{
			Code: t.TaskCode, Name: name, Status: st,
			Current: int(t.Progress.Current), Target: tgt, JumpURL: t.JumpURL,
		})
	}
	return out
}

// parseAcceptResults 解析接取结果（wb_tasks.py:209-222）。
func parseAcceptResults(body []byte) []acceptResult {
	var root struct {
		Data struct {
			Results []struct {
				TaskCode string `json:"task_code"`
				Status   string `json:"status"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	out := make([]acceptResult, 0, len(root.Data.Results))
	for _, r := range root.Data.Results {
		out = append(out, acceptResult{Code: r.TaskCode, Status: r.Status})
	}
	return out
}

// findGrowthTask 按任务码查找。
func findGrowthTask(tasks []growthTask, code string) (growthTask, bool) {
	for i := range tasks {
		if tasks[i].Code == code {
			return tasks[i], true
		}
	}
	return growthTask{}, false
}

// upstreamCodeOK 上游业务成功判定：code == 0（wb_tasks.py:203/233/415）。
func upstreamCodeOK(body []byte) bool {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return false
	}
	return toText(root["code"]) == "0"
}

// cnHTTPErrText 非 2xx 的结构化摘要（只含状态码与上游字段，绝不含正文）。
func cnHTTPErrText(status int, body []byte) string {
	if s := upstreamErrSummary(body); s != "" {
		return fmt.Sprintf("上游返回 %d: %s", status, s)
	}
	return fmt.Sprintf("上游返回 %d", status)
}

// rootData 取响应体 data 对象；缺失返回空 map。
func rootData(body []byte) map[string]any {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return map[string]any{}
	}
	if d, ok := dataObject(root); ok {
		return d
	}
	return map[string]any{}
}

// firstLocationID 取 travel/config 的第一个目的地 id（wb_tasks.py:400-407）。
func firstLocationID(body []byte) any {
	data := rootData(body)
	locs, ok := data["locations"].([]any)
	if !ok || len(locs) == 0 {
		return nil
	}
	first, ok := locs[0].(map[string]any)
	if !ok {
		return nil
	}
	if id, ok := first["id"]; ok && id != nil {
		return id
	}
	return nil
}

// truthy 宽松真值判定（用于 daily_limit_reached 之类的布尔/数值字段）。
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		s := strings.TrimSpace(t)
		return s != "" && s != "0" && !strings.EqualFold(s, "false")
	}
	return false
}
