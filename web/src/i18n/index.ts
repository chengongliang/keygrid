import i18n from 'i18next'
import { initReactI18next } from 'react-i18next'
import zhCN from './locales/zh-CN'
import en from './locales/en'

const LANG_KEY = 'lang'

// 初始语言：localStorage 手动选择优先，否则按浏览器语言（zh 开头 → 中文）
const saved = localStorage.getItem(LANG_KEY)
const initial = saved || (navigator.language.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en')

i18n.use(initReactI18next).init({
  resources: {
    'zh-CN': { translation: zhCN },
    en: { translation: en },
  },
  lng: initial,
  // 翻译缺失时回退中文（中文为主语言，新增文案先落中文）
  fallbackLng: 'zh-CN',
  defaultNS: 'translation',
  // React 自带 XSS 防护，插值无需转义
  interpolation: { escapeValue: false },
  returnEmptyString: false,
})

// 语言切换时持久化，下次进入记住选择
i18n.on('languageChanged', (lng) => {
  localStorage.setItem(LANG_KEY, lng)
  // 同步 <html lang>，利于字体渲染与无障碍
  document.documentElement.lang = lng
})
document.documentElement.lang = initial

export default i18n
