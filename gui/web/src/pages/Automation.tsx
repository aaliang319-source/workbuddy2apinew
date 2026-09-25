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
import type {
  AutomationItem,
  AutomationKindStatus,
  AutomationRun,
  AutomationStatus,
  SessionInfo,
} from '../types'
import { Alert, Badge, ConfirmDialog, fmtISO, fmtNum, Modal, Spinner } from '../ui'

// 逐账号状态口径（网关 automation.Item.Status）。unreached 是"脚本没返回该账号任何
// 结果"，与 skipped（明确跳过且有原因）分开显示——这正是"哪个没执行"的答案。
const STATUS_LABELS: Record<string, string> = {
  ok: '成功',
  already: '已领',
  fail: '失败',
  skipped: '跳过',
  unreached: '未执行',
}

const STATUS_BADGES: Record<string, string> = {
  ok: 'badge-ok',
  already: 'badge-dim',
  skipped: 'badge-dim',
  unreached: 'badge-warn',
  fail: 'badge-danger',
}

// 详情统计与过滤的展示顺序：异常在前，一眼看出这次哪里不对。
const STATUS_ORDER = ['fail', 'unreached', 'skipped', 'already', 'ok']

/** 逐账号状态计数（详情顶部一行统计，点一下即过滤）。 */
function countStatuses(items: AutomationItem[]): { status: string; n: number }[] {
  const m = new Map<string, number>()
  for (const it of items) m.set(it.status, (m.get(it.status) ?? 0) + 1)
  const known = STATUS_ORDER.filter((s) => m.has(s)).map((s) => ({ status: s, n: m.get(s) ?? 0 }))
  const unknown = [...m.keys()]
    .filter((s) => !STATUS_ORDER.includes(s))
    .map((s) => ({ status: s, n: m.get(s) ?? 0 }))
  return [...known, ...unknown]
}

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

