// format.ts 统计/明细页共用的格式化函数（原在 StatsPage.tsx 内，请求明细独立成页后抽出）。

/** 耗时格式化：<1s 用 ms，否则秒（两位小数）。0/空显示 —。 */
export function fmtMs(v: number): string {
  if (!v) return '—'
  return v >= 1000 ? `${(v / 1000).toFixed(2)}s` : `${Math.round(v)}ms`
}

/** 大 token 数缩写（1.2M / 345.6K / 123）。 */
export function fmtTok(v: number): string {
  if (!v) return '0'
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(2)}M`
  if (v >= 1_000) return `${(v / 1_000).toFixed(1)}K`
  return String(v)
}

/** credit 格式化（4 位小数，0 显示 0）。 */
export function fmtCredit(v: number): string {
  return v ? v.toFixed(4) : '0'
}

/** 单条明细时间列：RFC3339 → 本地 HH:MM:SS。 */
export function fmtClock(iso: string): string {
  const d = new Date(iso)
  return isNaN(d.getTime()) ? iso : d.toLocaleTimeString('zh-CN', { hour12: false })
}

/** 完整本地时间：RFC3339 → "YYYY-MM-DD HH:MM:SS"（行展开/悬浮用，消除跨天歧义）。 */
export function fmtFull(iso: string): string {
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

/** 单条请求状态配色：2xx 绿、4xx 橙、5xx/其他 红。 */
export function statusTone(status: number): string {
  if (status >= 200 && status < 300) return 'text-ok'
  if (status >= 400 && status < 500) return 'text-warn'
  return 'text-danger'
}
