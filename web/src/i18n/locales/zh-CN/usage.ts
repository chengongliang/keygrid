// 用量页 + 用量图表共享组件（usage-charts）文案
const usage = {
  title: '用量',
  totalCostTitle: '累计总费用（全部历史，按请求时价格快照）',
  totalCost: '累计消耗 {{amount}}',

  // ---- 筛选栏 ----
  allKeys: '全部 Key',
  allModels: '全部模型',
  clearFilters: '清除筛选',
  filterKey: 'Key「{{name}}」',
  filterModel: '模型 {{model}}',

  // ---- 空状态 ----
  noDataFiltered: '当前筛选条件下暂无用量数据{{filter}} —— 试试放宽时间范围或清除筛选',
  noDataHint: '该时间范围内暂无用量数据 —— 配好渠道后用 curl 或 SDK 调一次试试',
  updatedAt: '更新于 {{time}}',
  noDataInRange: '该时间范围内暂无用量数据',

  // ---- KPI 卡片 ----
  totalTokens: '总 Tokens',
  inputTokens: '输入 Tokens',
  outputTokens: '输出 Tokens',
  requests: '请求数',
  avgLatency: '平均延迟',
  cost: '费用',
  pctOf: '{{pct}}% 占比',
  deltaHint: '环比 = 较上一等长周期',
  hint: {
    totalTokens: '输入 + 输出 token 合计',
    inputTokens: '发送给模型的 prompt tokens',
    outputTokens: '模型返回的 completion tokens',
    requests: '计入统计的中转请求次数',
    avgLatency: '按请求数加权的上游响应耗时均值',
    cost: '按模型定价计算的请求费用（USD）\n未定价模型不计费',
    inputLegend: '输入 Tokens（prompt）\n按自然日（Asia/Shanghai）堆叠',
    outputLegend: '输出 Tokens（completion）\n按自然日（Asia/Shanghai）堆叠',
    reqLegend: '每日中转请求次数',
  },

  // ---- 每日趋势 ----
  dailyTrend: '每日趋势',
  input: '输入',
  output: '输出',

  // ---- 模型分布 ----
  modelDist: '模型分布',
  other: '其他',
  request: '请求',

  // ---- 分时活跃 ----
  hourly: '分时活跃',

  // ---- Key 用量表 ----
  keyUsage: 'Key 用量',
  keyUsageHint: '按总 Tokens 降序 · 点击行可按该 Key 筛选全部图表',
  keyExpand: '展开 Key 用量表',
  keyCollapse: '收起 Key 用量表',
  tokenShare: 'Token 占比',
  lastUsed: '最后使用',
  rowFilterTip: '点击按该 Key 筛选上方全部图表与明细',
  deletedKey: '已删除 Key',
  deletedKeyId: '已删除 Key #{{id}}',
  keyId: 'Key #{{id}}',
  filtering: '筛选中',

  // ---- 渠道用量表 ----
  providerUsage: '渠道用量',
  providerUsageHint: '按总 Tokens 降序 · 点击行可按该渠道筛选全部图表',
  rowFilterTipProvider: '点击按该渠道筛选上方全部图表与明细',
  deletedProvider: '已删除渠道',
  deletedProviderId: '已删除渠道 #{{id}}',
  providerIdKey: '渠道 #{{id}}',
  filterProvider: '渠道「{{name}}」',
  providerExpand: '展开渠道用量表',
  providerCollapse: '收起渠道用量表',

  // ---- 明细表 ----
  date: '日期',
  deleted: '已删除',

  // ---- 以下为 usage-charts 共享组件文案 ----
  // 日期快捷档
  range: {
    today: '今天',
    '24h': '24H',
    '7d': '7D',
    '30d': '30D',
    '90d': '90D',
    custom: '自定义',
  },
  // 环比徽标悬停提示（{{period}} 为「（M/D ~ M/D）」或空）
  deltaTip: '较上一周期{{period}}的涨跌',
  // 甜甜圈图分段悬停
  share: '占比',
  // 图例列表悬停（{{pct}} 已含百分号）
  legendTip: '{{label}}：{{value}}（{{pct}}）',
  // 分页条
  pageInfo: '共 {{total}} 条 · 第 {{from}}-{{to}} 条',
  perPage: '{{n}} 条/页',
  // 热力图
  requestsCount: '{{n}} 次请求',
  noRequests: '无请求',
  dow: {
    sun: '周日',
    mon: '周一',
    tue: '周二',
    wed: '周三',
    thu: '周四',
    fri: '周五',
    sat: '周六',
  },
  less: '少',
  more: '多',
}

export default usage
