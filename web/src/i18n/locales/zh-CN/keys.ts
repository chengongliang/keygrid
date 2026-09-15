// 端点与密钥页文案
const keys = {
  title: '端点与密钥',

  // ---- 接入信息卡片 ----
  apiEndpoint: 'API 端点',
  copyBaseUrl: '复制 Base URL',
  anthropicSdkTip: 'Anthropic SDK 填站点根地址（不带 /v1，SDK 自己拼路径）',
  auth: '鉴权',
  or: '或',
  authBothTip: '均用下方新建的 Key',
  chat: '对话',
  modelsEndpoint: '模型列表',
  copyExample: '复制示例',
  introOpenai: '三个入口都能路由到你配置的任意渠道（协议在网关内自动转换，模型名不限于单一生态）。OpenAI 兼容客户端（Cursor、Cherry Studio、LobeChat 等）填 Base URL + Key 即可；Anthropic 生态（Claude Code 等）设',
  introAnthropic: '，Key 同样用 sk-。模型名需在渠道支持范围内。',

  // ---- 接入示例代码占位符 ----
  example: {
    yourKey: 'sk-你的Key',
    modelName: '模型名',
    hello: '你好',
  },

  // ---- 列设置 ----
  view: '查看',
  showCols: '显示列',
  col: {
    key: '密钥',
    ip: 'IP 限制',
    quota: '额度',
    created: '创建时间',
    lastUsed: '最后使用时间',
    expires: '过期',
  },

  // ---- Key 表格 ----
  newKey: '+ 新建 Key',
  unnamed: '未命名',
  clickToggle: '点击启用/禁用',
  copyFullKey: '复制完整 Key',
  unlimited: '不限',
  neverUsed: '从未使用',
  noExpiry: '永不过期',
  empty: '还没有 API Key',

  // ---- 新建弹窗 ----
  createTitle: '新建 API Key',
  expiryOptional: '有效期（可选）',
  modelLimitOptional: '模型限制（可选，逗号分隔）',
  modelLimitPlaceholder: '留空 = 不限，如 gpt-4o,deepseek-chat',
  ipWhitelistOptional: 'IP 白名单（可选，逗号分隔 IP/CIDR）',
  ipWhitelistPlaceholder: '留空 = 不限，如 1.2.3.4,10.0.0.0/8',
  quotaLimitOptional: '额度上限（可选，USD）',
  quotaPlaceholder: '留空 = 不限，如 10',

  // ---- 编辑弹窗 ----
  editTitle: '编辑 Key · {{prefix}}…',
  expiryTime: '过期时间',
  modelLimit: '模型限制（逗号分隔）',
  ipWhitelist: 'IP 白名单（逗号分隔 IP/CIDR）',
  quotaLimit: '额度上限（USD，留空 = 不限）',
  quotaPlaceholderShort: '如 10',
  quotaHint: '按该 Key 的费用消耗累计；超限后转发返回 402。改小不追讨已消耗部分。',
  resetQuota: '同时重置已用额度（当前已用 {{used}}）',
  pleasePickExpiry: '请选择过期时间，或勾选永不过期',

  // ---- 创建成功弹窗 ----
  createdTitle: '✅ Key 已创建',

  // ---- 额度列 ----
  noQuotaTitle: '未设额度上限，超限不拦截',
  usedAmount: '已用 {{amount}}',
  quotaCellTitle: '已用 {{used}} / 上限 {{limit}}',
  overQuota: '已超限',

  // ---- 删除确认 ----
  deleteConfirm: '删除 key「{{name}}…」？使用该 key 的应用会立即失效。',
}

export default keys
