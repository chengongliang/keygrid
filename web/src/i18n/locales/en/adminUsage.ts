// Admin · platform usage page copy
const adminUsage = {
  title: 'Platform Usage',
  exportCsv: 'Export CSV',
  exportFailed: 'Export failed ({{code}})',
  // ---- Dimension / metric toggles (DIMS, METRIC_OPTS label keys, rendered via t()) ----
  byDay: 'By day',
  byUser: 'By user',
  byModel: 'By model',
  metricTokens: 'Tokens',
  requests: 'Requests',
  // ---- Short labels shared by trend segments, heatmap and table headers ----
  labelIn: 'Input',
  labelOut: 'Output',
  inTokens: 'Input tokens',
  outTokens: 'Output tokens',
  other: 'Others',
  centerRequests: 'Requests',
  // ---- Detail table headers ----
  colUser: 'User',
  colModel: 'Model',
  colDate: 'Date',
  colFailed: 'Failed',
  colCost: 'Cost',
  colAvgLatency: 'Avg latency',
  noDataInRange: 'No data in this time range',
  // ---- KPI cards ----
  kpi: {
    failRate: 'Failure rate',
    failRateHint: 'Share of requests with status code ≥ 400',
    failedCount: '{{n}} failed',
    allSuccess: 'All succeeded',
    totalTokens: 'Total tokens',
    requestsHint: 'Relay requests counted in stats\nDelta vs previous equal-length period',
    totalTokensHint: 'Input + output tokens combined\nDelta vs previous equal-length period',
    inTokensHint: 'Prompt tokens sent to models\nDelta vs previous equal-length period',
    outTokensHint: 'Completion tokens returned by models\nDelta vs previous equal-length period',
    cost: 'Total cost',
    costHint: 'Platform-wide cost priced per model (USD)\nUnpriced models are not billed\nDelta vs previous equal-length period',
    share: '{{p}}% of total',
  },
  // ---- Daily trend ----
  trend: {
    title: 'Daily trend',
    inHint: 'Input tokens (prompt)\nStacked by calendar day (Asia/Shanghai)',
    outHint: 'Output tokens (completion)\nStacked by calendar day (Asia/Shanghai)',
    reqHint: 'Daily relay requests',
  },
  // ---- Donut / TOP / heatmap titles ----
  donut: {
    title: 'Model distribution',
  },
  top: {
    title: 'Top users',
  },
  heat: {
    title: 'Hourly activity',
  },
}

export default adminUsage
