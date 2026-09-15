// Keys.tsx API Key 管理页：多 Key CRUD、Key↔账号多对多关联编辑（优先级/启用）、
// 账号禁用状态联动、cc-switch 一键导入（claude + codex 双深链）。
// 关联语义：Key 未关联任何账号则不可用（网关返回 403 key_not_provisioned）。
// 调度语义：分层——先只在最高优先级层内挑号，该层全不可用才降层；层内按
// credits/闲置/成功率加权随机（与全池调度同口径）。
import { useCallback, useEffect, useMemo, useState } from 'react'
import { api, ApiError } from '../api'
import type { Account, ApiKeyEntry, KeyAssociation } from '../types'
import { buildCcSwitchLink, gatewayEndpoint, minimalCcSwitchFields } from '../ccswitch'
import { Alert, Badge, ConfirmDialog, fmtISO, Modal, Spinner } from '../ui'

/** KeysData Keys 页数据：Key 列表 + 账号列表（供关联选择器）。 */
interface KeysData {
  keys: ApiKeyEntry[]
  accounts: Account[]
}

export default function Keys({ writable }: { writable: boolean }) {
  const [data, setData] = useState<KeysData | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [showValue, setShowValue] = useState<Record<string, boolean>>({})
  const [editing, setEditing] = useState<ApiKeyEntry | null>(null)
  const [creating, setCreating] = useState(false)
  const [deleting, setDeleting] = useState<ApiKeyEntry | null>(null)
  const [regenning, setRegenning] = useState<ApiKeyEntry | null>(null)

  const load = useCallback(async (silent = false) => {
    if (!silent) setLoading(true)
    try {
      const [ks, accs] = await Promise.all([api.keys(), api.accounts()])
      setData({ keys: ks.keys ?? [], accounts: accs.accounts ?? [] })
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载 Key 列表失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const accountLabel = useMemo(() => {
    const m = new Map<string, string>()
    for (const a of data?.accounts ?? []) {
      m.set(a.uid, a.nickname ? `${a.nickname} (${a.uid.slice(0, 8)})` : a.uid)
    }
    return m
  }, [data])

  const doCreate = async (name: string) => {
    setCreating(false)
    try {
      const k = await api.createKey(name)
      setNotice(`Key「${k.name}」已创建。请立即编辑并关联账号——未关联账号的 Key 不可用。`)
      await load(true)
      setEditing(k)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '创建失败')
    }
  }

  const doDelete = async (k: ApiKeyEntry) => {
    setDeleting(null)
    try {
      await api.deleteKey(k.id)
      setNotice(`Key「${k.name}」已删除，使用该 Key 的客户端将立即 401。`)
      await load(true)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '删除失败')
    }
  }

  const doRegenerate = async (k: ApiKeyEntry) => {
    setRegenning(null)
    try {
      await api.regenerateKey(k.id)
      setNotice(`Key「${k.name}」的密钥已重置，旧密钥立即失效。`)
      await load(true)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '重置失败')
    }
  }

  const doToggleEnabled = async (k: ApiKeyEntry, enabled: boolean) => {
    try {
      await api.updateKey(k.id, { enabled })
      await load(true)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '更新失败')
    }
  }

  if (loading && !data) return <Spinner label="正在加载 API Key…" />

  return (
    <>
      <div className="page-head">
        <div>
          <h1>API Keys</h1>
          <p>多 Key 管理与账号关联（多对多 · 优先级分层调度 · 热生效无需重启）</p>
        </div>
        <div className="page-actions">
          <button className="btn" onClick={() => void load()} disabled={loading}>
            {loading ? <Spinner /> : '🔄'} 刷新
          </button>
          <button className="btn btn-primary" onClick={() => setCreating(true)} disabled={!writable}>
            ➕ 新建 Key
          </button>
        </div>
      </div>

      {notice && (
        <Alert kind="ok" onClose={() => setNotice(null)}>
          {notice}
        </Alert>
      )}
      {error && (
        <Alert kind="error" onClose={() => setError(null)}>
          {error}
        </Alert>
      )}
      {!writable && (
        <Alert kind="warn">面板当前为只读模式，Key 管理操作不可用。</Alert>
      )}

      <div className="card">
        <div className="card-head">
          <h2>Key 列表</h2>
          <span className="hint">{data?.keys.length ?? 0} 个 Key</span>
        </div>
        {(data?.keys.length ?? 0) === 0 ? (
          <div className="empty">还没有业务 Key。点击「新建 Key」创建，创建后需关联账号才能使用。</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>名称</th>
                  <th>密钥</th>
                  <th>状态</th>
                  <th>关联账号 / 优先级</th>
                  <th>创建时间</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {(data?.keys ?? []).map((k) => {
                  const assocs = k.associations ?? []
                  const enabledAssocs = assocs.filter((a) => a.enabled)
                  const tiers = [...new Set(enabledAssocs.map((a) => a.priority))].sort((a, b) => b - a)
                  return (
                    <tr key={k.id}>
                      <td>{k.name || <span className="text-faint">（未命名）</span>}</td>
                      <td>
                        <span className="mono" style={{ fontSize: 12 }}>
                          {showValue[k.id]
                            ? k.value
                            : k.value.slice(0, 10) + '••••••••' + k.value.slice(-4)}
                        </span>{' '}
                        <button
                          className="btn btn-sm"
                          onClick={() => setShowValue((p) => ({ ...p, [k.id]: !p[k.id] }))}
                        >
                          {showValue[k.id] ? '隐藏' : '显示'}
                        </button>{' '}
                        <button
                          className="btn btn-sm"
                          onClick={() => void navigator.clipboard.writeText(k.value)}
                          title="复制完整密钥"
                        >
                          复制
                        </button>
                      </td>
                      <td>
                        <button
                          className="btn btn-sm"
                          disabled={!writable}
                          onClick={() => void doToggleEnabled(k, !k.enabled)}
                          title={writable ? (k.enabled ? '点击禁用' : '点击启用') : '只读模式不可操作'}
                        >
                          {k.enabled ? <Badge cls="badge-ok">启用</Badge> : <Badge cls="badge-danger">禁用</Badge>}
                        </button>
                      </td>
                      <td>
                        {enabledAssocs.length === 0 ? (
                          k.wildcard ? (
                            <>
                              <Badge cls="badge-dim">通配（全池）</Badge>{' '}
                              <span className="text-dim" style={{ fontSize: 12 }}>
                                无优先级分层，与迁移前行为一致
                              </span>
                            </>
                          ) : (
                            <Badge cls="badge-warn">未关联（不可用）</Badge>
                          )
                        ) : (
                          <>
                            <Badge cls="badge-accent">{enabledAssocs.length} 账号</Badge>{' '}
                            <span className="text-dim" style={{ fontSize: 12 }}>
                              优先级层：{tiers.join(' > ')}
                            </span>
                          </>
                        )}
                      </td>
                      <td className="text-dim" style={{ fontSize: 12 }}>
                        {fmtISO(k.created_at)}
                      </td>
                      <td>
                        <CcSwitchButtons keyName={k.name} apiKey={k.value} />
                        <button className="btn btn-sm" disabled={!writable} onClick={() => setEditing(k)}>
                          编辑关联
                        </button>{' '}
                        <button className="btn btn-sm" disabled={!writable} onClick={() => setRegenning(k)}>
                          重置密钥
                        </button>{' '}
                        <button className="btn btn-sm btn-danger" disabled={!writable} onClick={() => setDeleting(k)}>
                          删除
                        </button>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
        <div className="desc" style={{ marginTop: 10 }}>
          调度规则：每个请求只在该 Key 关联（且启用）的账号内选号；<strong>优先级数字越大越优先</strong>，
          高优先级层内账号全部冷却/占满时才降级到下一层。账号被禁用（账号管理页）后对所有 Key 立即失效。
        </div>
      </div>

      {creating && <CreateKeyDialog onClose={() => setCreating(false)} onCreate={doCreate} />}
      {editing && (
        <EditKeyDialog
          keyEntry={editing}
          accounts={data?.accounts ?? []}
          accountLabel={accountLabel}
          onClose={() => setEditing(null)}
          onSaved={async (msg) => {
            setEditing(null)
            setNotice(msg)
            await load(true)
          }}
        />
      )}
      {deleting && (
        <ConfirmDialog
          title="删除 API Key"
          danger
          confirmText="确认删除"
          onCancel={() => setDeleting(null)}
          onConfirm={() => void doDelete(deleting)}
          message={
            <>
              <p style={{ marginTop: 0 }}>
                确定删除 Key「<strong>{deleting.name}</strong>」？
              </p>
              <p style={{ marginBottom: 0 }}>
                删除立即生效，使用该 Key 的客户端将无法再访问网关（401）。此操作不可撤销。
              </p>
            </>
          }
        />
      )}
      {regenning && (
        <ConfirmDialog
          title="重置 Key 密钥"
          danger
          confirmText="确认重置"
          onCancel={() => setRegenning(null)}
          onConfirm={() => void doRegenerate(regenning)}
          message={
            <>
              <p style={{ marginTop: 0 }}>
                确定重置 Key「<strong>{regenning.name}</strong>」的密钥？
              </p>
              <p style={{ marginBottom: 0 }}>旧密钥立即失效，需要把新密钥重新分发/导入到所有使用方。</p>
            </>
          }
        />
      )}
    </>
  )
}

/** CcSwitchButtons 按 Key 生成 cc-switch 导入链接（claude + codex），点击唤起本机 CC Switch。 */
function CcSwitchButtons({ keyName, apiKey }: { keyName: string; apiKey: string }) {
  const [endpoint, setEndpoint] = useState('')
  const [copied, setCopied] = useState<'claude' | 'codex' | null>(null)

  useEffect(() => {
    void (async () => {
      try {
        const res = await api.config()
        setEndpoint(gatewayEndpoint(res.config.listen))
      } catch {
        // 配置拉取失败：留空由用户在 System 页手动生成
      }
    })()
  }, [])

  if (!endpoint) return null
  const f = minimalCcSwitchFields(endpoint, apiKey)
  const claudeLink = buildCcSwitchLink('claude', f, keyName || 'WorkBuddy2API')
  const codexLink = buildCcSwitchLink('codex', f, keyName || 'WorkBuddy2API')

  const copy = async (app: 'claude' | 'codex', link: string) => {
    try {
      await navigator.clipboard.writeText(link)
      setCopied(app)
      setTimeout(() => setCopied(null), 2000)
    } catch {
      /* 剪贴板不可用：忽略 */
    }
  }

  return (
    <>
      <a className="btn btn-sm btn-primary" href={claudeLink} style={{ textDecoration: 'none' }} title="唤起本机 CC Switch 导入为 Claude Code 供应商">
        CC·Claude
      </a>{' '}
      <button className="btn btn-sm" onClick={() => void copy('claude', claudeLink)} title="复制 Claude 深链">
        {copied === 'claude' ? '✓' : '复制'}
      </button>{' '}
      <a className="btn btn-sm btn-primary" href={codexLink} style={{ textDecoration: 'none' }} title="唤起本机 CC Switch 导入为 Codex 供应商">
        CC·Codex
      </a>{' '}
      <button className="btn btn-sm" onClick={() => void copy('codex', codexLink)} title="复制 Codex 深链">
        {copied === 'codex' ? '✓' : '复制'}
      </button>{' '}
    </>
  )
}

/** CreateKeyDialog 新建 Key：只填名称，value 由网关生成。 */
function CreateKeyDialog({ onClose, onCreate }: { onClose: () => void; onCreate: (name: string) => Promise<void> }) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async () => {
    if (!name.trim()) {
      setError('请填写 Key 名称（如：团队 / 用途）')
      return
    }
    setBusy(true)
    setError(null)
    try {
      await onCreate(name.trim())
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      title="新建 API Key"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose} disabled={busy}>
            取消
          </button>
          <button className="btn btn-primary" onClick={() => void submit()} disabled={busy}>
            {busy ? <Spinner /> : null} 创建
          </button>
        </>
      }
    >
      {error && <Alert kind="error">{error}</Alert>}
      <div className="field">
        <label>Key 名称</label>
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="如：team-alpha / 个人笔记本" autoFocus />
        <div className="desc">密钥（sk-wb2-…）由网关自动生成。创建后请立即关联账号——未关联账号的 Key 不可用。</div>
      </div>
    </Modal>
  )
}

/** EditKeyDialog 编辑 Key 的关联账号：多选 + 每账号优先级/启用。保存为全量替换。 */
function EditKeyDialog({
  keyEntry,
  accounts,
  accountLabel,
  onClose,
  onSaved,
}: {
  keyEntry: ApiKeyEntry
  accounts: Account[]
  accountLabel: Map<string, string>
  onClose: () => void
  onSaved: (msg: string) => Promise<void>
}) {
  const [assocs, setAssocs] = useState<KeyAssociation[]>(
    (keyEntry.associations ?? []).map((a) => ({ ...a })),
  )
  const [name, setName] = useState(keyEntry.name)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const inGateway = useMemo(() => new Set(accounts.filter((a) => a.in_gateway).map((a) => a.uid)), [accounts])

  const isSelected = (uid: string) => assocs.some((a) => a.uid === uid)
  const getAssoc = (uid: string) => assocs.find((a) => a.uid === uid)

  const toggle = (uid: string) => {
    setAssocs((prev) =>
      prev.some((a) => a.uid === uid)
        ? prev.filter((a) => a.uid !== uid)
        : [...prev, { uid, priority: 10, enabled: true }],
    )
  }
  const patch = (uid: string, p: Partial<KeyAssociation>) => {
    setAssocs((prev) => prev.map((a) => (a.uid === uid ? { ...a, ...p } : a)))
  }

  const submit = async () => {
    if (!name.trim()) {
      setError('Key 名称不能为空')
      return
    }
    if (assocs.length === 0) {
      setError('至少关联一个账号——未关联账号的 Key 不可用（403）')
      return
    }
    setBusy(true)
    setError(null)
    try {
      await api.updateKey(keyEntry.id, { name: name.trim(), associations: assocs })
      await onSaved(`Key「${name.trim()}」已更新（热生效，无需重启网关）`)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      title={`编辑 Key「${keyEntry.name}」`}
      onClose={onClose}
      wide
      footer={
        <>
          <button className="btn" onClick={onClose} disabled={busy}>
            取消
          </button>
          <button className="btn btn-primary" onClick={() => void submit()} disabled={busy}>
            {busy ? <Spinner /> : null} 保存（{assocs.length} 个关联）
          </button>
        </>
      }
    >
      {error && <Alert kind="error">{error}</Alert>}
      <div className="field">
        <label>Key 名称</label>
        <input value={name} onChange={(e) => setName(e.target.value)} />
      </div>
      <div className="field">
        <label>关联账号（勾选即关联；优先级越大越优先，先耗尽最高层再降级）</label>
        {accounts.length === 0 ? (
          <div className="empty">网关账号池为空，请先在「账号管理」添加账号。</div>
        ) : (
          <div className="table-wrap" style={{ maxHeight: 320, overflowY: 'auto' }}>
            <table>
              <thead>
                <tr>
                  <th style={{ width: 36 }}></th>
                  <th>账号</th>
                  <th>域</th>
                  <th>池内状态</th>
                  <th style={{ width: 110 }}>优先级</th>
                  <th style={{ width: 70 }}>启用</th>
                </tr>
              </thead>
              <tbody>
                {accounts.map((a) => {
                  const sel = isSelected(a.uid)
                  const assoc = getAssoc(a.uid)
                  const isGlobal = a.domain.includes('workbuddy.ai')
                  return (
                    <tr key={a.uid} style={sel ? { background: 'var(--accent-weak, rgba(59,130,246,.08))' } : undefined}>
                      <td>
                        <input type="checkbox" checked={sel} onChange={() => toggle(a.uid)} />
                      </td>
                      <td>
                        <span className="mono" style={{ fontSize: 12 }}>
                          {accountLabel.get(a.uid) ?? a.uid}
                        </span>
                        {!inGateway.has(a.uid) && (
                          <>
                            {' '}
                            <Badge cls="badge-warn">不在池内</Badge>
                          </>
                        )}
                      </td>
                      <td>
                        {isGlobal ? (
                          <Badge cls="badge-dim" >国际 global</Badge>
                        ) : (
                          <span className="text-dim" style={{ fontSize: 12 }}>国内 cn</span>
                        )}
                      </td>
                      <td>
                        {a.disabled ? (
                          <Badge cls="badge-danger">已禁用</Badge>
                        ) : a.cooling ? (
                          <Badge cls="badge-warn">冷却中</Badge>
                        ) : (
                          <Badge cls="badge-ok">{a.status || '正常'}</Badge>
                        )}
                      </td>
                      <td>
                        {sel && assoc ? (
                          <input
                            type="number"
                            className="mono"
                            value={assoc.priority}
                            onChange={(e) => patch(a.uid, { priority: parseInt(e.target.value, 10) || 0 })}
                            style={{ width: 80 }}
                          />
                        ) : (
                          <span className="text-faint">—</span>
                        )}
                      </td>
                      <td>
                        {sel && assoc ? (
                          <input
                            type="checkbox"
                            checked={assoc.enabled}
                            onChange={(e) => patch(a.uid, { enabled: e.target.checked })}
                          />
                        ) : (
                          <span className="text-faint">—</span>
                        )}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
        <div className="desc" style={{ marginTop: 8 }}>
          提示：「不在池内」的账号（凭证已删除/未加载）关联了也不会参与调度；被禁用的账号对全部 Key 失效，
          可在「账号管理」页重新启用。
          <br />
          <strong>域是硬性隔离</strong>：国内模型（裸模型名 / <span className="mono">cn:</span> 前缀）只在
          「国内 cn」账号里选号，<strong>国际 global 账号不会参与</strong>；反之 <span className="mono">global:</span>
          {' '}前缀模型只走国际账号。所以给国际账号设再高的优先级，也不会影响国内模型的调度结果。
        </div>
      </div>
    </Modal>
  )
}
