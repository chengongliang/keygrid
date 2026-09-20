import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Server, KeyRound, BarChart3, Users2, Gauge, SlidersHorizontal,
  Settings, Menu, LogOut, ChevronRight, DollarSign, Activity,
  PanelLeftClose, PanelLeftOpen,
} from 'lucide-react'
import Login from '@/pages/Login'
import Providers from '@/pages/Providers'
import ApiKeys from '@/pages/ApiKeys'
import Usage from '@/pages/Usage'
import SettingsPage from '@/pages/Settings'
import AdminUsers from '@/pages/AdminUsers'
import AdminProviders from '@/pages/AdminProviders'
import AdminUsage from '@/pages/AdminUsage'
import AdminPrices from '@/pages/AdminPrices'
import AdminSettings from '@/pages/AdminSettings'
import { api, type User } from '@/lib/api'
import { ThemeToggle } from '@/lib/theme'
import { LanguageToggle } from '@/i18n/LanguageToggle'
import { useTranslation } from 'react-i18next'

type Tab = 'providers' | 'keys' | 'usage' | 'settings' | 'admin_users' | 'admin_providers' | 'admin_usage' | 'admin_prices' | 'admin_settings'

// 侧边栏分组导航（参考 dashboard 模板：分组标题 + 图标项 + 底部固定项）
// label/title 存 i18n key，渲染处用 t() 转换（语言切换时重渲染生效）
const SECTION_MAIN = [
  {
    titleKey: 'app.sectionChannels',
    items: [
      { key: 'providers' as Tab, labelKey: 'app.tabProviders', icon: Server },
      { key: 'keys' as Tab, labelKey: 'app.tabKeys', icon: KeyRound },
    ],
  },
  {
    titleKey: 'app.sectionStats',
    items: [
      { key: 'usage' as Tab, labelKey: 'app.tabUsage', icon: BarChart3 },
    ],
  },
]

const SECTION_ADMIN = [
  {
    titleKey: 'app.sectionAdmin',
    items: [
      { key: 'admin_users' as Tab, labelKey: 'app.tabAdminUsers', icon: Users2 },
      { key: 'admin_providers' as Tab, labelKey: 'app.tabAdminProviders', icon: Activity },
      { key: 'admin_usage' as Tab, labelKey: 'app.tabAdminUsage', icon: Gauge },
      { key: 'admin_prices' as Tab, labelKey: 'app.tabAdminPrices', icon: DollarSign },
      { key: 'admin_settings' as Tab, labelKey: 'app.tabAdminSettings', icon: SlidersHorizontal },
    ],
  },
]

const TAB_KEY: Record<Tab, string> = {
  providers: 'app.tabProviders',
  keys: 'app.tabKeys',
  usage: 'app.tabUsage',
  settings: 'app.tabSettings',
  admin_users: 'app.tabAdminUsers',
  admin_providers: 'app.tabAdminProviders',
  admin_usage: 'app.tabAdminUsage',
  admin_prices: 'app.tabAdminPrices',
  admin_settings: 'app.tabAdminSettings',
}

