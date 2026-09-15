// Admin · model pricing page copy
const adminPrices = {
  title: 'Model Pricing',
  syncButton: 'One-click sync',
  addButton: 'Add price',
  edit: 'Edit price',
  // ---- Page intro (a wildcard * span sits in the middle, split in two parts) ----
  intro1: 'Unit price in USD / 1M tokens (aligned with official API pricing). Billing match: exact model name > ',
  intro2: ' fallback row > unpriced (allowed for free, not counted against quota). Price changes do not affect historical costs (priced per request-time snapshot).',
  // ---- Unpriced reminder ----
  unpricedBanner: 'Models used by providers but unpriced ({{n}}) — unpriced models are free of charge; consider adding prices',
  addPriceTip: 'Click to add price',
  unpricedSource: 'From each provider\'s model_map config (providers without a map cannot be enumerated; not an exhaustive list)',
  // ---- Form validation / notices ----
  errModelRequired: 'Model name is required',
  errNonNegative: 'Unit prices must be non-negative numbers',
  savedMsg: 'Saved {{model}}',
  deletedMsg: 'Deleted {{model}} (back to unpriced = free)',
  syncDoneMsg: 'Sync complete: imported {{n}} prices (source: OpenRouter)',
  searchPlaceholder: 'Search model name / remark',
  // ---- Table headers ----
  colInPrice: 'Input price',
  colOutPrice: 'Output price',
  colUpdatedAt: 'Updated',
  fallbackTip: 'Fallback price: unpriced models are billed at this price',
  fallbackBadge: 'Fallback *',
  noMatch: 'No matching entries',
  emptyTable: 'Price table is empty — unpriced models are always allowed for free; click "Add price" at the top right or pick from the reminder above',
  saving: 'Saving…',
  // ---- Add / edit modal ----
  labelModelA: 'Model name (name used in requests; enter ',
  labelModelB: ' for the fallback price)',
  modelPlaceholder: 'e.g. gpt-4o or *',
  labelInPriceFull: 'Input price (USD / 1M tokens)',
  labelOutPriceFull: 'Output price (USD / 1M tokens)',
  labelRemark: 'Remark (optional)',
  remarkPlaceholder: 'e.g. aligned with official API pricing',
  formulaExample: 'Example: official price $2.50 / 1M input tokens → enter 2.5. Cost = tokens ÷ 1M × unit price.',
  // ---- Delete confirmation modal (model name span in the middle, split in two parts) ----
  deleteTitle: 'Delete price',
  deleteConfirmA: 'Delete pricing for ',
  deleteConfirmB: '? After deletion the model falls back to "unpriced = free" (no billing, no quota counted).',
  // ---- One-click sync (OpenRouter) modal ----
  syncModalTitle: 'One-click price sync · OpenRouter',
  syncIntro: 'Fetch prices from the public OpenRouter model catalog (USD / 1M tokens, converted automatically; model name uses the last segment of the catalog id). Selected entries are written: new ones are inserted, existing ones are overwritten per selection.',
  syncLoadingText: 'Fetching prices from OpenRouter…',
  filterPlaceholder: 'Filter model names',
  syncColIn: 'Input',
  syncColOut: 'Output',
  statusNew: 'New',
  statusUpdate: 'Update',
  statusSame: 'Same',
  currentPriceTip: 'Current price {{from}} → {{to}}',
  syncSummary: '{{total}} in total: {{created}} new · {{updated}} changed · {{same}} same',
  syncSelected: '{{n}} selected',
  importing: 'Importing…',
  importChecked: 'Import selected ({{n}})',
  syncEmpty: 'Sync source returned no usable entries',
}

export default adminPrices
