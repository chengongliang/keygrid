// 管理端 · 渠道健康页文案
// 说明文案与后端 relay 熔断语义保持一致（滑动窗口计数 + 只认渠道/网络层故障）
const adminProviders = {
  title: '渠道健康',
  desc: '熔断按渠道统计，且需要在渠道里单独开启（默认为关，适合只有一条不稳定渠道的场景）：只有 5xx、限流（429）、连接/超时/断流这类渠道或网络层故障才计入，参数错误等 4xx 不计入。默认 30 秒内失败 5 次熔断 30 秒，之后自动半开试探，也可手动重置。',
  onlyAbnormal: '仅看异常',
  onlyAbnormalHint: '只显示熔断中 / 半开试探的渠道，或近 24h 有失败的渠道',
  colUser: '用户',
  colChannel: '渠道',
  colCred: '凭据',
  colBreaker: '熔断',
  colReq24h: '24h 请求',
  colFail24h: '24h 失败',
  colSuccessRate: '成功率',
  colLatency: '平均延迟',
  breakerClosed: '正常',
  breakerHalfOpen: '半开试探',
  breakerOpen: '熔断中',
  breakerOff: '未启用',
  resetBreakerOff: '该渠道未开启熔断检测，无需重置',
  resetBreakerIdle: '当前未熔断，无需重置',
  breakerOffTip: '该渠道未开启熔断检测（渠道编辑里可勾选），不会因连续失败被熔断。',
  rowHint: '点击查看该渠道最近失败详情',
  resetBreaker: '重置熔断',
  resetDone: '已重置 {{name}} 的熔断状态',
  resetFailed: '重置失败',
  empty: '没有匹配的渠道',
  total: '共 {{total}} 条渠道',
}

export default adminProviders
