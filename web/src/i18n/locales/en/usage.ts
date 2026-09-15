// Usage page + shared chart components (usage-charts)
const usage = {
  title: 'Usage',
  totalCostTitle: 'Total cost to date (all history, priced at per-request snapshots)',
  totalCost: 'Total spend {{amount}}',

  // ---- Filter bar ----
  allKeys: 'All keys',
  allModels: 'All models',
  clearFilters: 'Clear filters',
  filterKey: 'Key "{{name}}"',
  filterModel: 'Model {{model}}',

  // ---- Empty states ----
  noDataFiltered: 'No usage under the current filters{{filter}} — try widening the time range or clearing filters',
  noDataHint: 'No usage in this time range — set up a channel and make a call with curl or an SDK',
  updatedAt: 'Updated at {{time}}',
  noDataInRange: 'No usage in this time range',

  // ---- KPI cards ----
  totalTokens: 'Total tokens',
  inputTokens: 'Input tokens',
  outputTokens: 'Output tokens',
  requests: 'Requests',
  avgLatency: 'Avg latency',
  cost: 'Cost',
  pctOf: '{{pct}}% of total',
  deltaHint: 'vs previous equal-length period',
  hint: {
    totalTokens: 'Input + output tokens combined',
    inputTokens: 'Prompt tokens sent to the model',
    outputTokens: 'Completion tokens returned by the model',
    requests: 'Relay requests counted in the stats',
    avgLatency: 'Request-weighted average upstream response time',
    cost: 'Request cost from model pricing (USD)\nUnpriced models are not billed',
    inputLegend: 'Input tokens (prompt)\nStacked by calendar day (Asia/Shanghai)',
    outputLegend: 'Output tokens (completion)\nStacked by calendar day (Asia/Shanghai)',
    reqLegend: 'Relay requests per day',
  },

  // ---- Daily trend ----
  dailyTrend: 'Daily trend',
  input: 'Input',
  output: 'Output',

  // ---- Model distribution ----
  modelDist: 'Model distribution',
  other: 'Other',
  request: 'Request',

  // ---- Hourly activity ----
  hourly: 'Hourly activity',

  // ---- Key usage table ----
  keyUsage: 'Key usage',
  keyUsageHint: 'Sorted by total tokens desc · click a row to filter all charts by that key',
  keyExpand: 'Show key usage table',
  keyCollapse: 'Hide key usage table',
  tokenShare: 'Token share',
  lastUsed: 'Last used',
  rowFilterTip: 'Click to filter all charts and details above by this key',
  deletedKey: 'Deleted key',
  deletedKeyId: 'Deleted key #{{id}}',
  keyId: 'Key #{{id}}',
  filtering: 'Filtering',

  // ---- Channel usage table ----
  providerUsage: 'Channel usage',
  providerUsageHint: 'Sorted by total tokens desc · click a row to filter all charts by that channel',
  rowFilterTipProvider: 'Click to filter all charts and details above by this channel',
  deletedProvider: 'Deleted channel',
  deletedProviderId: 'Deleted channel #{{id}}',
  providerIdKey: 'Channel #{{id}}',
  filterProvider: 'Channel "{{name}}"',
  providerExpand: 'Show channel usage table',
  providerCollapse: 'Hide channel usage table',

  // ---- Detail table ----
  date: 'Date',
  deleted: 'Deleted',

  // ---- Below: shared chart components (usage-charts) ----
  // Date range presets
  range: {
    today: 'Today',
    '24h': '24H',
    '7d': '7D',
    '30d': '30D',
    '90d': '90D',
    custom: 'Custom',
  },
  // Delta badge hover ({{period}} is " (M/D ~ M/D)" or empty)
  deltaTip: 'Change vs previous period{{period}}',
  // Donut segment hover
  share: 'Share',
  // Legend list hover ({{pct}} already includes the percent sign)
  legendTip: '{{label}}: {{value}} ({{pct}})',
  // Pagination
  pageInfo: '{{total}} records · showing {{from}}-{{to}}',
  perPage: '{{n}} / page',
  // Heatmap
  requestsCount: '{{n}} requests',
  noRequests: 'No requests',
  dow: {
    sun: 'Sun',
    mon: 'Mon',
    tue: 'Tue',
    wed: 'Wed',
    thu: 'Thu',
    fri: 'Fri',
    sat: 'Sat',
  },
  less: 'Less',
  more: 'More',
}

export default usage