// hoursHint 小时输入框的说明。抖动是**每次排定现掷**的随机偏移，所以写的是
// "窗口内随机取一刻"而不是某个固定区间——用户配的是小时，不是分钟。
const hoursHint =
  '小时窗口（0-23，逗号分隔）。实际执行时刻 = 所配小时 + 每次排定时现掷的随机偏移（0-55 分钟内），' +
  '整点永远不会被选中，同一天的落点也不固定。'

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
  // 详情里的逐账号过滤（null = 全部）；点开新记录时清空，避免沿用上一条的过滤条件。
  const [statusFilter, setStatusFilter] = useState<string | null>(null)
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
      setStatusFilter(null)
      setDetail(res.run)
      void load(true)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '触发失败')
    } finally {
      setTriggering(null)
    }
  }

  if (loading) return <Spinner label="加载自动化状态…" />

  // 详情页的逐账号视图：状态统计 + 过滤（脚本类任务一次十几条，不筛很难看出"谁没跑"）。
  const detailItems = detail?.items ?? []
  const statusCounts = countStatuses(detailItems)
  const shownItems = statusFilter ? detailItems.filter((it) => it.status === statusFilter) : detailItems

  return (
    <>
      <div className="page-head">
        <div className="page-head-left">
          <h1>定时自动化</h1>
          <p>
            六类任务由网关定时执行，结果持久化在网关 data/automation.json（重启不丢）
            {status?.enabled ? ' · 定时循环运行中' : ' · 定时循环已关闭（手动触发仍可用）'}
          </p>
        </div>
      </div>

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
          <h2>排程</h2>
          <span className="hint">保存排程 = 写入 config.json + 立即热生效</span>
        </div>
        <p className="text-dim" style={{ marginTop: 0, fontSize: 12.5 }}>
          手动触发与定时共用同一执行器（同类型互斥）。
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
                  <th className="num">账号</th>
                  <th>成功 / 失败 / 跳过（含原因摘要）</th>
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
                    <td className="num text-dim">{r.accounts ? fmtNum(r.accounts) : '—'}</td>
                    <td>
                      <div>
                        <span className="text-ok">{fmtNum(r.ok)}</span> /{' '}
                        <span className={r.failed > 0 ? 'text-danger' : 'text-dim'}>{fmtNum(r.failed)}</span> /{' '}
                        <span className="text-dim">{fmtNum(r.skipped)}</span>
                      </div>
                      {(r.reason || r.parse_warning) && (
                        <div
                          className="text-dim"
                          style={{
                            fontSize: 12,
                            maxWidth: 340,
                            overflow: 'hidden',
                            textOverflow: 'ellipsis',
                            whiteSpace: 'nowrap',
                          }}
                          // 列表只给一行摘要，完整明细在详情里（鼠标悬停可看全文）
                          title={[r.reason, r.parse_warning].filter(Boolean).join('；')}
                        >
                          {r.parse_warning ? '⚠ ' : ''}
                          {r.reason || r.parse_warning}
                        </div>
                      )}
                    </td>
                    <td className="text-dim" style={{ fontSize: 12 }}>
                      {durationOf(r)}
                    </td>
                    <td>
                      <button
                        className="btn btn-sm"
                        onClick={() => {
                          setStatusFilter(null)
                          setDetail(r)
                        }}
                      >
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
              {detail.accounts ? ` · 账号 ${detail.accounts}` : ''}
              {detail.exit_code !== null && detail.exit_code !== undefined ? ` · 退出码 ${detail.exit_code}` : ''}
            </dd>
          </div>
          {detail.error && <Alert kind="error">{detail.error}</Alert>}
          {detail.parse_warning && (
            <Alert kind="warn">
              逐账号明细可能不全：{detail.parse_warning}
            </Alert>
          )}
          {detailItems.length > 0 && (
            <>
              {/* 统计即过滤器：详情里十几个账号，直接点"失败 2"看那两行。 */}
              <div className="row" style={{ gap: 8, flexWrap: 'wrap', alignItems: 'center', marginTop: 10 }}>
                <button
                  className={`btn btn-sm ${statusFilter === null ? 'btn-primary' : ''}`}
                  onClick={() => setStatusFilter(null)}
                >
                  全部 {detailItems.length}
                </button>
                {statusCounts.map((c) => (
                  <button
                    key={c.status}
                    className={`btn btn-sm ${statusFilter === c.status ? 'btn-primary' : ''}`}
                    onClick={() => setStatusFilter(statusFilter === c.status ? null : c.status)}
                  >
                    {STATUS_LABELS[c.status] ?? c.status} {c.n}
                  </button>
                ))}
                {statusFilter === 'unreached' && (
                  <span className="text-dim" style={{ fontSize: 12 }}>
                    未执行 = 脚本没返回该账号任何结果（多为输出显示上限截断，或执行中途中断）
                  </span>
                )}
              </div>
              <div style={{ overflowX: 'auto', marginTop: 8 }}>
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
                    {shownItems.map((it, i) => (
                      <tr key={`${it.uid || 'script'}-${i}`}>
                        <td>{it.nickname || it.uid?.slice(0, 8) || '—'}</td>
                        <td>
                          <Badge cls={STATUS_BADGES[it.status] ?? 'badge-dim'}>
                            {STATUS_LABELS[it.status] ?? it.status}
                          </Badge>
                        </td>
                        <td className="text-dim" style={{ fontSize: 12, maxWidth: 380, wordBreak: 'break-all' }}>
                          {it.message || '—'}
                        </td>
                        <td className="num">{it.reward ? `+${it.reward}` : '—'}</td>
                        <td className="num">{it.credits ?? '—'}</td>
                      </tr>
                    ))}
                    {shownItems.length === 0 && (
                      <tr>
                        <td colSpan={5} className="text-dim">
                          该状态下没有条目
                        </td>
                      </tr>
                    )}
                  </tbody>
                </table>
                {detail.truncated && (
                  <p className="text-dim">条目超出上限，仅显示前 500 条（上方计数为完整口径）。</p>
                )}
              </div>
            </>
          )}
          {(detail.reason || detail.parse_warning) && (
            <p className="desc" style={{ marginTop: 10 }}>
              结论：{detail.reason || '—'}
              {detail.parse_warning ? `（${detail.parse_warning}）` : ''}
            </p>
          )}
          {detail.output && (
            <details style={{ marginTop: 12 }}>
              <summary className="desc" style={{ cursor: 'pointer' }}>
                脚本原始输出（尾部，展开查看）
              </summary>
              <div
                className="muted-box mono"
                style={{ whiteSpace: 'pre-wrap', maxHeight: 260, overflowY: 'auto', marginTop: 6 }}
              >
                {detail.output}
              </div>
            </details>
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
            title={hoursHint}
          />
        </div>
      </div>
      <p className="desc" style={{ marginTop: 6 }}>
        下次执行：
        {!st.enabled ? (
          <span className="text-dim">—（未启用）</span>
        ) : st.next_fire ? (
          <span className="mono">{fmtISO(st.next_fire)}</span>
        ) : (
          // 循环还没排定过：不编造时刻，也不在前端重算（重算就是另掷一次随机数）。
          <span className="text-dim">待排定</span>
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
