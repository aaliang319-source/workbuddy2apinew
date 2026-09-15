// Automation.tsx 自动化页：六类定时任务的状态总览、手动触发、排程编辑与运行历史。
//
// 数据源是网关 /admin/automation/*（面板代理为 /api/automation/*）：
//   - 状态卡片：启用开关 + 小时编辑（保存 = config.json 落盘 + 网关内存热生效，无需重启）
//   - 「立即执行」：网关侧异步跑，前端轮询 run 详情直至完成（脚本类展示输出尾部）
//   - 运行历史：最近 20 条（持久化在网关 data/automation.json，重启不丢）
//
// 注意：账号管理页的"批量签到/批量旅行"等按钮走的是面板遗留任务通道（直连上游），
// 与本页的网关级执行是两条路径，结果口径可能略有差异（后续统一）。
import { useCallback, useEffect, useRef, useState } from 'react'
import { api, ApiError } from '../api'
import type { AutomationKindStatus, AutomationRun, AutomationStatus, SessionInfo } from '../types'
import { Alert, Badge, ConfirmDialog, fmtISO, fmtNum, Modal, Spinner } from '../ui'

const KIND_LABELS: Record<string, string> = {
  checkin: '自动签到',
  travel: '猫猫旅行',
  activity: '活跃上报',
  keepalive: 'Token 保活',
  school: '开学季任务',
  cat: '夜猫子任务',
}

const KIND_DESC: Record<string, string> = {
  checkin: '全量签到 → 查余额 → 解冻冷却账号（仅 cn 账号参与）',
  travel: '领养 / 派出 / 领奖单趟巡检（仅 cn 账号参与）',
  activity: '每号 N 条对话活跃上报 + 连登回读自检（cn 与 global 都上报）',
  keepalive: '刷新全部账号 token；连续失效自动禁用',
  school: '开学季脚本：任务点亮 / 领奖 / 抽奖（有真实写副作用）',
  cat: '夜猫子脚本：23:00–08:00 窗口内补 1 次上报（窗口外自动跳过）',
}

// 脚本类任务有真实写副作用（领取/抽奖），手动执行前必须确认。
const SCRIPT_KINDS = new Set(['school', 'cat'])

type Draft = { enabled: boolean; hours: string }

