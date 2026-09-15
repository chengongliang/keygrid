// 管理端 · 平台用量页文案
const adminUsage = {
  title: '全平台用量',
  exportCsv: '导出 CSV',
  exportFailed: '导出失败 {{code}}',
  // ---- 维度 / 指标切换（DIMS、METRIC_OPTS 的 label 存此处 key，渲染处 t()）----
  byDay: '按日',
  byUser: '按用户',
  byModel: '按模型',
  metricTokens: 'Token',
  requests: '请求数',
  // ---- 高频短标签（趋势分段、热力、表头共用）----
  labelIn: '输入',
  labelOut: '输出',
  inTokens: '输入 Tokens',
  outTokens: '输出 Tokens',
  other: '其他',
  centerRequests: '请求',
  // ---- 明细表头 ----
  colUser: '用户',
  colModel: '模型',
  colDate: '日期',
  colFailed: '失败',
  colCost: '费用',
  colAvgLatency: '平均延迟',
  noDataInRange: '该时间范围内暂无数据',
  // ---- KPI 卡片 ----
  kpi: {
    failRate: '失败率',
    failRateHint: '状态码 ≥ 400 的请求占比',
    failedCount: '{{n}} 次失败',
    allSuccess: '全部成功',
    totalTokens: '总 Tokens',
    requestsHint: '计入统计的中转请求次数\n环比 = 较上一等长周期',
    totalTokensHint: '输入 + 输出 token 合计\n环比 = 较上一等长周期',
    inTokensHint: '发送给模型的 prompt tokens\n环比 = 较上一等长周期',
    outTokensHint: '模型返回的 completion tokens\n环比 = 较上一等长周期',
    cost: '总费用',
    costHint: '全平台按模型定价计算的费用（USD）\n未定价模型不计费\n环比 = 较上一等长周期',
    share: '{{p}}% 占比',
  },
  // ---- 每日趋势 ----
  trend: {
    title: '每日趋势',
    inHint: '输入 Tokens（prompt）\n按自然日（Asia/Shanghai）堆叠',
    outHint: '输出 Tokens（completion）\n按自然日（Asia/Shanghai）堆叠',
    reqHint: '每日中转请求次数',
  },
  // ---- 分布图 / TOP / 热力图标题 ----
  donut: {
    title: '模型分布',
  },
  top: {
    title: 'TOP 用户',
  },
  heat: {
    title: '分时活跃',
  },
}

export default adminUsage
