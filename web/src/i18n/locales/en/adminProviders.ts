// Admin · channel health page copy
// Wording mirrors the relay breaker semantics (sliding window + channel/network failures only)
const adminProviders = {
  title: 'Channel Health',
  desc: 'Circuit breaking is per channel and must be enabled per channel (off by default, which suits setups with a single flaky channel): only channel/network-level failures count (5xx, rate limit 429, connect/timeout/interrupted stream) — 4xx such as bad parameters never trip it. Default: 5 failures within 30s trips the channel for 30s, then a half-open probe runs automatically; you can also reset it manually.',
  onlyAbnormal: 'Abnormal only',
  colUser: 'User',
  colChannel: 'Channel',
  colCred: 'Credential',
  colBreaker: 'Breaker',
  colReq24h: '24h requests',
  colFail24h: '24h failures',
  colSuccessRate: 'Success rate',
  colLatency: 'Avg latency',
  breakerClosed: 'Healthy',
  breakerHalfOpen: 'Probing',
  breakerOpen: 'Tripped',
  breakerOff: 'Off',
  resetBreakerOff: 'Circuit breaking is off for this channel — nothing to reset',
  resetBreakerIdle: 'Not tripped — nothing to reset',
  breakerOffTip: 'Circuit breaking is not enabled for this channel (enable it in the channel editor); failures will never trip it.',
  rowHint: 'Click to view this channel\u2019s recent failures',
  resetBreaker: 'Reset breaker',
  resetDone: 'Breaker of {{name}} reset',
  resetFailed: 'Reset failed',
  empty: 'No matching channels',
  total: '{{total}} channels in total',
}

export default adminProviders
