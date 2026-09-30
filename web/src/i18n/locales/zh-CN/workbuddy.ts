// WorkBuddy 自动化页面（渠道运行态 / 设置 / 任务开关 / 执行日志）
export default {
  title: 'WorkBuddy 自动化',
  subtitle: '以官方客户端身分代跑签到、领积分等日常任务，并汇总各账号运行态',

  // ---- 总开关 ----
  enable: '启用 WorkBuddy 自动化',
  enableHint: '已启用：平台会按下方设置与任务开关自动运行',
  enableRisk: '未启用：以下任务区不可用',
  // 开启前必须二次确认的风险告知（措辞经产品确认，勿随意简写）
  confirmEnable:
    '该能力会以官方客户端身分访问上游，账号可能被上游风控限制；自动化任务还会模拟用户行为。请确认你使用的是自己已获授权的账号。',

  // ---- 设置 ----
  settingsTitle: '设置',
  reserveCredits: '保留积分',
  reserveCreditsHint: '积分低于该值时暂停消耗积分的任务；0 = 不保留',
  dailyTokenLimit: '每日 token 上限',
  dailyTokenLimitHint: '0 = 不限',
  product: '出站身分',
  productWorkbuddy: 'WorkBuddy 桌面端',
  productVscode: 'VS Code 插件',
  productCli: 'CLI',
  scrubMode: '脱敏档位',
  scrubOff: '关闭',
  scrubShadow: '仅统计',
  scrubOn: '改写',
  scrubOnWarn: '消息内容可能被平台改写，且每次改写都会留审计记录',
  fingerprint: '设备指纹',
  fingerprintHint: '合成指纹，仅用于降低多账号关联，不含真实设备信息',
  save: '保存设置',
  saved: '设置已保存',
  saveFailed: '保存失败：{{error}}',
  loadFailed: '加载失败：{{error}}',

  // ---- 渠道运行态 ----
  channelsTitle: '渠道运行态',
  channelsHint: '积分与 token 为最近一次快照，点击「刷新」可手动同步',
  colName: '名称',
  colRealm: '区域',
  colAccount: '账号',
  colCredits: '积分',
  colDailyTokens: '当日 token',
  colStatus: '状态',
  colCooldowns: '模型冷却',
  colLastOk: '最近成功',
  realmIntl: '国际',
  realmCn: '国内',
  refreshCredits: '刷新',
  creditsRefreshed: '已刷新「{{name}}」的积分',
  none: '无',

  // ---- 渠道状态（parked_reason）----
  parkNone: '正常',
  parkDisabled: '已停用',
  parkDailyLimit: '额度用尽',
  parkReserve: '保留积分',
  parkCooldown: '冷却中',
  parkCooldownUntil: '预计恢复时间 {{time}}',
  cooldownUntil: '{{model}} 恢复时间 {{time}}',

  // ---- 任务区 ----
  jobsTitle: '自动化任务',
  jobsPlatformOff: '平台已禁用自动化任务，任务开关暂不可用',
  jobsDisabledHint: '请先启用 WorkBuddy 自动化',
  runNow: '立即执行',
  confirmHighRisk: '「{{job}}」属于高风险任务，会模拟用户行为并可能触发上游风控。确认开启？',
  confirmRun: '确认立即执行「{{job}}」？',
  runResult: '渠道 #{{id}}：{{status}} — {{message}}',
  runDone: '「{{job}}」已触发',
  highRisk: '高风险',

  // 7 个任务名
  jobCheckin: '每日签到',
  jobCredits: '领取积分',
  jobDailyChat: '每日对话',
  jobGrowth: '成长任务',
  jobNight: '夜间任务',
  jobCat: '猫猫任务',
  jobDailyChatWeb: '网页每日对话',

  // ---- 执行日志 ----
  runsTitle: '执行日志',
  runsHint: '最近 50 条（后端已按时间倒序）',
  colJob: '任务',
  colProvider: '渠道',
  colTrigger: '触发方式',
  colDuration: '耗时',
  colMessage: '摘要',
  triggerSchedule: '定时',
  triggerManual: '手动',

  // 执行状态
  statusOk: '成功',
  statusSkipped: '跳过',
  statusFailed: '失败',
  statusParked: '已暂停',

  // ---- 空态 ----
  emptyTitle: '还没有 WorkBuddy / CodeBuddy 渠道',
  emptyHint: '请先到「渠道」页添加 WorkBuddy 或 CodeBuddy 渠道，再回来配置自动化。',
  goProviders: '前往渠道页',
}
