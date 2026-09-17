// API keys & endpoints page
const keys = {
  title: 'API Keys & Endpoints',

  // ---- Endpoint info card ----
  apiEndpoint: 'API endpoint',
  copyBaseUrl: 'Copy base URL',
  anthropicSdkTip: 'For the Anthropic SDK, use the site root (no /v1 — the SDK appends the path itself)',
  auth: 'Auth',
  or: 'or',
  authBothTip: 'both work with a key created below',
  chat: 'Chat',
  modelsEndpoint: 'Model list',
  copyExample: 'Copy example',
  introOpenai: 'All three endpoints route to any channel you configured (protocols are converted inside the gateway, so model names are not tied to one ecosystem). OpenAI-compatible clients (Cursor, Cherry Studio, LobeChat, etc.) just need the Base URL + key. For the Anthropic ecosystem (Claude Code, etc.), set',
  introAnthropic: ' — the same sk- key works. Model names must be supported by your channels.',

  // ---- Placeholders inside code examples ----
  example: {
    yourKey: 'sk-your-key',
    modelName: 'model-name',
    hello: 'hello',
  },

  // ---- Column settings ----
  view: 'Columns',
  showCols: 'Show columns',
  col: {
    key: 'Key',
    providers: 'Channels',
    ip: 'IP restriction',
    quota: 'Quota',
    created: 'Created',
    lastUsed: 'Last used',
    expires: 'Expires',
  },

  // ---- Key table ----
  newKey: '+ New Key',
  unnamed: 'Unnamed',
  clickToggle: 'Click to enable/disable',
  copyFullKey: 'Copy full key',
  unlimited: 'Unlimited',
  neverUsed: 'Never used',
  noExpiry: 'Never expires',
  empty: 'No API keys yet',

  // ---- Create modal ----
  createTitle: 'Create API Key',
  expiryOptional: 'Expiry (optional)',
  modelLimitOptional: 'Model restriction (optional, comma-separated)',
  modelLimitPlaceholder: 'Empty = unlimited, e.g. gpt-4o,deepseek-chat',
  providerLimitOptional: 'Channel restriction (optional, none checked = all channels)',
  ipWhitelistOptional: 'IP allowlist (optional, comma-separated IP/CIDR)',
  ipWhitelistPlaceholder: 'Empty = unlimited, e.g. 1.2.3.4,10.0.0.0/8',
  quotaLimitOptional: 'Quota limit (optional, USD)',
  quotaPlaceholder: 'Empty = unlimited, e.g. 10',

  // ---- Edit modal ----
  editTitle: 'Edit Key · {{prefix}}…',
  expiryTime: 'Expiry time',
  modelLimit: 'Model restriction (comma-separated)',
  providerLimit: 'Channel restriction (none checked = unlimited)',
  providerLimitHint: 'Requests with this key only route to the checked channels; model matching, priority and failover rules still apply.',
  providerDeleted: 'deleted',
  noProviderForBinding: 'No channels yet — add one on the Providers page first.',
  providerOrphanHint: '{{count}} binding(s) point to deleted channels and will be cleared on save.',
  ipWhitelist: 'IP allowlist (comma-separated IP/CIDR)',
  quotaLimit: 'Quota limit (USD, empty = unlimited)',
  quotaPlaceholderShort: 'e.g. 10',
  quotaHint: 'Accumulates the cost of this key; forwarding returns 402 once exceeded. Already-spent usage is not reclaimed when lowering the limit.',
  resetQuota: 'Also reset used quota (currently used {{used}})',
  pleasePickExpiry: 'Pick an expiry time, or check "Never expires"',

  // ---- Created modal ----
  createdTitle: '✅ Key created',

  // ---- Quota cell ----
  noQuotaTitle: 'No quota limit set; overage is not blocked',
  usedAmount: 'Used {{amount}}',
  quotaCellTitle: 'Used {{used}} / limit {{limit}}',
  overQuota: 'over limit',

  // ---- Delete confirm ----
  deleteConfirm: 'Delete key "{{name}}…"? Apps using it will stop working immediately.',
}

export default keys
