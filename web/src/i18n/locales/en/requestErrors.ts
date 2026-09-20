// Failed-request diagnostics (shared by the channel health views)
export default {
  title: 'Recent failures',
  hint: 'Metadata and upstream error summary only (≤512 chars, never prompts or response bodies); max 30 rows per channel per minute, kept 7 days',
  empty: 'No failures recorded',
  noProvider: '(not channel-specific)',
  colTime: 'Time',
  colKind: 'Kind',
  colStatus: 'Status',
  colModel: 'Model',
  colKey: 'API key',
  colLatency: 'Latency',
  colMessage: 'Details',

  // Failure kinds
  kindNoChannel: 'No channel',
  kindCircuitOpen: 'Breaker open',
  kindCredential: 'Credential',
  kindTransform: 'Transform failed',
  kindProxy: 'Proxy config',
  kindUpstream4xx: 'Upstream 4xx',
  kindUpstream429: 'Rate limited',
  kindUpstream5xx: 'Upstream 5xx',
  kindTransport: 'Network / connect',
  kindStreamAborted: 'Stream aborted',
}
