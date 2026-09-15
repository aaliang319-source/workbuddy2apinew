// Requests.tsx 请求明细：单条请求级视图（网关侧保留最近 200 条，新→旧）。
// 与「请求统计」页的按模型聚合互补——聚合看趋势，明细看个案：
// 哪条请求慢了、失败了（错误分类）、被哪个号接了、扣了多少。
// 数据源同为网关 /v1/stats（stats.recent 字段）。
import { Fragment, useCallback, useEffect, useMemo, useState } from 'react'
import { api, ApiError } from '../api'
import type { Account, RequestRecord, SessionInfo, StatsResponse } from '../types'
import { Alert, Empty, Spinner, displayName } from '../ui'
import { fmtClock, fmtCredit, fmtFull, fmtMs, fmtTok, statusTone } from '../format'

export default function Requests({ session: _session }: { session: SessionInfo }) {
  const [resp, setResp] = useState<StatsResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [autoRefresh, setAutoRefresh] = useState(true)
  // 账号档案：uid 前 8 位 → 完整账号（昵称等），供明细行展示"这是谁"。
  const [accounts, setAccounts] = useState<Record<string, Account>>({})
  // 视图过滤（纯前端，作用于已拉取的 ≤200 条）。
  const [onlyFailed, setOnlyFailed] = useState(false)
  const [modelFilter, setModelFilter] = useState('')
  const [typeFilter, setTypeFilter] = useState<'all' | 'stream' | 'sync'>('all')
  // 排序（默认网关原生新→旧）+ 分页 + 行展开。
  const [sortKey, setSortKey] = useState<'time' | 'wall' | 'ttfb' | 'credit'>('time')
  // 每页条数可配置（默认 10），选择持久化到 localStorage（key: requests.pageSize）。
  const [pageSize, setPageSize] = useState(() => {
    const n = Number(localStorage.getItem('requests.pageSize'))
    return [10, 20, 50, 100].includes(n) ? n : 10
  })
  const [page, setPage] = useState(0)
  const [expanded, setExpanded] = useState<string | null>(null)

  const stats = resp?.stats ?? null

  const load = useCallback(async (silent = false) => {
    if (!silent) setLoading(true)
    try {
      const s = await api.stats()
      setResp(s)
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载请求明细失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // 账号档案随明细一起拉：uid 前 8 位 → 完整账号（昵称等），供明细行展示"这是谁"。
  // 静默失败不阻断明细展示（账号列退回 uid 显示）。
  const loadAccounts = useCallback(async () => {
    try {
      const r = await api.accounts()
      const byUID: Record<string, Account> = {}
      for (const a of r.accounts) byUID[a.uid.slice(0, 8)] = a
      setAccounts(byUID)
    } catch {
      // 忽略：账号列表不可用时明细照常展示
    }
  }, [])

  useEffect(() => {
    void loadAccounts()
  }, [loadAccounts])

  // 自动刷新：明细是滚动窗口，5 秒足够跟上；聚合页是 10 秒。
  useEffect(() => {
    if (!autoRefresh) return
    const timer = setInterval(() => void load(true), 5_000)
    return () => clearInterval(timer)
  }, [autoRefresh, load])

  const recent = stats?.recent ?? []

  const models = useMemo(
    () => [...new Set(recent.map((r) => r.model).filter(Boolean))].sort(),
    [recent],
  )

  const rows = useMemo(() => {
    let list = recent.filter((r) => {
      if (onlyFailed && r.status >= 200 && r.status < 300) return false
      if (typeFilter !== 'all' && (typeFilter === 'stream' ? !r.stream : r.stream)) return false
      if (modelFilter && r.model !== modelFilter) return false
      return true
    })
    // 非时间排序：按所选维度降序（排查"最慢的请求""扣费最高"用）。
    if (sortKey !== 'time') {
      const key = sortKey === 'wall' ? 'wall_ms' : sortKey === 'ttfb' ? 'ttfb_ms' : 'credit'
      list = [...list].sort((a, b) => (b[key] ?? 0) - (a[key] ?? 0))
    }
    return list
  }, [recent, onlyFailed, typeFilter, modelFilter, sortKey])

  // 筛选后小计：看"这波失败亏了多少 / 整体慢不慢"，不必心算。
  const summary = useMemo(() => {
    const n = rows.length
    const fails = rows.filter((r) => r.status < 200 || r.status >= 300).length
    const avgWall = n ? rows.reduce((s, r) => s + r.wall_ms, 0) / n : 0
    const credit = rows.reduce((s, r) => s + (r.credit ?? 0), 0)
    return { n, fails, avgWall, credit }
  }, [rows])

  // 分页：每页条数用户可配（默认 10；筛选/排序/条数变化时由调用方重置回第一页）。
  const pageCount = Math.max(1, Math.ceil(rows.length / pageSize))
  const pageSafe = Math.min(page, pageCount - 1)
  const paged = rows.slice(pageSafe * pageSize, pageSafe * pageSize + pageSize)
  const resetPage = () => setPage(0)
  const changePageSize = (n: number) => {
    setPageSize(n)
    localStorage.setItem('requests.pageSize', String(n))
    resetPage()
  }
  const hasFilter = onlyFailed || typeFilter !== 'all' || modelFilter !== ''
  // 行展开标识：time+model+status+uid 组合在 200 条内唯一。
  const toggleExpand = (r: RequestRecord) => {
    const k = `${r.time}|${r.model}|${r.status}|${r.uid ?? ''}`
    setExpanded((cur) => (cur === k ? null : k))
  }

  if (loading && !stats) return <Spinner label="正在加载请求明细…" />

  if (stats && !stats.enabled) {
    return (
      <>
        <div className="page-head">
          <div>
            <h1>请求明细</h1>
            <p>单条请求级记录（模型 / 账号 / 耗时 / 用量 / 扣费 / 错误）</p>
          </div>
        </div>
        <Alert kind="warn">
          网关未开启请求统计（<span className="mono">server.metrics_enabled=false</span>），
          无法记录请求明细。请在「网关配置」中开启后重启网关。
        </Alert>
      </>
    )
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>请求明细</h1>
          <p>单条请求级记录：模型 / 账号 / 耗时与首字 / 输入输出 / 缓存 / 扣费 / 错误</p>
        </div>
        <div className="page-actions">
          <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 13 }}>
            <input type="checkbox" checked={autoRefresh} onChange={(e) => setAutoRefresh(e.target.checked)} />
            自动刷新（5s）
          </label>
          <button className="btn" onClick={() => { void load(); void loadAccounts() }} disabled={loading}>
            {loading ? <Spinner /> : '🔄'} 刷新
          </button>
        </div>
      </div>

      {error && (
        <Alert kind="error" onClose={() => setError(null)}>
          {error}
        </Alert>
      )}

      <div className="card">
        <div className="card-head">
          <h2>请求明细</h2>
          <span className="hint">
            显示 {rows.length} / 共 {recent.length} 条（网关保留 200 条，落盘持久化，重启不丢）· 点击行展开详情
          </span>
        </div>

        <div style={{ display: 'flex', gap: 14, flexWrap: 'wrap', alignItems: 'center', marginBottom: 10 }}>
          <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 13 }}>
            <input type="checkbox" checked={onlyFailed} onChange={(e) => { setOnlyFailed(e.target.checked); resetPage() }} />
            只看失败（非 2xx）
          </label>
          <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 13 }}>
            类型
            <select value={typeFilter} onChange={(e) => { setTypeFilter(e.target.value as typeof typeFilter); resetPage() }}>
              <option value="all">全部</option>
              <option value="stream">流式</option>
              <option value="sync">同步</option>
            </select>
          </label>
          <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 13 }}>
            模型
            <select className="mono" value={modelFilter} onChange={(e) => { setModelFilter(e.target.value); resetPage() }} style={{ maxWidth: 260 }}>
              <option value="">全部</option>
              {models.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          </label>
          <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 13 }}>
            排序
            <select value={sortKey} onChange={(e) => { setSortKey(e.target.value as typeof sortKey); resetPage() }}>
              <option value="time">最新优先</option>
              <option value="wall">耗时最高</option>
              <option value="ttfb">首字最高</option>
              <option value="credit">扣费最高</option>
            </select>
          </label>
          <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: 13 }}>
            每页
            <select value={pageSize} onChange={(e) => changePageSize(Number(e.target.value))}>
              <option value={10}>10 条</option>
              <option value={20}>20 条</option>
              <option value={50}>50 条</option>
              <option value={100}>100 条</option>
            </select>
          </label>
          {hasFilter && (
            <button
              className="btn btn-sm"
              onClick={() => {
                setOnlyFailed(false)
                setTypeFilter('all')
                setModelFilter('')
                resetPage()
              }}
            >
              清除筛选
            </button>
          )}
        </div>

        {/* 筛选后小计：把"这波请求整体怎么样"从人眼扫表格变成一眼四个数。 */}
        {rows.length > 0 && (
          <div className="text-dim" style={{ display: 'flex', gap: 16, flexWrap: 'wrap', fontSize: 12, marginBottom: 10 }}>
            <span>筛选小计：{summary.n} 条</span>
            {summary.fails > 0 && <span className="text-danger">失败 {summary.fails}</span>}
            <span>平均耗时 {fmtMs(summary.avgWall)}</span>
            <span>总扣费 {fmtCredit(summary.credit)}</span>
          </div>
        )}

        {rows.length === 0 ? (
          <Empty>
            {recent.length === 0 ? '暂无请求明细。' : '当前筛选条件下没有匹配的请求。'}
            <div style={{ marginTop: 6, fontSize: 12 }}>
              {recent.length === 0 ? '向网关发一次请求（可用「聊天测试」页）后即可看到。' : ''}
            </div>
          </Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>时间</th>
                  <th>模型</th>
                  <th className="num">状态</th>
                  <th>账号</th>
                  <th>Key</th>
                  <th>协议</th>
                  <th>类型</th>
                  <th className="num">尝试</th>
                  <th className="num">耗时</th>
                  <th className="num">首字</th>
                  <th className="num">输入</th>
                  <th className="num">输出</th>
                  <th className="num">缓存命中</th>
                  <th className="num">扣费</th>
                  <th>错误</th>
                </tr>
              </thead>
              <tbody>
                {paged.map((r) => {
                  const exKey = `${r.time}|${r.model}|${r.status}|${r.uid ?? ''}`
                  const isOpen = expanded === exKey
                  return (
                    <Fragment key={exKey}>
                      <tr
                        key={exKey}
                        onClick={() => toggleExpand(r)}
                        style={{ cursor: 'pointer' }}
                        title="点击展开/收起详情"
                      >
                        <td className="mono text-faint" title={fmtFull(r.time)}>{fmtClock(r.time)}</td>
                        <td className="mono">
                      {r.model}
                      {r.resp_model && r.resp_model !== r.model && (
                        <span className="text-dim" style={{ fontSize: 11 }}> → {r.resp_model}</span>
                      )}
                    </td>
                        <td className="num">
                          <span className={statusTone(r.status)}>{r.status}</span>
                        </td>
                        <td className="text-dim">
                          {(() => {
                            const a = accounts[r.uid ?? '']
                            if (!a) return <span className="mono">{r.uid || '—'}</span>
                            return (
                              <>
                                <div>{displayName(a)}</div>
                                <div className="mono text-faint" style={{ fontSize: 11 }}>{r.uid}</div>
                              </>
                            )
                          })()}
                        </td>
                        <td className="text-dim" style={{ fontSize: 12 }}>{r.key_name || '—'}</td>
                        <td className="text-faint">{r.proto || '—'}</td>
                        <td className="text-faint">{r.stream ? '流式' : '同步'}</td>
                        <td className="num">
                          {r.tries && r.tries > 1 ? (
                            <span className="text-warn" title="轮转过账号（限流/失败换号）">{r.tries}</span>
                          ) : (
                            r.tries || '—'
                          )}
                        </td>
                        <td className="num">{fmtMs(r.wall_ms)}</td>
                        <td className="num">{r.ttfb_ms ? fmtMs(r.ttfb_ms) : '—'}</td>
                        <td className="num">{r.prompt_tokens ? fmtTok(r.prompt_tokens) : '—'}</td>
                        <td className="num">{r.completion_tokens ? fmtTok(r.completion_tokens) : '—'}</td>
                        <td className="num">
                          {r.cache_hit_tokens || r.cache_miss_tokens
                            ? `${fmtTok(r.cache_hit_tokens ?? 0)} / ${fmtTok(r.cache_miss_tokens ?? 0)}`
                            : '—'}
                        </td>
                        <td className="num">{r.credit ? fmtCredit(r.credit) : '—'}</td>
                        <td className="text-danger" title={r.err || undefined}>
                          {r.err || ''}
                        </td>
                      </tr>
                      {isOpen && (
                        <tr key={`${exKey}-detail`} onClick={(e) => e.stopPropagation()}>
                          <td colSpan={15} style={{ background: 'rgba(127,127,127,0.06)' }}>
                            <dl className="kv" style={{ margin: '2px 0' }}>
                              <dt>完整时间</dt>
                              <dd className="mono">{fmtFull(r.time)}</dd>
                              <dt>协议</dt>
                              <dd>{r.proto || '—'}</dd>
                              <dt>Key</dt>
                              <dd className="mono">{r.key_name || '—'}</dd>
                              <dt>尝试</dt>
                              <dd>
                                {r.tries ?? 1}
                                {(r.tries ?? 1) > 1 && (
                                  <span className="text-warn">（轮转换号 {(r.tries ?? 1) - 1} 次）</span>
                                )}
                              </dd>
                              <dt>缓存 token</dt>
                              <dd className="mono">
                                hit {fmtTok(r.cache_hit_tokens ?? 0)} / miss {fmtTok(r.cache_miss_tokens ?? 0)}
                              </dd>
                              <dt>账号</dt>
                              <dd className="mono">
                                {(() => {
                                  const a = accounts[r.uid ?? '']
                                  if (!a) return r.uid || '—'
                                  return `${displayName(a)}（${a.uid}）`
                                })()}
                              </dd>
                              <dt>失败原因</dt>
                              <dd className={r.err ? 'text-danger' : undefined}>{r.err || '—'}</dd>
                            </dl>
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}

        {/* 分页脚注：仅超过一页时出现。 */}
        {rows.length > pageSize && (
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginTop: 8 }}>
            <span className="text-faint" style={{ fontSize: 12 }}>
              第 {pageSafe + 1} / {pageCount} 页 · 共 {rows.length} 条
            </span>
            <div style={{ display: 'flex', gap: 8 }}>
              <button className="btn btn-sm" disabled={pageSafe === 0} onClick={() => setPage(pageSafe - 1)}>
                ← 上一页
              </button>
              <button className="btn btn-sm" disabled={pageSafe >= pageCount - 1} onClick={() => setPage(pageSafe + 1)}>
                下一页 →
              </button>
            </div>
          </div>
        )}
      </div>

      <div className="card">
        <div className="card-head">
          <h2>字段说明</h2>
        </div>
        <dl className="kv">
          <dt>首字</dt>
          <dd>请求发出到收到第一个 token 的时间（TTFB）。只对流式请求有意义，非流式显示为 —。</dd>
          <dt>账号</dt>
          <dd>本条请求实际使用的账号 UID（网关按 Key 关联 + 优先级分层调度选出）。</dd>
          <dt>Key</dt>
          <dd>该请求命中的业务 API Key 名——排查"为什么走了某个账号"时先看这一列（不同 Key 的关联与优先级不同）。</dd>
          <dt>尝试</dt>
          <dd>轮转尝试次数：1 = 一次选中即成功；大于 1 表示中途换过号（限流/失效/余额不足），换号原因见「错误」列。</dd>
          <dt>缓存命中</dt>
          <dd>命中 / 未命中的输入 token 数。命中部分计费远低于未命中，直接影响花费。</dd>
          <dt>扣费</dt>
          <dd>上游返回的 credit 累计（非估算），单位是账号积分（与「官方应付」的元不是同一量纲）。</dd>
          <dt>保留范围</dt>
          <dd>网关内存 + 落盘保留最近 200 条，重启不丢；更早的记录只保留在按模型聚合统计里。</dd>
        </dl>
      </div>
    </>
  )
}
