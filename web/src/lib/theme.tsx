import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

type Mode = 'light' | 'dark'

const KEY = 'theme'

function apply(mode: Mode) {
  document.documentElement.classList.toggle('dark', mode === 'dark')
}

export function useTheme() {
  const [mode, setMode] = useState<Mode>(() => {
    const saved = localStorage.getItem(KEY)
    if (saved === 'light' || saved === 'dark') return saved
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  })

  useEffect(() => {
    apply(mode)
    localStorage.setItem(KEY, mode)
  }, [mode])

  return { mode, toggle: () => setMode((m) => (m === 'dark' ? 'light' : 'dark')) }
}

export function ThemeToggle() {
  const { mode, toggle } = useTheme()
  const { t } = useTranslation()
  return (
    <button onClick={toggle} className="btn-ghost" title={mode === 'dark' ? t('common.toLight') : t('common.toDark')}>
      {mode === 'dark' ? t('common.labelDark') : t('common.labelLight')}
    </button>
  )
}
