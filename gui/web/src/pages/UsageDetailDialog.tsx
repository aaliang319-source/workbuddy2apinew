// UsageDetailDialog 单账号积分消耗详情弹窗：
//   ① 汇总块（区间消耗 / 请求数 / 相当于官方 API 花费）
//   ② 模型分解（横向条 + 百分比，积分降序）
//   ③ 逐日表格（近 30 天消耗 / 请求数，新→旧）
// 数据源：仪表盘已拉取的 stats.usage.accounts[i] + account_costs。
import type { AccountUsage, UsageRange, UsageDay } from '../types'
import { Badge, fmtNum, Modal } from '../ui'

export type { UsageRange }

/** 数值格式：小于 0.01 且非 0 显示 4 位小数；一般保留 2 位。 */
function fmtCredit(v: number): string {
  if (v === 0) return '0'
  if (Math.abs(v) < 0.01) return v.toFixed(4)
  if (Math.abs(v) >= 10000) return fmtNum(Math.round(v))
  return v.toFixed(2)
}

/** 按区间取该账号的消耗值。 */
function metricOf(u: AccountUsage, range: UsageRange): number {
  if (range === 'today') return u.today
  if (range === 'last7d') return u.last_7d
  return u.total
}

export default function UsageDetailDialog({
  account,
  range,
  officialCost,
  onClose,
}: {
  account: AccountUsage
  range: UsageRange
  /** 该账号按官方价折算的花费（元）；未配置价格表时 undefined */
  officialCost?: number
  onClose: () => void
}) {
  const value = metricOf(account, range)
  const models = account.by_model ?? []
  const maxCredit = Math.max(...models.map((m) => m.credit), 0)
  // 逐日表格：新→旧（daily 是旧→新，反转；去掉全零的尾部空白天）。
  const daily = [...(account.daily ?? [])].reverse()

  return (
    <Modal
      title={`消耗详情 · ${account.nickname || account.uid}`}
      onClose={onClose}
      wide
      footer={
        <button className="btn btn-primary" onClick={onClose}>
          关闭
        </button>
      }
    >
      <div className="grid grid-stats" style={{ marginBottom: 16 }}>
        <div className="stat">
          <div className="stat-label">区间消耗</div>
          <div className="stat-value">{fmtCredit(value)}</div>
          <div className="stat-sub">
            {range === 'today' ? '今天' : range === 'last7d' ? '近 7 天' : '累计（30 天）'}
          </div>
        </div>
        <div className="stat">
          <div className="stat-label">请求数</div>
          <div className="stat-value">{fmtNum(account.requests)}</div>
          <div className="stat-sub">保留期内</div>
        </div>
        <div className="stat">
          <div className="stat-label">相当于官方 API</div>
          <div className="stat-value">
            {officialCost === undefined ? <span className="text-faint">—</span> : `¥${officialCost.toFixed(2)}`}
          </div>
          <div className="stat-sub">{officialCost === undefined ? '未配置价格表' : '同量走官方价'}</div>
        </div>
      </div>

      <div className="card-head" style={{ marginBottom: 8 }}>
        <h2 style={{ fontSize: 15 }}>模型分解</h2>
        <span className="hint">{models.length} 个模型</span>
      </div>
      {models.length === 0 ? (
        <div className="empty">该账号暂无消耗记录。</div>
      ) : (
        <div style={{ marginBottom: 18 }}>
          {models.map((m) => (
            <div key={m.model} style={{ marginBottom: 10 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 3 }}>
                <span className="mono" style={{ fontSize: 12.5 }}>
                  {m.model}
                  {m.model === '(unknown)' && (
                    <>
                      {' '}
                      <Badge cls="badge-dim">旧数据</Badge>
                    </>
                  )}
                </span>
                <span className="mono" style={{ fontSize: 12.5 }}>
                  {fmtCredit(m.credit)}
                  <span className="text-faint" style={{ fontSize: 11, marginLeft: 6 }}>
                    {m.credit > 0 && maxCredit > 0 ? `${((m.credit / maxCredit) * 100).toFixed(0)}% · ` : ''}
                    {m.requests} 次 · {fmtNum(m.prompt_tokens + m.completion_tokens)} tok
                  </span>
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
                {m.credit > 0 && (
                  <div
                    style={{
                      width: `${maxCredit > 0 ? (m.credit / maxCredit) * 100 : 0}%`,
                      height: '100%',
                      borderRadius: 4,
                      background: 'var(--accent)',
                    }}
                  />
                )}
              </div>
            </div>
          ))}
        </div>
      )}

      <div className="card-head" style={{ marginBottom: 8 }}>
        <h2 style={{ fontSize: 15 }}>逐日明细</h2>
        <span className="hint">近 7 天</span>
      </div>
      <div className="table-wrap" style={{ maxHeight: 240, overflowY: 'auto' }}>
        <table>
          <thead>
            <tr>
              <th>日期</th>
              <th className="num">消耗</th>
              <th className="num">请求数</th>
            </tr>
          </thead>
          <tbody>
            {daily.map((d: UsageDay) => (
              <tr key={d.day}>
                <td className="mono" style={{ fontSize: 12 }}>
                  {d.day}
                </td>
                <td className="num mono">{fmtCredit(d.credit)}</td>
                <td className="num text-dim">{d.requests}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="desc" style={{ marginTop: 8 }}>
        逐日为面板卡片同口径的 7 天窗口；累计与模型分解为保留期（30 天）口径。
      </div>
    </Modal>
  )
}
