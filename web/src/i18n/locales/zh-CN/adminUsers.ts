// 管理端 · 用户管理页文案
const adminUsers = {
  title: '用户管理',
  searchPlaceholder: '搜索邮箱 / 名字…',
  // 操作失败兜底文案（接口未返回错误信息时）
  actionFailed: '操作失败',
  // ---- 表头 ----
  colEmail: '邮箱',
  colRole: '角色',
  colProviders: '渠道',
  colRequests: '请求',
  colLastActive: '最近活跃',
  // ---- 行内操作与提示 ----
  promote: '提升',
  demote: '降级',
  promotedMsg: '已将 {{email}} 提升为 admin',
  demotedMsg: '已将 {{email}} 降级为 user',
  disabledMsg: '已禁用 {{email}}（其 API Key 一并失效）',
  enabledMsg: '已启用 {{email}}',
  resetPassword: '重置密码',
  noMatch: '没有匹配的用户',
  totalUsers: '共 {{total}} 个用户',
  // ---- 重置密码弹窗 ----
  resetPasswordTitle: '重置密码：{{email}}',
  newPasswordTip: '新密码（仅本次展示，请立即复制下发）：',
  done: '完成',
  resetConfirmTip: '将为该用户生成一个 16 位随机密码，原密码立即失效。确认？',
  confirmReset: '确认重置',
}

export default adminUsers
