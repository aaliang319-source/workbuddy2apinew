// theme.ts 明暗主题切换：写入 <html data-theme> 并持久化到 localStorage。
//
// 暗色是默认主题（见 design/设计规范.md §1）。首次访问若系统偏好明色则用明色，
// 之后一律以用户显式选择为准。

import { useCallback, useEffect, useState } from 'react'

export type Theme = 'dark' | 'light'

const KEY = 'wb2api.theme'

function initialTheme(): Theme {
  const saved = localStorage.getItem(KEY)
  if (saved === 'dark' || saved === 'light') return saved
  return window.matchMedia?.('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
}

/** 返回当前主题与切换函数。切换会立即反映到 <html data-theme>。 */
export function useTheme(): [Theme, () => void] {
  const [theme, setTheme] = useState<Theme>(initialTheme)

  useEffect(() => {
    document.documentElement.dataset.theme = theme
    // 让浏览器原生控件（滚动条、表单）跟随主题。
    document.documentElement.style.colorScheme = theme
    localStorage.setItem(KEY, theme)
  }, [theme])

  const toggle = useCallback(() => setTheme((t) => (t === 'dark' ? 'light' : 'dark')), [])
  return [theme, toggle]
}