export default function Automation({ session }: { session: SessionInfo }) {
  const [status, setStatus] = useState<AutomationStatus | null>(null)
  const [runs, setRuns] = useState<AutomationRun[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  // 排程草稿：kind → 启用开关 + 小时文本（逗号分隔）。保存时合并全部卡片一起提交。
  const [drafts, setDrafts] = useState<Record<string, Draft>>({})
  const [saving, setSaving] = useState(false)
  const [triggering, setTriggering] = useState<string | null>(null)
  const [confirmKind, setConfirmKind] = useState<string | null>(null)
  const [detail, setDetail] = useState<AutomationRun | null>(null)
  const pollRef = useRef<number | null>(null)

  const writable = !session.read_only

  const load = useCallback(async (silent = false) => {
    if (!silent) setLoading(true)
    try {
      const [st, rs] = await Promise.all([api.automationStatus(), api.automationRuns(20)])
      setStatus(st)
      setRuns(rs.runs)
      setDrafts((prev) => {
        const next: Record<string, Draft> = {}
        for (const k of st.kinds) {
          next[k.kind] = prev[k.kind] ?? { enabled: k.enabled, hours: (k.hours ?? []).join(', ') }
        }
        return next
      })
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载自动化状态失败')
    } finally {
      if (!silent) setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
    const t = setInterval(() => void load(true), 15_000)
    return () => clearInterval(t)
  }, [load])

  // 运行详情轮询：run 完成后刷新总览与历史并停止。
  useEffect(() => {
    if (!detail?.running) return
    pollRef.current = window.setTimeout(() => {
      void (async () => {
        try {
          const r = await api.automationRun(detail.id)
          setDetail(r)
          if (!r.running) await load(true)
        } catch {
          /* 轮询失败静默，下个周期重试 */
        }
      })()
    }, 1500)
    return () => {
      if (pollRef.current) window.clearTimeout(pollRef.current)
    }
  }, [detail, load])

  const setDraft = (kind: string, patch: Partial<Draft>) =>
    setDrafts((prev) => ({ ...prev, [kind]: { ...prev[kind], ...patch } }))

  /** 把全部卡片的草稿合并为 schedule 载荷（apply 是整段替换，必须带上所有类型）。 */
  const buildSchedule = (): Record<string, unknown> | string => {
    if (!status) return '状态未加载'
    const sched: Record<string, unknown> = {}
    for (const k of status.kinds) {
      const d = drafts[k.kind] ?? { enabled: k.enabled, hours: (k.hours ?? []).join(', ') }
      const hours = d.hours
        .split(/[,，\s]+/)
        .filter(Boolean)
        .map((n) => Number(n))
      if (hours.some((h) => !Number.isInteger(h) || h < 0 || h > 23)) {
        return `${KIND_LABELS[k.kind] ?? k.kind} 的小时必须是 0-23 的整数（逗号分隔）`
      }
      if (d.enabled && hours.length === 0) {
        return `${KIND_LABELS[k.kind] ?? k.kind} 已启用但未填任何小时点`
      }
      sched[`${k.kind}_hours`] = hours
      sched[`${k.kind}_enabled`] = d.enabled
    }
    return sched
  }

  const saveSchedule = async () => {
    const payload = buildSchedule()
    if (typeof payload === 'string') {
      setError(payload)
      return
    }
    setSaving(true)
    setNotice(null)
    try {
      const res = await api.automationApply(payload)
      setNotice('排程已保存并热生效（无需重启网关）')
      setError(null)
      // 热生效后的状态直接来自网关响应，避免等 15s 轮询。
      setStatus(res.status)
      setDrafts((prev) => {
        const next: Record<string, Draft> = {}
        for (const k of res.status.kinds) {
          next[k.kind] = prev[k.kind] ?? { enabled: k.enabled, hours: (k.hours ?? []).join(', ') }
        }
        return next
      })
      void load(true)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '保存排程失败')
    } finally {
      setSaving(false)
    }
  }

  const trigger = async (kind: string) => {
    setTriggering(kind)
    setNotice(null)
    try {
      const res = await api.automationTrigger(kind)
      setDetail(res.run)
      void load(true)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '触发失败')
    } finally {
      setTriggering(null)
    }
  }

  if (loading) return <Spinner label="加载自动化状态…" />

  return (
    <>
      {error && (
        <Alert kind="error" onClose={() => setError(null)}>
          {error}
        </Alert>
      )}
      {notice && (
        <Alert kind="ok" onClose={() => setNotice(null)}>
          {notice}
        </Alert>
      )}

      <div className="card">
        <div className="card-head">
          <h2>定时自动化</h2>
          <span className="hint">
            {status?.enabled ? '定时循环运行中' : '定时循环已关闭（手动触发仍可用）'} · 保存即热生效
          </span>
        </div>
        <p className="text-dim" style={{ marginTop: 0, fontSize: 13 }}>
          六类任务由网关定时执行，结果持久化在网关 <span className="mono">data/automation.json</span>（重启不丢）。
          保存排程 = 写入网关 config.json + 立即热生效，无需重启。手动触发与定时共用同一执行器（同类型互斥）。
        </p>
        <div className="row" style={{ flexWrap: 'wrap' }}>
          {(status?.kinds ?? []).map((k) => (
            <KindCard
              key={k.kind}
              st={k}
              draft={drafts[k.kind] ?? { enabled: k.enabled, hours: (k.hours ?? []).join(', ') }}
              onChange={(d) => setDraft(k.kind, d)}
              onRun={() => (SCRIPT_KINDS.has(k.kind) ? setConfirmKind(k.kind) : void trigger(k.kind))}
              triggering={triggering === k.kind}
              writable={writable}
              dirty={(drafts[k.kind]?.enabled ?? k.enabled) !== k.enabled || (drafts[k.kind]?.hours ?? '') !== (k.hours ?? []).join(', ')}
            />
          ))}
        </div>
        <div style={{ marginTop: 10, display: 'flex', alignItems: 'center', gap: 10 }}>
          <button className="btn btn-primary" onClick={() => void saveSchedule()} disabled={saving || !writable}>
            {saving ? '保存中…' : '保存全部排程'}
          </button>
          {!writable && <span className="text-dim">只读模式：排程与手动触发已禁用</span>}
        </div>
        <div className="desc" style={{ marginTop: 10 }}>
          账号管理页的「批量签到 / 批量旅行」按钮走的是面板遗留通道（直连上游），与本页的网关级执行是两条路径，后续会统一。
          活跃上报条数（<span className="mono">activity_report_count</span>）暂不支持热生效，改动需在「网关配置」页保存后重启网关。
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h2>运行历史</h2>
          <span className="hint">最近 20 条 · 持久化于网关</span>
        </div>
        {runs.length === 0 ? (
          <p className="text-dim">还没有运行记录——定时触发或点上面的「立即执行」后会出现。</p>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table>
              <thead>
                <tr>
                  <th>任务</th>
                  <th>触发</th>
                  <th>开始时间</th>
                  <th className="num">成功 / 失败 / 跳过</th>
                  <th>耗时</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {runs.map((r) => (
                  <tr key={r.id}>
                    <td>{KIND_LABELS[r.kind] ?? r.kind}</td>
                    <td>
                      <Badge cls={r.trigger === 'manual' ? 'badge-accent' : 'badge-dim'}>
                        {r.trigger === 'manual' ? '手动' : '定时'}
                      </Badge>
                    </td>
                    <td className="text-dim" style={{ fontSize: 12 }}>
                      {fmtISO(r.started_at)}
                    </td>
                    <td className="num">
                      <span className="text-ok">{fmtNum(r.ok)}</span> /{' '}
                      <span className={r.failed > 0 ? 'text-danger' : 'text-dim'}>{fmtNum(r.failed)}</span> /{' '}
                      <span className="text-dim">{fmtNum(r.skipped)}</span>
                    </td>
                    <td className="text-dim" style={{ fontSize: 12 }}>
                      {durationOf(r)}
                    </td>
                    <td>
                      <button className="btn btn-sm" onClick={() => setDetail(r)}>
                        详情
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {detail && (
        <Modal title={`运行详情 · ${KIND_LABELS[detail.kind] ?? detail.kind}`} onClose={() => setDetail(null)} wide>
          {detail.running && (
            <p className="text-dim">
              <Spinner label="执行中，1.5 秒后刷新…" />
            </p>
          )}
          <div className="kv" style={{ marginBottom: 10 }}>
            <dt>开始</dt>
            <dd>{fmtISO(detail.started_at)}</dd>
            <dt>结束</dt>
            <dd>{detail.running ? '—' : fmtISO(detail.finished_at)}</dd>
            <dt>结果</dt>
            <dd>
              成功 {detail.ok} · 失败 {detail.failed} · 跳过 {detail.skipped}
              {detail.exit_code !== null && detail.exit_code !== undefined ? ` · 退出码 ${detail.exit_code}` : ''}
            </dd>
          </div>
          {detail.error && <Alert kind="error">{detail.error}</Alert>}
          {(detail.items ?? []).length > 0 && (
            <div style={{ overflowX: 'auto', marginTop: 10 }}>
              <table>
                <thead>
                  <tr>
                    <th>账号</th>
                    <th>状态</th>
                    <th>说明</th>
                    <th className="num">收益</th>
                    <th className="num">余额</th>
                  </tr>
                </thead>
                <tbody>
                  {(detail.items ?? []).map((it, i) => (
                    <tr key={`${it.uid || 'script'}-${i}`}>
                      <td>{it.nickname || it.uid?.slice(0, 8) || '—'}</td>
                      <td>
                        <Badge cls={it.ok ? (it.status === 'ok' ? 'badge-ok' : 'badge-dim') : 'badge-danger'}>
                          {it.status}
                        </Badge>
                      </td>
                      <td className="text-dim" style={{ fontSize: 12, maxWidth: 360, wordBreak: 'break-all' }}>
                        {it.message || '—'}
                      </td>
                      <td className="num">{it.reward ? `+${it.reward}` : '—'}</td>
                      <td className="num">{it.credits ?? '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {detail.truncated && <p className="text-dim">条目超出上限，仅显示前 500 条。</p>}
            </div>
          )}
          {detail.output && (
            <>
              <p className="desc" style={{ marginBottom: 4 }}>
                脚本输出（尾部）：
              </p>
              <div className="muted-box mono" style={{ whiteSpace: 'pre-wrap', maxHeight: 240, overflowY: 'auto' }}>
                {detail.output}
              </div>
            </>
          )}
        </Modal>
      )}

      {confirmKind && (
        <ConfirmDialog
          title={`立即执行「${KIND_LABELS[confirmKind]}」`}
          confirmText="确认执行"
          busy={triggering === confirmKind}
          onCancel={() => setConfirmKind(null)}
          onConfirm={() => {
            const k = confirmKind
            setConfirmKind(null)
            void trigger(k)
          }}
          message={
            <p style={{ marginTop: 0 }}>
              {KIND_DESC[confirmKind]}该任务会<strong>真实写上游</strong>（领取 / 抽奖 / 领养等），确认执行？
            </p>
          }
        />
      )}
    </>
  )
}

// KindCard 单个任务的状态/编辑卡片。
function KindCard({
  st,
  draft,
  onChange,
  onRun,
  triggering,
  writable,
  dirty,
}: {
  st: AutomationKindStatus
  draft: Draft
  onChange: (d: Partial<Draft>) => void
  onRun: () => void
  triggering: boolean
  writable: boolean
  dirty: boolean
}) {
  return (
    <div className="card" style={{ flex: '1 1 300px', margin: 0 }}>
      <div className="card-head">
        <h2>{KIND_LABELS[st.kind] ?? st.kind}</h2>
        {st.running ? (
          <Badge cls="badge-accent">执行中</Badge>
        ) : st.enabled ? (
          <Badge cls="badge-ok">已启用</Badge>
        ) : (
          <Badge cls="badge-dim">已禁用</Badge>
        )}
      </div>
      <p className="text-dim" style={{ margin: '4px 0 8px', fontSize: 12 }}>
        {KIND_DESC[st.kind] ?? ''}
      </p>
      <div style={{ display: 'flex', gap: 10, alignItems: 'center', flexWrap: 'wrap' }}>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={draft.enabled}
            disabled={!writable}
            onChange={(e) => onChange({ enabled: e.target.checked })}
          />
          启用定时
        </label>
        <div style={{ flex: '1 1 120px' }}>
          <input
            type="text"
            value={draft.hours}
            disabled={!writable}
            onChange={(e) => onChange({ hours: e.target.value })}
            placeholder="9, 21"
          />
        </div>
      </div>
      <p className="desc" style={{ marginTop: 6 }}>
        下次执行：
        {st.enabled && st.next_fire ? (
          <span className="mono">{fmtISO(st.next_fire)}</span>
        ) : (
          <span className="text-dim">—（未启用）</span>
        )}
        {dirty && <span className="text-danger"> · 有未保存修改</span>}
        {st.last_run && (
          <>
            {' · 上次：'}
            <span className={st.last_run.failed > 0 ? 'text-danger' : 'text-ok'}>
              ok={st.last_run.ok} fail={st.last_run.failed} skip={st.last_run.skipped}
            </span>
            {' @ '}
            {fmtISO(st.last_run.started_at)}
          </>
        )}
      </p>
      <button className="btn btn-sm" onClick={onRun} disabled={!writable || triggering || st.running}>
        {triggering ? '触发中…' : st.running ? '执行中…' : '立即执行'}
      </button>
    </div>
  )
}

// durationOf 计算一次运行的耗时文本（运行中则显示"进行中"）。
function durationOf(r: AutomationRun): string {
  if (r.running) return '进行中'
  if (!r.finished_at) return '—'
  const start = Date.parse(r.started_at)
  const end = Date.parse(r.finished_at)
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return '—'
  const sec = Math.round((end - start) / 1000)
  if (sec < 60) return `${sec}s`
  return `${Math.floor(sec / 60)}m${sec % 60}s`
}
