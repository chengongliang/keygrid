import { useTranslation } from 'react-i18next'

// 顶栏语言切换按钮（仿 ThemeToggle：轻按钮 + emoji 标识，点击在中/英间切换）
export function LanguageToggle() {
  const { i18n, t } = useTranslation()
  const isZh = i18n.language.startsWith('zh')
  return (
    <button
      onClick={() => i18n.changeLanguage(isZh ? 'en' : 'zh-CN')}
      className="btn-ghost"
      title={t('common.switchLangTip')}
    >
      {isZh ? '中 / EN' : 'EN / 中'}
    </button>
  )
}
