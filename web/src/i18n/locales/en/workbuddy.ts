// WorkBuddy automation page (channel runtime, settings, job toggles, run log)
export default {
  title: 'WorkBuddy Automation',
  subtitle: 'Runs check-in, credits and other daily jobs as the official client, and summarizes each account\'s runtime state',

  // ---- Master switch ----
  enable: 'Enable WorkBuddy automation',
  enableHint: 'Enabled: the platform runs jobs automatically per the settings and toggles below',
  enableRisk: 'Disabled: the job section below is unavailable',
  // Risk notice confirmed with product; do not shorten
  confirmEnable:
    'This capability accesses the upstream as the official client; your account may be limited by upstream risk controls, and automated jobs also simulate user behavior. Please confirm you are using an account you are authorized to use.',

  // ---- Settings ----
  settingsTitle: 'Settings',
  reserveCredits: 'Reserve credits',
  reserveCreditsHint: 'Pause credit-consuming jobs when credits fall below this value; 0 = no reserve',
  dailyTokenLimit: 'Daily token limit',
  dailyTokenLimitHint: '0 = unlimited',
  product: 'Outbound identity',
  productWorkbuddy: 'WorkBuddy desktop',
  productVscode: 'VS Code extension',
  productCli: 'CLI',
  scrubMode: 'Scrub mode (on by default)',

  scrubOffWarn: 'With scrubbing off, fingerprint text from clients such as Claude Code or the Codex CLI makes the upstream reject the request as an unapproved channel (HTTP 400 / code 11128) — those clients will not work on this channel.',
  scrubDefaultHint: 'On unless turned off. Fingerprint text from clients such as Claude Code or the Codex CLI makes the upstream reject the request as an unapproved channel (11128); scrubbing rewrites only the matched fragments. Turning it off gives up that protection.',
  scrubOff: 'Off',
  scrubShadow: 'Count only',
  scrubOn: 'Rewrite',
  scrubOnWarn: 'Message content may be rewritten by the platform, and every rewrite is recorded in the audit log',
  fingerprint: 'Device fingerprint',
  fingerprintHint: 'Synthetic fingerprint, used only to reduce multi-account correlation; contains no real device data',
  save: 'Save settings',
  saved: 'Settings saved',
  saveFailed: 'Save failed: {{error}}',
  loadFailed: 'Load failed: {{error}}',

  // ---- Channel runtime ----
  channelsTitle: 'Channel runtime',
  channelsHint: 'Credits and tokens are the latest snapshot; click "Refresh" to sync manually',
  colName: 'Name',
  colRealm: 'Realm',
  colAccount: 'Account',
  colCredits: 'Credits',
  colDailyTokens: 'Tokens today',
  colStatus: 'Status',
  colCooldowns: 'Model cooldowns',
  colLastOk: 'Last success',
  realmIntl: 'Intl',
  realmCn: 'CN',
  refreshCredits: 'Refresh',
  creditsRefreshed: 'Refreshed credits for "{{name}}"',
  none: 'None',

  // ---- Channel status (parked_reason) ----
  parkNone: 'Normal',
  parkDisabled: 'Disabled',
  parkDailyLimit: 'Daily limit',
  parkReserve: 'Reserve credits',
  parkCooldown: 'Cooling down',
  parkCooldownUntil: 'Expected recovery at {{time}}',
  cooldownWholeChannel: 'Whole channel (account risk control)',
  cooldownUntil: '{{model}} recovers at {{time}}',

  // ---- Providers page badge (same parked_reason vocabulary) ----
  parkedDisabled: 'Parked: manually off',
  parkedDailyLimit: 'Parked: daily limit',
  parkedReserve: 'Parked: reserve credits',
  parkedCooldown: 'Parked: rate-limit cooldown',
  parkedDisabledTitle: 'This channel was manually turned off, so WorkBuddy jobs will not run on it.',
  parkedDailyLimitTitle: 'This channel has reached its daily token limit, so WorkBuddy jobs will not run on it today.',
  parkedReserveTitle: 'This channel\'s credits are below the reserve-credits threshold, so credit-consuming jobs are paused.',
  parkedCooldownTitle: 'This channel is cooling down after rate limiting, so WorkBuddy jobs will skip it for now.',
  parkedUnknownTitle: 'This channel is parked: {{reason}}',
  lowCredits: 'Credits {{credits}}',
  lowCreditsTitle: 'This channel has {{credits}} credits, below the reserve-credits threshold of {{reserve}}; credit-consuming jobs are paused.',

  // ---- Jobs ----
  jobsTitle: 'Automation jobs',
  jobsPlatformOff: 'Automation jobs are disabled platform-wide; toggles are unavailable',
  jobsDisabledHint: 'Enable WorkBuddy automation first',
  runNow: 'Run now',
  confirmHighRisk: '"{{job}}" is a high-risk job that simulates user behavior and may trigger upstream risk controls. Enable it?',
  confirmRun: 'Run "{{job}}" now?',
  runResult: 'Channel #{{id}}: {{status}} — {{message}}',
  runDone: '"{{job}}" triggered',
  highRisk: 'High risk',

  // 7 job names
  jobCheckin: 'Daily check-in',
  jobCredits: 'Claim credits',
  jobDailyChat: 'Daily chat',
  jobGrowth: 'Growth tasks',
  jobNight: 'Night tasks',
  jobCat: 'Cat tasks',
  jobDailyChatWeb: 'Web daily chat',

  // ---- Run log ----
  runsTitle: 'Run log',
  runsHint: 'Most recent 50 (already sorted newest-first by the backend)',
  colJob: 'Job',
  colProvider: 'Channel',
  colTrigger: 'Trigger',
  colDuration: 'Duration',
  colMessage: 'Summary',
  triggerSchedule: 'Scheduled',
  triggerManual: 'Manual',

  // Run status
  statusOk: 'OK',
  statusSkipped: 'Skipped',
  statusFailed: 'Failed',
  statusParked: 'Parked',

  // ---- Empty state ----
  emptyTitle: 'No WorkBuddy / CodeBuddy channel yet',
  emptyHint: 'Add a WorkBuddy or CodeBuddy channel on the Channels page first, then come back to configure automation.',
  goProviders: 'Go to Channels',
}
