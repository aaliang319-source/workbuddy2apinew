// Dashboard.tsx 仪表盘：账号池健康度、积分总览、异常提示、积分消耗。
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, ApiError } from '../api'
import type { Account, AccountUsage, Overview, UsageRange, UsageSnapshot } from '../types'
import { Alert, Badge, displayName, fmtDuration, fmtISO, fmtNum, Spinner, statusBadge } from '../ui'
import UsageDetailDialog from './UsageDetailDialog'

export default function Dashboard() {
  const [ov, setOv] = useState<Overview | null>(null)
  const [accounts, setAccounts] = useState<Account[]>([])
  const [usage, setUsage] = useState<UsageSnapshot | null>(null)
  const [accountCosts, setAccountCosts] = useState<Record<string, number>>({})
  const [statsEnabled, setStatsEnabled] = useState(true)
  const [usageRange, setUsageRange] = useState<UsageRange>('today')
  const [detail, setDetail] = useState<AccountUsage | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [refreshedAt, setRefreshedAt] = useState<Date | null>(null)

  const load = useCallback(async (silent = false) => {
    if (!silent) setLoading(true)
    try {
      const [o, a, st] = await Promise.all([api.overview(), api.accounts(), api.stats().catch(() => null)])
      setOv(o)
      setAccounts(a.accounts ?? [])
      // 统计端点失败（网关旧版/统计关闭）不阻塞仪表盘其余部分。
      if (st) {
        setUsage(st.stats.usage ?? null)
        setAccountCosts((st as { account_costs?: Record<string, number> }).account_costs ?? {})
        setStatsEnabled(st.stats.enabled)
      }
      setError(null)
      setRefreshedAt(new Date())
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
    // 每 15 秒静默刷新，让冷却倒计时与在途数保持新鲜。
    const timer = setInterval(() => void load(true), 15_000)
    return () => clearInterval(timer)
  }, [load])

  if (loading && !ov) return <Spinner label="正在加载仪表盘…" />

  const problems = accounts.filter(
    (a) => a.status === 'disabled' || a.status === 'token_expired' || a.status === 'cooling',
  )
  const topAccounts = [...accounts]
    .sort((a, b) => (b.live_credits ?? b.credits) - (a.live_credits ?? a.credits))
    .slice(0, 6)

  return (
    <>
      <div className="page-head">
        <div>
          <h1>仪表盘</h1>
          <p>
            账号池实时健康度与积分总览
            {refreshedAt && <span className="text-faint"> · 更新于 {refreshedAt.toLocaleTimeString('zh-CN', { hour12: false })}</span>}
            <span className="text-faint"> · 每 15 秒自动刷新</span>
          </p>
        </div>
        <div className="page-actions">
          <button className="btn" onClick={() => void load()} disabled={loading}>
            {loading ? <Spinner /> : '🔄'} 立即刷新
          </button>
        </div>
      </div>

      {error && <Alert kind="error">{error}</Alert>}

      {ov && !ov.gateway_ok && (
        <Alert kind="error">
          <strong>无法连接网关（{ov.gateway_url}）</strong>
          <div style={{ marginTop: 4 }}>{ov.gateway_error}</div>
          <div style={{ marginTop: 6, fontSize: 12.5 }} className="text-dim">
            请确认 workbuddy2api 容器正在运行（<span className="mono">docker ps</span>），且本面板的
            <span className="mono"> gateway_url </span>配置正确。
          </div>
        </Alert>
      )}

      {ov?.warnings?.map((w) => (
        <Alert key={w} kind="warn">
          {w}
        </Alert>
      ))}

      {ov && (ov.expired > 0 || ov.expiring > 0) && (
        <Alert kind="warn">
          有 <strong>{ov.expired}</strong> 个账号 token 已过期、<strong>{ov.expiring}</strong> 个即将过期。
          <Link to="/accounts" style={{ marginLeft: 6 }}>
            去账号管理批量刷新 →
          </Link>
        </Alert>
      )}

      {ov && ov.file_issues && ov.file_issues.length > 0 && (
        <Alert kind="warn">
          <strong>凭证目录有问题文件：</strong>
          <ul style={{ margin: '5px 0 0', paddingLeft: 18 }}>
            {ov.file_issues.map((i) => (
              <li key={i} className="mono" style={{ fontSize: 12 }}>
                {i}
              </li>
            ))}
          </ul>
        </Alert>
      )}

      {ov && (
        <>
          <div className="grid grid-stats" style={{ marginBottom: 16 }}>
            <Stat
              label="网关账号池"
              value={`${ov.healthy}/${ov.total}`}
              sub={ov.total === 0 ? '尚未添加任何账号' : `可用 / 总数`}
              tone={ov.healthy === 0 ? 'danger' : ov.healthy < ov.total ? 'warn' : 'ok'}
            />
            <Stat label="冷却中" value={String(ov.cooling)} sub="限流 / 熔断冷却" tone={ov.cooling > 0 ? 'warn' : undefined} />
            <Stat label="已禁用" value={String(ov.disabled)} sub="需重新登录" tone={ov.disabled > 0 ? 'danger' : undefined} />
            <Stat
              label="剩余积分"
              value={fmtNum(ov.credits.remain)}
              sub={ov.credits.size > 0 ? `总量 ${fmtNum(ov.credits.size)}` : '未查询到配额'}
            />
            <Stat label="在途请求" value={String(ov.in_flight)} sub={`满载账号 ${ov.in_flight_full} 个`} />
            <Stat label="粘性会话" value={String(ov.sticky_sessions)} sub={`Redis：${ov.redis_mode}`} />
            <Stat
              label="凭证文件"
              value={String(ov.file_count)}
              sub={ov.expired > 0 ? `${ov.expired} 个已过期` : '全部有效'}
              tone={ov.expired > 0 ? 'warn' : undefined}
            />
          </div>

          <UsageCard
            usage={usage}
            statsEnabled={statsEnabled}
            range={usageRange}
            onRange={setUsageRange}
            onOpenDetail={setDetail}
          />

          <div className="grid grid-2">
            <div className="card">
              <div className="card-head">
                <h2>积分最高的账号</h2>
                <Link to="/accounts" className="hint">
                  查看全部 →
                </Link>
              </div>
              {topAccounts.length === 0 ? (
                <div className="empty">
                  还没有账号。
                  <div style={{ marginTop: 10 }}>
                    <Link className="btn btn-primary btn-sm" to="/login">
                      ➕ 添加第一个账号
                    </Link>
                  </div>
                </div>
              ) : (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>账号</th>
                        <th>状态</th>
                        <th className="num">积分</th>
                        <th className="num">成功</th>
                      </tr>
                    </thead>
                    <tbody>
                      {topAccounts.map((a) => {
                        const b = statusBadge(a.status)
                        return (
                          <tr key={a.uid}>
                            <td>
                              <div>{displayName(a)}</div>
                              <div className="mono text-faint" style={{ fontSize: 11 }}>
                                {a.uid.slice(0, 8)}
                              </div>
                            </td>
                            <td>
                              <Badge cls={b.cls}>{b.text}</Badge>
                            </td>
                            <td className="num">{fmtNum(a.live_credits ?? a.credits)}</td>
                            <td className="num text-dim">{fmtNum(a.success_count)}</td>
                          </tr>
                        )
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </div>

            <div className="card">
              <div className="card-head">
                <h2>需要处理的账号</h2>
                <span className="hint">{problems.length} 个</span>
              </div>
              {problems.length === 0 ? (
                <div className="empty">🎉 所有账号状态正常</div>
              ) : (
                <div className="table-wrap">
                  <table>
                    <thead>
                      <tr>
                        <th>账号</th>
                        <th>问题</th>
                        <th>剩余</th>
                      </tr>
                    </thead>
                    <tbody>
                      {problems.map((a) => {
                        const b = statusBadge(a.status)
                        return (
                          <tr key={a.uid}>
                            <td>
                              <div>{displayName(a)}</div>
                              <div className="mono text-faint" style={{ fontSize: 11 }}>
                                {a.uid.slice(0, 8)}
                              </div>
                            </td>
                            <td>
                              <Badge cls={b.cls}>{b.text}</Badge>
                              {a.reason && (
                                <div className="text-faint" style={{ fontSize: 11.5, marginTop: 3 }}>
                                  {a.reason}
                                </div>
                              )}
                              {a.breaker_fails > 0 && (
                                <div className="text-faint" style={{ fontSize: 11.5 }}>
                                  熔断计数 {a.breaker_fails}
                                </div>
                              )}
                            </td>
                            <td className="text-dim" style={{ fontSize: 12 }}>
                              {a.cool_remaining_sec ? fmtDuration(a.cool_remaining_sec) : fmtISO(a.last_err)}
                            </td>
                          </tr>
                        )
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </div>
          </div>

          <div className="card">
            <div className="card-head">
              <h2>运行信息</h2>
            </div>
            <dl className="kv">
              <dt>网关地址</dt>
              <dd className="mono">{ov.gateway_url}</dd>
              <dt>身份标识</dt>
              <dd>
                {ov.health?.service === 'workbuddy2api' ? (
                  <Badge cls="badge-ok">workbuddy2api 已确认</Badge>
                ) : (
                  <Badge cls="badge-warn">未确认</Badge>
                )}
              </dd>
              <dt>凭证目录计数</dt>
              <dd>{ov.file_count} 个文件</dd>
              <dt>服务端时间</dt>
              <dd>{new Date(ov.server_time).toLocaleString('zh-CN', { hour12: false })}</dd>
            </dl>
          </div>
        </>
      )}

      {detail && (
        <UsageDetailDialog
          account={detail}
          range={usageRange}
          officialCost={accountCosts[detail.uid]}
          onClose={() => setDetail(null)}
        />
      )}
    </>
  )
}

function Stat({
  label,
  value,
  sub,
  tone,
}: {
  label: string
  value: string
  sub?: string
  tone?: 'ok' | 'warn' | 'danger'
}) {
  const cls = tone === 'ok' ? 'text-ok' : tone === 'warn' ? 'text-warn' : tone === 'danger' ? 'text-danger' : ''
  return (
    <div className="stat">
      <div className="stat-label">{label}</div>
      <div className={`stat-value ${cls}`}>{value}</div>
      {sub && <div className="stat-sub">{sub}</div>}
    </div>
  )
}

/** 数值格式：小于 0.01 且非 0 显示科学计数以外的 4 位小数；一般保留 2 位。 */
function fmtCredit(v: number): string {
  if (v === 0) return '0'
  if (Math.abs(v) < 0.01) return v.toFixed(4)
  if (Math.abs(v) >= 10000) return fmtNum(Math.round(v))
  return v.toFixed(2)
}

/**
 * UsageCard 积分消耗卡片：30 天趋势图 + 汇总指标块 + 按账号横向比例条（可点开详情）。
 *
 * 设计取舍（dataviz 口径）：
 *  - 单一度量（积分），无多序列 → 不需要图例；数值直接标注，颜色不编码身份；
 *  - 趋势图一天一根柱（单色）、悬停显示当天数值；不画坐标轴，峰值由柱高承载；
 *  - 账号横条单色（--accent），条长 = 区间内占比（相对最大者），点击开详情弹窗。
 */
function UsageCard({
  usage,
  statsEnabled,
  range,
  onRange,
  onOpenDetail,
}: {
  usage: UsageSnapshot | null
  statsEnabled: boolean
  range: UsageRange
  onRange: (r: UsageRange) => void
  onOpenDetail: (u: AccountUsage) => void
}) {
  // 趋势图数据（无数据时给空数组，hooks 不能条件调用）。
  const trend = useMemo(() => usage?.daily ?? [], [usage])

  if (!statsEnabled) {
    return (
      <div className="card" style={{ marginBottom: 16 }}>
        <div className="card-head">
          <h2>积分消耗</h2>
        </div>
        <div className="empty">
          统计未开启（config <span className="mono">server.metrics_enabled</span>），开启后此处展示每账号每日消耗。
        </div>
      </div>
    )
  }
  if (!usage) return null

  const accounts = usage.accounts ?? []
  const keyOf: Record<UsageRange, 'today' | 'last7d' | 'total'> = { today: 'today', last7d: 'last7d', total: 'total' }
  const metricOf = (u: AccountUsage) =>
    keyOf[range] === 'today' ? u.today : keyOf[range] === 'last7d' ? u.last_7d : u.total

  const rows = accounts.map((u) => ({ u, v: metricOf(u) }))
  const max = Math.max(...rows.map((r) => r.v), 0)
  const trendMax = Math.max(...trend.map((d) => d.credit), 0)

  return (
    <div className="card" style={{ marginBottom: 16 }}>
      <div className="card-head">
        <h2>积分消耗</h2>
        <div style={{ display: 'flex', gap: 4 }}>
          {(
            [
              ['today', '今天'],
              ['last7d', '近 7 天'],
              ['total', '累计'],
            ] as [UsageRange, string][]
          ).map(([r, label]) => (
            <button
              key={r}
              className={`btn btn-sm${range === r ? ' btn-primary' : ''}`}
              onClick={() => onRange(r)}
            >
              {label}
            </button>
          ))}
        </div>
      </div>

      <div className="grid grid-stats" style={{ marginBottom: 14 }}>
        <Stat label="今日" value={fmtCredit(usage.today)} sub={usage.yesterday > 0 ? `昨日 ${fmtCredit(usage.yesterday)}` : undefined} />
        <Stat label="近 7 天" value={fmtCredit(usage.last_7d)} sub={`日均 ${fmtCredit(usage.last_7d / 7)}`} />
        <Stat label="累计" value={fmtCredit(usage.total)} sub={`保留 30 天口径`} />
      </div>

      {trendMax > 0 && (
        <div style={{ marginBottom: 16 }}>
          <div className="desc" style={{ marginBottom: 6 }}>
            近 30 天消耗趋势（悬停看当日数值）
          </div>
          <div style={{ display: 'flex', gap: 2, alignItems: 'flex-end', height: 48 }}>
            {trend.map((d) => {
              const h = d.credit > 0 ? Math.max(3, (d.credit / trendMax) * 44) : 2
              return (
                <div
                  key={d.day}
                  title={`${d.day}：${fmtCredit(d.credit)}（${d.requests} 次请求）`}
                  style={{
                    flex: 1,
                    height: h,
                    borderRadius: 2,
                    background: d.credit > 0 ? 'var(--accent)' : 'var(--bg-subtle, rgba(128,128,128,.2))',
                    opacity: d.credit > 0 ? 1 : 0.4,
                    minWidth: 2,
                  }}
                />
              )
            })}
          </div>
          <div style={{ display: 'flex', justifyContent: 'space-between', marginTop: 4 }}>
            <span className="text-faint" style={{ fontSize: 11 }}>{trend[0]?.day}</span>
            <span className="text-faint" style={{ fontSize: 11 }}>{trend[trend.length - 1]?.day}</span>
          </div>
        </div>
      )}

      {rows.length === 0 ? (
        <div className="empty">还没有消耗记录 —— 发起一次对话后这里会出现按账号的消耗分布。</div>
      ) : (
        <div>
          {rows.map(({ u, v }) => (
            <div key={u.uid} style={{ marginBottom: 10 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 3 }}>
                <span style={{ fontSize: 12.5 }}>
                  {u.nickname || <span className="text-faint">未知账号</span>}
                  <span className="mono text-faint" style={{ fontSize: 11, marginLeft: 6 }}>
                    {u.uid}
                  </span>
                </span>
                <span className="mono" style={{ fontSize: 12.5 }}>
                  {fmtCredit(v)}
                  <span className="text-faint" style={{ fontSize: 11, marginLeft: 6 }}>
                    {u.requests} 次
                  </span>
                  <button
                    className="btn btn-sm"
                    style={{ marginLeft: 8, padding: '1px 6px', fontSize: 11 }}
                    onClick={() => onOpenDetail(u)}
                  >
                    详情
                  </button>
                </span>
              </div>
              <div
                style={{
                  height: 8,
                  borderRadius: 4,
                  background: 'var(--bg-subtle, rgba(128,128,128,.15))',
                  overflow: 'hidden',
                }}
              >
                {v > 0 && (
                  <div
                    style={{
                      width: `${max > 0 ? (v / max) * 100 : 0}%`,
                      height: '100%',
                      borderRadius: 4,
                      background: 'var(--accent)',
                    }}
                  />
                )}
              </div>
            </div>
          ))}
          <div className="desc" style={{ marginTop: 8 }}>
            排名按当前区间消耗降序；条长为区间内占比（相对最大者）；「详情」看逐日与模型分解。
          </div>
        </div>
      )}
    </div>
  )
}
