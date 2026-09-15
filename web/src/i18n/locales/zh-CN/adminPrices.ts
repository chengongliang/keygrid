// 管理端 · 模型定价页文案
const adminPrices = {
  title: '模型定价',
  syncButton: '一键同步',
  addButton: '新增定价',
  edit: '编辑定价',
  // ---- 页面说明（中间嵌 * 占位 span，拆为两段）----
  intro1: '单价单位：USD / 1M tokens（对齐官方 API 定价）。计费匹配：精确模型名 > ',
  intro2: ' 兜底行 > 未定价（免费放行、不计额度）。调价不影响历史费用（按请求时价格快照）。',
  // ---- 未定价提醒 ----
  unpricedBanner: '渠道已用但未定价的模型（{{n}}）—— 未定价模型不计费，建议补价',
  addPriceTip: '点击补价',
  unpricedSource: '来自各渠道 model_map 配置（无 map 的渠道无法枚举，非完整列表）',
  // ---- 表单校验 / 提示 ----
  errModelRequired: '模型名必填',
  errNonNegative: '单价必须是非负数字',
  savedMsg: '已保存 {{model}}',
  deletedMsg: '已删除 {{model}}（该模型回到未定价 = 免费）',
  syncDoneMsg: '同步完成：导入 {{n}} 条定价（来源 OpenRouter）',
  searchPlaceholder: '搜索模型名 / 备注',
  // ---- 表头 ----
  colInPrice: '输入单价',
  colOutPrice: '输出单价',
  colUpdatedAt: '更新时间',
  fallbackTip: '兜底价：未定价模型按此价计费',
  fallbackBadge: '兜底价 *',
  noMatch: '无匹配条目',
  emptyTable: '价格表为空 —— 未定价模型一律免费放行；点右上角「新增定价」或从顶部提醒一键补价',
  saving: '保存中…',
  // ---- 新增 / 编辑弹窗 ----
  labelModelA: '模型名（请求入口名；填',
  labelModelB: '配置兜底价）',
  modelPlaceholder: '如 gpt-4o 或 *',
  labelInPriceFull: '输入单价（USD / 1M tokens）',
  labelOutPriceFull: '输出单价（USD / 1M tokens）',
  labelRemark: '备注（可选）',
  remarkPlaceholder: '如：对齐官方 API 定价',
  formulaExample: '示例：官方价 $2.50 / 1M 输入 tokens → 填 2.5。费用 = tokens ÷ 1M × 单价。',
  // ---- 删除确认弹窗（中间嵌模型名 span，拆为两段）----
  deleteTitle: '删除定价',
  deleteConfirmA: '确定删除 ',
  deleteConfirmB: ' 的定价？删除后该模型回到「未定价 = 免费」语义（不再计费、不计入额度累计）。',
  // ---- 一键同步（OpenRouter）弹窗 ----
  syncModalTitle: '一键同步定价 · OpenRouter',
  syncIntro: '从 OpenRouter 公开模型目录拉取价格（USD / 1M tokens，已自动换算；模型名取目录 id 末段）。勾选后写入：新增条目直接入表，已有条目按勾选覆盖。',
  syncLoadingText: '正在从 OpenRouter 拉取价格…',
  filterPlaceholder: '筛选模型名',
  syncColIn: '输入价',
  syncColOut: '输出价',
  statusNew: '新增',
  statusUpdate: '更新',
  statusSame: '一致',
  currentPriceTip: '现价 {{from}} → {{to}}',
  syncSummary: '共 {{total}} 条：新增 {{created}} · 有变 {{updated}} · 一致 {{same}}',
  syncSelected: '已勾选 {{n}} 条',
  importing: '导入中…',
  importChecked: '导入已勾选（{{n}}）',
  syncEmpty: '同步源未返回可用条目',
}

export default adminPrices