export default function App() {
  const { t } = useTranslation()
  const [token, setToken] = useState<string | null>(() => localStorage.getItem('token'))
  const [user, setUser] = useState<User | null>(null)
  // 初始 tab：URL hash 优先（深链），其次恢复上次停留的页面（刷新/重开浏览器友好），最后默认我的供应商
  const [tab, setTab] = useState<Tab>(() => ((location.hash.slice(1) || localStorage.getItem('tab') || 'providers') as Tab))
  const [expired, setExpired] = useState(false)
  // 公告横幅：结构化存储，渲染时再拼接文案（保证语言切换实时生效；announcement 为后端自定义文本）
  const [banner, setBanner] = useState<{ maintenance: boolean; announcement: string } | null>(null)
  const [mobileNavOpen, setMobileNavOpen] = useState(false)
  const [profileOpen, setProfileOpen] = useState(false)
  // 桌面侧边栏折叠（仅 lg+ 生效；偏好持久化，移动端抽屉不受影响）
  const [navCollapsed, setNavCollapsed] = useState(() => localStorage.getItem('nav-collapsed') === '1')
  const toggleNavCollapsed = useCallback(() => {
    setNavCollapsed((prev) => {
      localStorage.setItem('nav-collapsed', prev ? '0' : '1')
      return !prev
    })
  }, [])

  // 标记本次 user 加载源于「新登录」（登录表单 / SSO 回跳）：用于 admin 首次落入默认首页。
  // 刷新恢复会话（/api/auth/me）不标记，避免每次刷新都被强拉回默认页
  const justLoggedIn = useRef(false)

  // admin 默认首页 = 平台用量：仅新登录且当前停在普通默认页（无明确意图 hash）时切换
  useEffect(() => {
    if (user?.role === 'admin' && justLoggedIn.current) {
      justLoggedIn.current = false
      const cur = (location.hash.slice(1) || 'providers') as Tab
      if (cur === 'providers') {
        location.hash = 'admin_usage'
        setTab('admin_usage')
      }
    }
  }, [user?.role])

  // SSO 回跳落地 —— callback 302 回 #/…?sso_token=…；换掉 token 后清掉 URL 里的敏感参数
  useEffect(() => {
    const m = location.hash.match(/^#\/?(.*)\?sso_token=([^&]+)/)
    if (m) {
      const t = decodeURIComponent(m[2])
      localStorage.setItem('token', t)
      setToken(t)
      // SSO 回跳等同新登录：admin 首次落入默认首页
      justLoggedIn.current = true
      // 保留原 hash 路由，去掉 sso_token
      history.replaceState(null, '', location.pathname + '#' + (m[1] ? m[1].replace(/\?.*$/, '') : ''))
    }
  }, [])

  const logout = useCallback((reason?: string) => {
    localStorage.removeItem('token')
    localStorage.removeItem('user')
    setToken(null)
    setUser(null)
    setExpired(!!reason)
    location.hash = ''
    // RP-Initiated Logout —— IdP 配了 end_session 会 302 跳 IdP 登出再回登录页
    fetch('/api/auth/logout', { redirect: 'follow' }).catch(() => {})
  }, [])

  useEffect(() => {
    if (!token) return
    api.get<User>('/api/auth/me')
      .then(setUser)
      .catch((e) => {
        if (e.status === 401) logout()
      })
  }, [token, logout])

  // 平台公告（登录后轮询一次；维护提示对全员可见）
  useEffect(() => {
    api.get<{ announcement: string; maintenance_mode: boolean }>('/api/config')
      .then((c) => setBanner({ maintenance: !!c.maintenance_mode, announcement: c.announcement || '' }))
      .catch(() => {})
  }, [token])

  useEffect(() => {
    const onHash = () => {
      const t = ((location.hash.slice(1) || 'providers') as Tab)
      setTab(t)
      // 记住停留页面：无 hash 访问（直接输域名/书签根路径）时也能恢复
      localStorage.setItem('tab', t)
    }
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  if (!token) {
    return (
      <Login
        expired={expired}
        onLoggedIn={(t, u) => {
          localStorage.setItem('token', t)
          localStorage.setItem('user', JSON.stringify(u))
          setToken(t)
          setUser(u)
          setExpired(false)
          // 新登录：admin 首次落入默认首页（无明确意图 hash 时）
          justLoggedIn.current = true
        }}
      />
    )
  }

  const isAdmin = user?.role === 'admin'
  // 非 admin 误入 admin 页（如 JWT 里 role 过期前被降级）→ 弹回
  // 默认首页：admin → 平台用量；普通用户 → 我的供应商（仅在无 hash 时生效，保留深链）
  const activeTab: Tab = tab.startsWith('admin_') && !isAdmin
    ? 'providers'
    : tab || 'providers'

  const switchTab = (t: Tab) => {
    location.hash = t
    setTab(t)
    setMobileNavOpen(false)
    setProfileOpen(false)
  }

  const navItem = (t2: { key: Tab; labelKey: string; icon: typeof Server }) => (
    <button
      key={t2.key}
      onClick={() => switchTab(t2.key)}
      // 折叠态（仅 lg+）：图标居中、文字隐藏，原生 title 作提示
      title={navCollapsed ? t(t2.labelKey) : undefined}
      className={`nav-item flex w-full items-center gap-3 rounded-lg px-3 py-2 text-sm transition ${activeTab === t2.key ? 'nav-active' : 'text-muted hover:opacity-90'} ${navCollapsed ? 'lg:justify-center lg:px-0' : ''}`}
    >
      <t2.icon className="h-4 w-4 shrink-0" />
      <span className={navCollapsed ? 'lg:hidden' : ''}>{t(t2.labelKey)}</span>
    </button>
  )

  return (
    <div className="min-h-screen">
      {/* 移动端汉堡按钮 */}
      <button
        type="button"
        className="fixed left-4 top-4 z-[70] rounded-lg p-2 shadow-md lg:hidden"
        style={{ background: 'var(--panel-solid)' }}
        onClick={() => setMobileNavOpen(!mobileNavOpen)}
      >
        <Menu className="h-5 w-5 text-muted" />
      </button>

      {/* 侧边栏（桌面可折叠成图标窄条，折叠偏好持久化；移动端抽屉行为不变） */}
      <nav
        className={`fixed inset-y-0 left-0 z-[70] w-64 border-r transition-all duration-200 ease-in-out lg:translate-x-0 ${mobileNavOpen ? 'translate-x-0' : '-translate-x-full'} ${navCollapsed ? 'lg:w-16' : 'lg:w-64'}`}
        style={{ background: 'var(--panel-solid)', borderColor: 'var(--line)' }}
      >
        {/* 折叠开关：悬浮在侧边栏右缘，展开/折叠两态通用 */}
        <button
          type="button"
          onClick={toggleNavCollapsed}
          title={t(navCollapsed ? 'app.expandSidebar' : 'app.collapseSidebar')}
          aria-label={t(navCollapsed ? 'app.expandSidebar' : 'app.collapseSidebar')}
          className="absolute -right-3 top-20 z-10 hidden h-6 w-6 items-center justify-center rounded-full border shadow-sm transition hover:opacity-90 lg:flex"
          style={{ background: 'var(--panel-solid)', borderColor: 'var(--line)', color: 'var(--muted)' }}
        >
          {navCollapsed ? <PanelLeftOpen className="h-3.5 w-3.5" /> : <PanelLeftClose className="h-3.5 w-3.5" />}
        </button>
        <div className="flex h-full flex-col">
          <div className={`flex h-16 shrink-0 items-center border-b ${navCollapsed ? 'lg:justify-center' : 'px-6'}`} style={{ borderColor: 'var(--line)' }}>
            <div className="flex items-center gap-3">
              <img src="/logo.png" alt="KeyGrid" className={`h-9 w-auto ${navCollapsed ? 'lg:hidden' : ''}`} draggable={false} />
              {/* 折叠态（仅 lg+）：方形图标版 logo */}
              <img src="/icon.png" alt="KeyGrid" className={`h-9 w-9 shrink-0 ${navCollapsed ? 'lg:block' : 'hidden'}`} draggable={false} />
            </div>
          </div>
          <div className="flex-1 overflow-y-auto px-4 py-5">
            <div className="space-y-6">
              {[...SECTION_MAIN, ...(isAdmin ? SECTION_ADMIN : [])].map((section) => (
                <div key={section.titleKey}>
                  {/* 展开态：分组标题；折叠态：短分隔线 */}
                  <div className={`mb-2 px-3 text-xs font-semibold uppercase tracking-wider text-muted ${navCollapsed ? 'lg:hidden' : ''}`}>
                    {t(section.titleKey)}
                  </div>
                  <div className={`mx-auto h-px w-6 ${navCollapsed ? 'mb-4 hidden lg:block' : 'hidden'}`} style={{ background: 'var(--line)' }} />
                  <div className="space-y-1">{section.items.map(navItem)}</div>
                </div>
              ))}
            </div>
          </div>
          <div className="border-t px-4 py-4" style={{ borderColor: 'var(--line)' }}>
            <div className="space-y-1">
              {navItem({ key: 'settings', labelKey: 'app.tabSettings', icon: Settings })}
            </div>
          </div>
        </div>
      </nav>

      {/* 移动端遮罩 */}
      {mobileNavOpen && (
        <div className="fixed inset-0 z-[65] bg-black/50 lg:hidden" onClick={() => setMobileNavOpen(false)} />
      )}

      {/* 主内容区：左边距跟随侧边栏展开/折叠 */}
      <div className={`flex min-h-screen min-w-0 flex-col transition-[margin] duration-200 ease-in-out ${navCollapsed ? 'lg:ml-16' : 'lg:ml-64'}`}>
        {/* 顶栏：面包屑 + 主题 + 用户下拉 */}
        <header className="sticky top-0 z-10 h-16 border-b backdrop-blur" style={{ borderColor: 'var(--line)', background: 'color-mix(in oklab, var(--bg) 88%, transparent)' }}>
          <nav className="flex h-full items-center justify-between px-4 sm:px-6">
            <div className="ml-12 hidden items-center gap-1 text-sm sm:flex lg:ml-0">
              <span className="text-muted">KeyGrid</span>
              <ChevronRight className="mx-1 h-4 w-4 text-muted" />
              <span className="font-medium">{t(TAB_KEY[activeTab])}</span>
            </div>
            <div className="ml-auto flex items-center gap-3">
              <LanguageToggle />
              <ThemeToggle />
              {/* 用户下拉（参考模板 profile card） */}
              <div className="relative">
                <button
                  type="button"
                  onClick={() => setProfileOpen(!profileOpen)}
                  className="flex h-8 w-8 items-center justify-center rounded-full text-sm font-medium ring-2"
                  style={{ background: 'var(--panel)', color: 'var(--text)', boxShadow: '0 0 0 1px var(--line)' }}
                  title={user?.email}
                >
                  {(user?.email ?? '?').slice(0, 1).toUpperCase()}
                </button>
                {profileOpen && (
                  <>
                    <div className="fixed inset-0 z-40" onClick={() => setProfileOpen(false)} />
                    <div
                      className="absolute right-0 top-10 z-50 w-72 overflow-hidden rounded-2xl border shadow-lg"
                      style={{ background: 'var(--panel-solid)', borderColor: 'var(--line)' }}
                    >
                      <div className="px-5 pb-4 pt-6">
                        <div className="mb-4 flex items-center gap-4">
                          <div className="relative shrink-0">
                            <div
                              className="flex h-14 w-14 items-center justify-center rounded-full text-xl font-semibold"
                              style={{ background: 'var(--panel)', color: 'var(--text)', boxShadow: '0 0 0 1px var(--line)' }}
                            >
                              {(user?.email ?? '?').slice(0, 1).toUpperCase()}
                            </div>
                            <div className="absolute bottom-0 right-0 h-3.5 w-3.5 rounded-full bg-emerald-500 ring-2" style={{ boxShadow: '0 0 0 2px var(--panel-solid)' }} />
                          </div>
                          <div className="min-w-0">
                            <h2 className="truncate text-base font-semibold">{user?.email ?? '…'}</h2>
                            <p className="mt-0.5 text-xs text-muted">
                              {isAdmin ? t('app.roleAdmin') : t('app.roleMember')}
                            </p>
                          </div>
                        </div>
                        <div className="my-3 h-px" style={{ background: 'var(--line)' }} />
                        <button
                          type="button"
                          onClick={() => logout()}
                          className="flex w-full items-center gap-2 rounded-lg p-2 text-sm transition hover:opacity-80"
                          style={{ color: '#ef4444' }}
                        >
                          <LogOut className="h-4 w-4" />
                          {t('app.logout')}
                        </button>
                      </div>
                    </div>
                  </>
                )}
              </div>
            </div>
          </nav>
        </header>
        {banner && (banner.maintenance || banner.announcement) && (
          <div className="warn-well mx-4 mt-4 rounded-lg px-4 py-2 text-sm sm:mx-6" style={{ color: 'var(--text)' }}>
            {[banner.maintenance ? t('app.maintenanceBanner') : '', banner.announcement].filter(Boolean).join(' — ')}
          </div>
        )}
        <main className="flex-1 px-4 py-6 sm:px-6">
          <div className="mx-auto max-w-6xl">
            {activeTab === 'providers' && <Providers />}
            {activeTab === 'keys' && <ApiKeys />}
            {activeTab === 'usage' && <Usage />}
            {activeTab === 'settings' && <SettingsPage user={user} onLogout={logout} />}
            {activeTab === 'admin_users' && <AdminUsers />}
{activeTab === 'admin_providers' && <AdminProviders />}
            {activeTab === 'admin_usage' && <AdminUsage />}
            {activeTab === 'admin_prices' && <AdminPrices />}
            {activeTab === 'admin_settings' && <AdminSettings />}
          </div>
        </main>
      </div>
    </div>
  )
}
