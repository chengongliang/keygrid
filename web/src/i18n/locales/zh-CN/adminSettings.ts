// 管理端 · 系统设置页文案
const adminSettings = {
  title: '系统设置',
  // ---- 系统公告 ----
  announcement: {
    title: '系统公告',
    desc: '全员可见，展示在登录页与页面顶部；留空则不展示。',
    placeholder: '例如：本周六 22:00-23:00 系统维护，届时转发面暂不可用',
    saved: '公告已更新',
    save: '保存公告',
  },
  // ---- 维护模式 ----
  maintenance: {
    title: '维护模式',
    desc: '开启后 /v1 转发面对所有请求返回 503 + 公告文案；管理端与平台 Web 不受影响。',
    active: '维护中',
    normal: '正常服务',
    openBtn: '开启维护模式',
    closeBtn: '关闭维护模式',
    confirmMsg: '确认开启维护模式？所有 /v1 转发请求将立即返回 503。',
    openedMsg: '维护模式已开启',
    exitedMsg: '已退出维护模式',
  },
  // ---- 注册策略 ----
  register: {
    title: '注册策略',
    desc: '控制 /api/auth/register 的开放程度；首个管理员建号不受限制。',
    open: '开放注册',
    openDesc: '任何人可注册',
    invite: '邀请制',
    inviteDesc: '需有效邀请码',
    closed: '关闭注册',
    closedDesc: '禁止新用户注册',
    saved: '注册策略已切换为「{{name}}」',
  },
  // ---- 网络代理 ----
  proxy: {
    title: '网络代理',
    configured: '已配置',
    unconfigured: '未配置',
    desc: '部分上游站点（如 OpenAI）需通过代理才能访问。在此统一配置出口代理，用户添加/编辑渠道时勾选「通过平台代理访问」即可；代理地址对用户只读展示（密码已脱敏）、不可修改。支持 http(s) 与 socks5；留空 = 未配置代理（已勾选代理的渠道将无法转发）。保存后转发面最长 30 秒内热生效。',
    placeholder: 'http://127.0.0.1:7890 或 socks5://user:pass@host:1080',
    saved: '代理设置已保存',
    save: '保存代理',
  },
  // ---- OIDC 单点登录 ----
  oidc: {
    title: 'OIDC 单点登录',
    off: '未启用',
    desc: '兼容任何标准 OIDC IdP（Keycloak / Casdoor / Authentik / 飞书 SSO 等），Authorization Code + PKCE。保存后即时生效，无需重启；平台设置优先于 OIDC_* 环境变量。员工用 IdP 账号登录后自动建号。',
    copyCallback: '复制回调地址',
    callbackTip: '在 IdP 的客户端配置里把 Redirect URI 填为上面的回调地址。',
    enableLabel: '启用 OIDC 登录（登录页显示 SSO 按钮）',
    issuerLabel: 'WellKnown / Issuer 地址',
    displayNameLabel: '显示名称（登录按钮文案，可选）',
    displayNamePlaceholder: '企业账号',
    hide: '隐藏',
    show: '显示',
    scopesLabel: 'Scopes（空格或逗号分隔；需包含 openid 与 email）',
    saved: 'OIDC 配置已保存，已即时生效',
    save: '保存 OIDC 配置',
  },
  // ---- 邀请码 ----
  invite: {
    title: '邀请码',
    desc: '邀请制注册时使用；14 天有效，单次使用。',
    generate: '+ 生成邀请码',
    colCode: '邀请码',
    colExpires: '过期时间',
    used: '已使用',
    expired: '已过期',
    available: '可用',
  },
}

export default adminSettings
