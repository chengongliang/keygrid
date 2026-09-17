// Provider channels page: list cards, add wizard, OAuth authorization, model probing/testing, quota display, billing map, model picker
const providers = {
  // ---- List page ----
  title: 'My Providers',
  addChannel: '+ Add channel',
  empty: 'No channels yet. Click "+ Add channel" to connect your first provider ~',
  disabledSection: 'Disabled',
  pendingAuth: 'Pending auth',
  continueAuth: 'Continue auth',
  reauth: 'Re-authorize',
  deleteConfirm: 'Delete channel "{{name}}"? Its credentials will be removed as well.',
  priorityBadge: 'Priority {{n}}',
  proxyBadge: '🛡 Proxy',
  proxyBadgeTitle: 'Upstream requests go through the platform proxy',
  quotaBtn: 'Quota',
  addBtn: '+ Add',

  // ---- Shared form fields (wizard / edit modal) ----
  nameLabel: 'Display name *',
  protocolLabel: 'Protocol',
  priorityLabel: 'Priority (higher first)',
  proxyAccess: 'Access via platform proxy',
  proxyAdminNote: 'Proxy address is configured by the admin',

  // ---- Common errors / status ----
  errRateLimited: 'Quota queries are rate-limited, please try again later',
  errModelMapJson: 'model_map must be a JSON object, e.g. {"gpt-4o":"gpt-4o-2024-11-20"}',
  errMapFormat: 'Invalid model-map JSON — fix it before editing',
  fetching: 'Fetching…',
  fetchBtn: '↻ Fetch models',
  fetchFailed: 'Fetch failed',
  requestingUpstream: 'Requesting upstream…',
  testing: 'Testing…',
  unknownError: 'Unknown error',

  // ---- finish_reason labels ----
  finish: {
    stop: 'Completed',
    length: 'Truncated at limit',
    toolCalls: 'Tool calls',
    functionCall: 'Function calls',
    contentFilter: 'Content filtered',
  },

  // ---- Wire protocol display names (card subtitle) ----
  protocol: {
    openai: 'OpenAI-compatible',
    anthropic: 'Anthropic',
    gemini: 'Gemini',
  },

  // ---- Add wizard ----
  wizard: {
    typeApiKeyDesc: 'GLM / DeepSeek / SiliconFlow / Volcengine Ark / Qianfan / Hunyuan / MiMo / MiniMax, or any OpenAI-compatible endpoint',
    typeOauth: 'OAuth subscription',
    typeOauthDesc: 'Kimi / iFlow / Qoder / Trae / CodeBuddy / OpenAI Codex / Claude Pro authorized login',
    quickPick: 'Quick pick',
    quickPickOauth: ' (OAuth platforms)',
    quickPickApi: ' (popular CN platforms)',
    namePhOauth: 'My Kimi',
    namePhApi: 'My DeepSeek',
    baseUrlLabel: 'Base URL * (OpenAI-compatible)',
    baseUrlOauthLabel: 'Upstream URL * (Responses endpoint)',
    baseUrlOauthHint: 'Upstream URL for OAuth channels; Codex requires the full endpoint (…/backend-api/codex/responses). Leave empty to use the platform default.',
    getApiKey: 'Get API key',
    builtinModels: 'Built-in models: {{models}}',
    modelsMore: ' +{{count}} more',
    apiKeyLabel: 'API key *',
    fetchHint: 'Fill in the Base URL and API key to fetch the upstream model list',
    prevStep: '← Back',
    createAndAuth: 'Create & authorize →',
  },

  // ---- OAuth authorization modal ----
  oauth: {
    title: 'OAuth authorization',
    step1Device: '1. Open the authorization page',
    openHost: 'Open {{host}} ↗',
    step2Device: '2. Enter the code',
    waiting: 'Waiting for confirmation…',
    step1Browser: '1. Complete the login in the new window',
    openAuthPage: 'Open authorization page ↗',
    step2PastePre: '2. After login the browser redirects to',
    step2PastePost: " (the page won't open — that's expected). Copy the full URL from the address bar and paste it here",
    submitting: 'Submitting…',
    done: 'Finish authorization',
    step2Poll: '2. Once authorized, click the button below',
    iHaveAuthorized: 'I have authorized — continue',
  },

  // ---- Model probing / testing modal ----
  models: {
    title: 'Models · {{name}}',
    subtitle: 'Fetch available models from the upstream via GET /v1/models',
    listCount: '{{count}} models in total; select one to test its availability:',
    clickToSelect: 'Click to select',
    noneReturned: 'The upstream returned no models.',
    availLabel: 'Test model availability (quick check)',
    availPlaceholder: 'gpt-4o-mini (type manually or pick from the list above)',
    quickTest: 'Quick check',
    minRequest: 'Sending a minimal max_tokens=1 request to the upstream…',
    availOk: '✅ {{model}} available (HTTP {{status}}, {{latency}})',
    availFail: '❌ {{model}} unavailable: {{error}}',
    realLabel: 'Simulate a real request (sends an actual prompt instead of a bare ping)',
    presetTitle: 'Prompt: {{prompt}}',
    promptPlaceholder: 'Prompt to send to the model (pick a type above to auto-fill)',
    nonstreamTest: 'Non-stream test',
    streamTest: 'Stream test',
    stopTest: 'Stop',
    streaming: 'Receiving…',
    stopped: 'Stopped manually',
    sendingRealStream: 'Sending a real streaming request to the upstream…',
    sendingRealNonstream: 'Sending a real non-streaming request to the upstream…',
    ok: '✅ Success',
    fail: '❌ Failed',
    stream: 'Stream',
    nonstream: 'Non-stream',
    totalLatency: 'Total',
    firstToken: 'First token',
    finishReason: 'Finish reason',
    promptTokens: 'Input',
    completionTokens: 'Output',
    promptLabel: 'Prompt',
    thinking: 'Reasoning ({{count}} chars)',
    contentLabel: 'Output',
    historyLabel: 'Test history (click a row for details)',
  },

  // ---- Edit modal ----
  edit: {
    title: 'Edit: {{name}}',
    oauthLocked: "🔐 Authorized channels can't switch platforms; delete and re-add to use a different subscription.",
    apiKeyLabel: 'API key (leave empty to keep unchanged)',
    enableChannel: 'Enable this channel',
  },

  // ---- Quota display (openai codex /wham/usage snapshot) ----
  quota: {
    // Window labels
    winDefault: 'Window',
    win5h: '5h window',
    winDaily: 'Today',
    winWeekly: 'This week',
    winMonthly: 'This month',
    winYearly: 'This year',
    // Reset countdown / freshness
    resetNow: 'Resets soon',
    resetInMinutes: 'Resets in {{mins}} min',
    resetInHours: 'Resets in {{hours}} h',
    resetInDays: 'Resets in {{days}} d',
    justNow: 'Just now',
    minutesAgo: '{{mins}} min ago',
    hoursAgo: '{{hours}} h ago',
    daysAgo: '{{days}} d ago',
    // Compact block on channel card
    cardTitle: 'Click for quota details',
    syncFailed: 'Quota sync failed: {{error}}',
    noData: 'No quota data yet',
    limitReached: 'Limited',
    resetCreditsTitle: 'Rate-limit reset credit: spend one to clear the current rate-limit window immediately',
    resetCredits: '⚡ Resets ×{{n}}',
    relaySuffix: ' · from relay',
    // Detail modal
    title: 'Quota · {{name}}',
    neverSynced: 'No quota snapshot synced yet.',
    querying: 'Querying…',
    queryNow: 'Query now',
    relaySource: 'From relay',
    proactiveSource: 'Proactive query',
    updatedAt: 'updated {{ago}}',
    lastSyncFailed: 'Last sync failed: {{error}} (showing last successful data)',
    allowed: 'Requests allowed',
    resetCreditsAvail: '⚡ Reset credits ×{{n}}',
    creditsTitle: 'Pay-as-you-go credits balance',
    credits: '💰 credits {{balance}}',
    creditsUnlimited: 'unlimited',
    additionalLimits: 'Per-model extra limits',
    remaining: '~{{n}}% left',
  },

  // ---- Billing name map editor ----
  billing: {
    label: 'Billing name map (optional · channel alias → price-list name)',
    descPre: 'When an upstream model name differs from the price list (e.g.',
    descMid: 'is actually',
    descPost: '), map it here so billing uses the standard name; mapping only renames, never changes prices.',
    renameTitle: 'Delete and re-add to rename',
    pickPlaceholder: 'Pick a price-list model…',
    deleteTitle: 'Delete this mapping',
    incomplete: 'Some mappings have no standard model selected — complete or delete them before saving',
    inputPlaceholder: 'Request model name, e.g. my-alias',
    unpriced: 'Models not in the price list:',
    addAsMapping: 'Add as billing mapping',
    noPrices: 'The price list is empty (unpriced models are not billed) — ask the admin to configure "Model pricing" first.',
  },

  // ---- Model picker ----
  picker: {
    label: 'Enabled models (checked models are served by the gateway)',
    enabledCount: '{{n}} enabled',
    fetchTitle: 'Fetch from the upstream GET /v1/models',
    selectAll: 'Select all',
    clear: 'Clear',
    filterPlaceholder: 'Filter models…',
    noMatch: 'The upstream returned no matching models.',
    manualPlaceholder: 'Add a model name manually (not in the upstream list)',
    advanced: 'Advanced: rename mapping (JSON: platform name → upstream name)',
  },

  // ---- Test preset type labels ----
  testPreset: {
    chat: 'Chat',
    knowledge: 'Knowledge',
    math: 'Math',
    reason: 'Reasoning',
    long: 'Long text',
    code: 'Code',
    custom: 'Custom',
  },

  // ---- Test preset prompts (payload sent to the model, follows UI language) ----
  testPrompt: {
    chat: 'Hello, please introduce yourself in one sentence.',
    knowledge: 'Explain in detail the TCP three-way handshake and four-way teardown, including the state changes at each stage.',
    math: 'Solve the equations: 2x + 3y = 12 and 4x - y = 5. Give the values of x and y.',
    reason: 'There are 12 identical-looking balls, 11 of the same weight and 1 of a different weight. What is the minimum number of balance-scale weighings needed to find the odd ball? Describe the plan.',
    long: "Explain in detail the Schrödinger's cat thought experiment in quantum mechanics, including its physical meaning and its contribution to quantum mechanics.",
    code: 'Implement quicksort in Python, with comments and a simple example.',
  },
}

export default providers
