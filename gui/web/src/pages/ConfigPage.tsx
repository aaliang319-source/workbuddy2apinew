// ConfigPage.tsx 网关 config.json 在线编辑：表单模式 + JSON 源码模式。
//
// 关键设计：表单读写的是**完整原始文档对象**（只改动已知字段），
// 因此配置文件里 GUi 不认识的自定义字段会被原样保留，不会因为一次保存被抹掉。
import { useCallback, useEffect, useState } from 'react'
import { api, ApiError } from '../api'
import type { ConfigMeta, SessionInfo } from '../types'
import { Alert, ConfirmDialog, fmtISO, Spinner } from '../ui'

type Doc = Record<string, unknown>

/** 按路径读写嵌套字段。 */
function getPath(doc: Doc, path: string): unknown {
  return path.split('.').reduce<unknown>((acc, key) => {
    if (acc && typeof acc === 'object') return (acc as Doc)[key]
    return undefined
  }, doc)
}

/** 设置嵌套字段（不可变更新，保证 React 看到新对象）。 */
function setPath(doc: Doc, path: string, value: unknown): Doc {
  const keys = path.split('.')
  const clone: Doc = { ...doc }
  let cur: Doc = clone
  for (let i = 0; i < keys.length - 1; i++) {
    const k = keys[i]
    const next = cur[k]
    cur[k] = next && typeof next === 'object' ? { ...(next as Doc) } : {}
    cur = cur[k] as Doc
  }
  cur[keys[keys.length - 1]] = value
  return clone
}

/** 数字字段：空串表示"未设置"，返回 undefined 以便从配置中删除。 */
function numOrUndefined(v: string): number | undefined {
  if (v.trim() === '') return undefined
  const n = Number(v)
  return Number.isFinite(n) ? n : undefined
}

/** 数字字段回显：undefined → 空串。 */
function numText(v: unknown): string {
  if (v === undefined || v === null) return ''
  return String(v)
}

/** 小时数组的文本互转：逗号/空格分隔。 */
function hoursToText(v: unknown): string {
  if (Array.isArray(v)) return v.join(', ')
  return ''
}

function textToHours(text: string): number[] | undefined {
  const parts = text
    .split(/[,，\s]+/)
    .map((s) => s.trim())
    .filter(Boolean)
  if (parts.length === 0) return undefined
  const nums = parts.map((s) => parseInt(s, 10))
  if (nums.some((n) => Number.isNaN(n))) return undefined
  return nums
}

export default function ConfigPage({ session }: { session: SessionInfo }) {
  const [doc, setDoc] = useState<Doc | null>(null)
  const [meta, setMeta] = useState<ConfigMeta | null>(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [tab, setTab] = useState<'form' | 'json'>('form')
  const [jsonText, setJsonText] = useState('')
  const [jsonError, setJsonError] = useState<string | null>(null)
  const [showApiKey, setShowApiKey] = useState(false)
  const [showSmtpPass, setShowSmtpPass] = useState(false)
  const [confirmReset, setConfirmReset] = useState(false)
  const [resetting, setResetting] = useState(false)
  // 通知测试发送（同步调网关 /admin/notify/test → 面板代理）。
  const [testingNotify, setTestingNotify] = useState(false)
  const [notifyTestMsg, setNotifyTestMsg] = useState<string | null>(null)
  const [notifyTestOK, setNotifyTestOK] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await api.config()
      setDoc(res.config)
      setMeta(res.meta)
      setJsonText(JSON.stringify(res.config, null, 2))
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载配置失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  /** 表单字段更新：同时同步 JSON 文本，保证两个视图一致。 */
  const update = (path: string, value: unknown) => {
    setDoc((prev) => {
      if (!prev) return prev
      const next = setPath(prev, path, value)
      setJsonText(JSON.stringify(next, null, 2))
      return next
    })
  }

  /** sendNotifyTest 让网关用当前已加载的配置发一封测试邮件（同步结果）。 */
  const sendNotifyTest = async () => {
    setTestingNotify(true)
    setNotifyTestMsg(null)
    try {
      const res = await api.notifyTest()
      setNotifyTestOK(true)
      setNotifyTestMsg(res.message || '测试邮件已发送')
    } catch (err) {
      setNotifyTestOK(false)
      setNotifyTestMsg(err instanceof ApiError ? err.message : '发送失败')
    } finally {
      setTestingNotify(false)
    }
  }

  const saveForm = async () => {
    if (!doc) return
    setSaving(true)
    setNotice(null)
    try {
      const res = await api.saveConfig(doc)
      // schedule 段支持热生效：文件已落盘，再让网关内存排程立即生效（免重启）。
      // 失败不阻断——文件已保存，重启网关同样生效。
      let applyNote = ''
      if (doc.schedule && typeof doc.schedule === 'object') {
        try {
          await api.automationApply(doc.schedule as Record<string, unknown>)
          applyNote = '；定时排程已热生效（无需重启）'
        } catch {
          applyNote = '；定时排程热生效失败，重启网关后生效'
        }
      }
      setNotice(res.message + applyNote)
      setError(null)
      // 保存后重新读取：首次保存会生成备份文件，meta 里的 backup_path 随之出现
      //（「恢复备份」按钮依赖它）；顺带同步文件修改时间。
      await reloadMeta()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  /** reloadMeta 只刷新元信息与已保存内容，用于保存成功后同步状态。 */
  const reloadMeta = async () => {
    try {
      const res = await api.config()
      setMeta(res.meta)
      // 用服务端返回的规范化内容覆盖本地（例如 JSON 缩进、字段顺序）。
      setDoc(res.config)
      setJsonText(JSON.stringify(res.config, null, 2))
    } catch {
      // 刷新失败不影响"已保存"这一事实，保留当前视图。
    }
  }

  const saveJSON = async () => {
    setJsonError(null)
    let parsed: Doc
    try {
      parsed = JSON.parse(jsonText) as Doc
    } catch (err) {
      setJsonError(err instanceof Error ? err.message : 'JSON 语法错误')
      return
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      setJsonError('配置顶层必须是一个 JSON 对象')
      return
    }
    setSaving(true)
    setNotice(null)
    try {
      const res = await api.saveConfig(parsed)
      setNotice(res.message)
      setError(null)
      await reloadMeta()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  const doReset = async () => {
    setResetting(true)
    try {
      const res = await api.resetConfig()
      setConfirmReset(false)
      setNotice(res.message)
      await load()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '恢复失败')
    } finally {
      setResetting(false)
    }
  }

  if (loading && !doc) return <Spinner label="正在读取网关配置…" />

  const writeDisabled = session.read_only
  const str = (path: string) => {
    const v = getPath(doc ?? {}, path)
    return v === undefined || v === null ? '' : String(v)
  }
  const bool = (path: string) => getPath(doc ?? {}, path) === true
  const numStr = (path: string) => {
    const v = getPath(doc ?? {}, path)
    return v === undefined || v === null ? '' : String(v)
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>网关配置</h1>
          <p>
            在线编辑 <span className="mono">{meta?.path || 'config.json'}</span>
            {meta?.mod_time && <span className="text-faint"> · 最后修改 {fmtISO(meta.mod_time)}</span>}
          </p>
        </div>
        <div className="page-actions">
          <button className="btn" onClick={() => void load()} disabled={loading}>
            {loading ? <Spinner /> : '🔄'} 重新读取
          </button>
          {meta?.backup_path && (
            <button
              className="btn btn-danger"
              onClick={() => setConfirmReset(true)}
              disabled={writeDisabled || !session.dangerous_ops}
              title={!session.dangerous_ops ? '需在服务端开启 dangerous_ops' : '从首次备份恢复'}
            >
              ↩️ 恢复备份
            </button>
          )}
        </div>
      </div>

      {writeDisabled && <Alert kind="warn">服务端已开启只读模式，配置无法保存。</Alert>}
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
      {meta && !meta.exists && (
        <Alert kind="warn">
          配置文件尚不存在（{meta.path}）。保存后会以当前内容创建；网关首次启动时用默认值。
        </Alert>
      )}

      <Alert kind="info">
        <strong>修改后需要重启网关才生效。</strong> 保存只会写文件，不会自动重启。
        {session.dangerous_ops ? (
          <>
            {' '}可在「系统」页一键重启容器。
          </>
        ) : (
          <>
            {' '}（自动重启能力需在服务端开启 <span className="mono">dangerous_ops</span>）
          </>
        )}
        {meta?.backup_path ? (
          <div style={{ marginTop: 5, fontSize: 12.5 }}>
            首次保存前已自动备份原始文件到 <span className="mono">{meta.backup_path}</span>
            {meta.backup_at ? `（${fmtISO(meta.backup_at)}）` : ''}。随时可用右上角「恢复备份」回滚。
          </div>
        ) : (
          <div style={{ marginTop: 5, fontSize: 12.5 }}>
            尚未生成备份：首次保存时会自动把当前的 config.json 备份一份，之后可随时回滚。
          </div>
        )}
      </Alert>

      <div className="tabs">
        <button className={`tab ${tab === 'form' ? 'active' : ''}`} onClick={() => setTab('form')}>
          表单编辑
        </button>
        <button className={`tab ${tab === 'json' ? 'active' : ''}`} onClick={() => setTab('json')}>
          JSON 源码
        </button>
      </div>

      {tab === 'form' && doc && (
        <>
          <div className="card">
            <div className="card-head">
              <h2>基础设置</h2>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>监听地址 listen</label>
                <input type="text" value={str('listen')} onChange={(e) => update('listen', e.target.value)} placeholder=":7863" />
                <div className="desc">网关 HTTP 监听地址，冒号开头表示所有网卡。</div>
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>API 密钥 api_key</label>
                <div style={{ display: 'flex', gap: 6 }}>
                  <input
                    type={showApiKey ? 'text' : 'password'}
                    value={str('api_key')}
                    onChange={(e) => update('api_key', e.target.value)}
                    placeholder="留空 = 不鉴权（公网务必设置）"
                  />
                  <button className="btn shrink" onClick={() => setShowApiKey((v) => !v)} type="button">
                    {showApiKey ? '隐藏' : '显示'}
                  </button>
                </div>
                <div className="desc">
                  客户端调用 <span className="mono">/v1/chat/completions</span> 时需带
                  <span className="mono"> Authorization: Bearer &lt;api_key&gt;</span>。留空则任何人可用。
                </div>
              </div>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>凭证目录 auth_dir</label>
                <input type="text" value={str('auth_dir')} onChange={(e) => update('auth_dir', e.target.value)} placeholder="./auths" />
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>状态文件 state_file</label>
                <input type="text" value={str('state_file')} onChange={(e) => update('state_file', e.target.value)} placeholder="./data/state.json" />
              </div>
            </div>
          </div>

          <div className="card">
            <div className="card-head">
              <h2>定时任务</h2>
              <span className="hint">时间按容器时区（compose 默认 Asia/Shanghai）· 保存后可在「自动化」页热生效</span>
            </div>
            <div className="row" style={{ flexWrap: 'wrap' }}>
              {(
                [
                  ['checkin_hours', '签到小时 checkin_hours', '9, 21', '全量签到 → 查余额 → 解冻冷却账号'],
                  ['travel_hours', '旅行小时 travel_hours', '9, 21', '猫猫旅行单趟巡检：领养/派出/领奖（独立排程，不搭签到便车）'],
                  ['activity_hours', '活跃小时 activity_hours', '10', '每号 N 条对话活跃上报 + 连登自检'],
                  ['keepalive_hours', '保活小时 keepalive_hours', '22', '刷新全部账号 token，连续失效自动禁用'],
                  ['school_hours', '开学季 school_hours', '12', '开学季脚本（有真实领取/抽奖副作用）'],
                  ['cat_hours', '夜猫子 cat_hours', '1', '夜猫窗口 23:00-08:00 内补 1 次上报'],
                ] as const
              ).map(([key, label, ph, desc]) => (
                <div className="field" style={{ flex: '1 1 240px' }} key={key}>
                  <label>{label}</label>
                  <input
                    type="text"
                    value={hoursToText(getPath(doc, `schedule.${key}`))}
                    onChange={(e) => update(`schedule.${key}`, textToHours(e.target.value))}
                    placeholder={ph}
                  />
                  <div className="desc">{desc}</div>
                </div>
              ))}
            </div>
            <div className="row" style={{ flexWrap: 'wrap' }}>
              {(
                [
                  ['checkin_enabled', '启用签到'],
                  ['travel_enabled', '启用猫猫旅行'],
                  ['activity_enabled', '启用活跃上报'],
                  ['keepalive_enabled', '启用 Token 保活'],
                  ['school_enabled', '启用开学季任务'],
                  ['cat_enabled', '启用夜猫子任务'],
                ] as const
              ).map(([key, label]) => (
                <div className="field" style={{ flex: '1 1 150px' }} key={key}>
                  <label className="checkbox">
                    <input
                      type="checkbox"
                      checked={bool(`schedule.${key}`)}
                      onChange={(e) => update(`schedule.${key}`, e.target.checked)}
                    />
                    {label}
                  </label>
                </div>
              ))}
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>活跃上报条数 activity_report_count</label>
                <input
                  type="number"
                  value={numText(getPath(doc, 'schedule.activity_report_count'))}
                  onChange={(e) => update('schedule.activity_report_count', numOrUndefined(e.target.value) ?? 1)}
                  placeholder="5"
                />
                <div className="desc">每号每次的上报条数（领猫前置需 5 次对话）。改动需重启网关生效。</div>
              </div>
            </div>
          </div>

          <div className="card">
            <div className="card-head">
              <h2>账号池与冷却</h2>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>软限流冷却基数 cooldown.soft_rate</label>
                <input type="text" value={str('cooldown.soft_rate')} onChange={(e) => update('cooldown.soft_rate', e.target.value)} placeholder="600s" />
                <div className="desc">429/限流文案触发的冷却时长，连续触发按 2 倍指数退避。</div>
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>软冷却退避封顶 cooldown.soft_rate_max</label>
                <input type="text" value={str('cooldown.soft_rate_max')} onChange={(e) => update('cooldown.soft_rate_max', e.target.value)} placeholder="2h" />
              </div>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>单账号最大在途 pool.max_in_flight</label>
                <input
                  type="text"
                  value={numStr('pool.max_in_flight')}
                  onChange={(e) => update('pool.max_in_flight', numOrUndefined(e.target.value))}
                  placeholder="3"
                />
                <div className="desc">0 = 不限制并发。</div>
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>熔断阈值 pool.breaker_threshold</label>
                <input
                  type="text"
                  value={numStr('pool.breaker_threshold')}
                  onChange={(e) => update('pool.breaker_threshold', numOrUndefined(e.target.value))}
                  placeholder="3"
                />
                <div className="desc">连续失败达到该次数即熔断。</div>
              </div>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>熔断基础时长 pool.breaker_cooldown</label>
                <input type="text" value={str('pool.breaker_cooldown')} onChange={(e) => update('pool.breaker_cooldown', e.target.value)} placeholder="30m" />
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>熔断退避封顶 pool.breaker_cooldown_max</label>
                <input type="text" value={str('pool.breaker_cooldown_max')} onChange={(e) => update('pool.breaker_cooldown_max', e.target.value)} placeholder="6h" />
              </div>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>闲置补偿速率 pool.idle_weight_per_hour</label>
                <input
                  type="text"
                  value={numStr('pool.idle_weight_per_hour')}
                  onChange={(e) => update('pool.idle_weight_per_hour', numOrUndefined(e.target.value))}
                  placeholder="0.5"
                />
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>闲置补偿封顶 pool.idle_weight_max</label>
                <input
                  type="text"
                  value={numStr('pool.idle_weight_max')}
                  onChange={(e) => update('pool.idle_weight_max', numOrUndefined(e.target.value))}
                  placeholder="5.0"
                />
              </div>
            </div>
          </div>

          <div className="card">
            <div className="card-head">
              <h2>上游超时</h2>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>短 RPC 总超时（秒）</label>
                <input
                  type="text"
                  value={numStr('upstream.timeout_seconds')}
                  onChange={(e) => update('upstream.timeout_seconds', numOrUndefined(e.target.value))}
                  placeholder="120"
                />
                <div className="desc">刷新 token / 签到 / 余额 / 模型列表的硬上限。</div>
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>聊天首字节超时（秒）</label>
                <input
                  type="text"
                  value={numStr('upstream.header_timeout_seconds')}
                  onChange={(e) => update('upstream.header_timeout_seconds', numOrUndefined(e.target.value))}
                  placeholder="120"
                />
                <div className="desc">超时即换号重发；留空回落 timeout_seconds。</div>
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>聊天流空闲超时（秒）</label>
                <input
                  type="text"
                  value={numStr('upstream.idle_timeout_seconds')}
                  onChange={(e) => update('upstream.idle_timeout_seconds', numOrUndefined(e.target.value))}
                  placeholder="300"
                />
                <div className="desc">持续吐数据不受影响，静默超时才断流。</div>
              </div>
            </div>
          </div>

          <div className="card">
            <div className="card-head">
              <h2>会话粘性与功能开关</h2>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label className="checkbox">
                  <input
                    type="checkbox"
                    checked={bool('session_sticky.enabled')}
                    onChange={(e) => update('session_sticky.enabled', e.target.checked)}
                  />
                  启用会话粘性
                </label>
                <div className="desc">同一会话固定到同一账号，多轮对话不跳号。</div>
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>粘性 TTL</label>
                <input type="text" value={str('session_sticky.ttl')} onChange={(e) => update('session_sticky.ttl', e.target.value)} placeholder="30m" />
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>粘性 GC 周期</label>
                <input
                  type="text"
                  value={str('session_sticky.gc_interval')}
                  onChange={(e) => update('session_sticky.gc_interval', e.target.value)}
                  placeholder="5m"
                />
              </div>
            </div>
            <div className="field" style={{ marginBottom: 0 }}>
              <label className="checkbox">
                <input
                  type="checkbox"
                  checked={bool('features.sanitize_blacklist_fingerprints')}
                  onChange={(e) => update('features.sanitize_blacklist_fingerprints', e.target.checked)}
                />
                出站请求体指纹脱敏
              </label>
              <div className="desc">清洗请求体中的黑名单指纹字段，降低被上游识别为自动化工具的风险。</div>
            </div>
          </div>

          <div className="card">
            <div className="card-head">
              <h2>Redis 镜像（可选）</h2>
              <span className="hint">留空 = 纯内存模式，功能照常</span>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 2 }}>
                <label>Upstash URL</label>
                <input type="text" value={str('upstash.url')} onChange={(e) => update('upstash.url', e.target.value)} placeholder="rediss://… 或 https://xxx.upstash.io" />
              </div>
              <div className="field" style={{ flex: 2 }}>
                <label>Upstash Token</label>
                <input
                  type={showApiKey ? 'text' : 'password'}
                  value={str('upstash.token')}
                  onChange={(e) => update('upstash.token', e.target.value)}
                  placeholder="REST token"
                />
              </div>
            </div>
          </div>

          {/* 通知（邮件提醒）：额度即将耗尽 / 额度已耗尽 / 账号路由切换 */}
          <div className="card">
            <div className="card-head">
              <h2>通知（邮件提醒）</h2>
              <span className="hint">SMTP 修改后需重启网关生效</span>
            </div>
            <label style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 10 }}>
              <input
                type="checkbox"
                checked={bool('notify.enabled')}
                onChange={(e) => update('notify.enabled', e.target.checked)}
              />
              启用邮件通知
            </label>
            <div className="row">
              <div className="field" style={{ flex: 2 }}>
                <label>SMTP 服务器 smtp_host</label>
                <input type="text" value={str('notify.smtp_host')} onChange={(e) => update('notify.smtp_host', e.target.value)} placeholder="smtp.qq.com" />
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>端口 smtp_port</label>
                <input type="text" value={numStr('notify.smtp_port')} onChange={(e) => update('notify.smtp_port', numOrUndefined(e.target.value))} placeholder="587" />
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>加密方式 smtp_tls</label>
                <select value={str('notify.smtp_tls') || 'starttls'} onChange={(e) => update('notify.smtp_tls', e.target.value)}>
                  <option value="starttls">starttls（587）</option>
                  <option value="tls">tls（465 隐式加密）</option>
                  <option value="none">none（明文，仅内网中继）</option>
                </select>
              </div>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>账号 smtp_username</label>
                <input type="text" value={str('notify.smtp_username')} onChange={(e) => update('notify.smtp_username', e.target.value)} placeholder="you@qq.com" />
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>授权码/密码 smtp_password</label>
                <div style={{ display: 'flex', gap: 6 }}>
                  <input
                    type={showSmtpPass ? 'text' : 'password'}
                    value={str('notify.smtp_password')}
                    onChange={(e) => update('notify.smtp_password', e.target.value)}
                    placeholder="SMTP 授权码（非登录密码）"
                  />
                  <button className="btn shrink" onClick={() => setShowSmtpPass((v) => !v)} type="button">
                    {showSmtpPass ? '隐藏' : '显示'}
                  </button>
                </div>
              </div>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>发件人 smtp_from</label>
                <input type="text" value={str('notify.smtp_from')} onChange={(e) => update('notify.smtp_from', e.target.value)} placeholder="与 smtp_username 一致" />
              </div>
              <div className="field" style={{ flex: 2 }}>
                <label>收件人 smtp_to（逗号分隔）</label>
                <input
                  type="text"
                  value={Array.isArray(getPath(doc ?? {}, 'notify.smtp_to')) ? (getPath(doc ?? {}, 'notify.smtp_to') as string[]).join(', ') : str('notify.smtp_to')}
                  onChange={(e) => update('notify.smtp_to', e.target.value.split(/[,，\s]+/).map((s) => s.trim()).filter(Boolean))}
                  placeholder="ops@example.com, me@example.com"
                />
              </div>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>额度告警阈值 notify.credits_threshold</label>
                <input type="text" value={numStr('notify.credits_threshold')} onChange={(e) => update('notify.credits_threshold', numOrUndefined(e.target.value))} placeholder="100" />
                <div className="desc">可用额度低于该值时提醒（"额度即将耗尽"）。</div>
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>快过期告警窗口 notify.expiring_days</label>
                <input type="text" value={numStr('notify.expiring_days')} onChange={(e) => update('notify.expiring_days', numOrUndefined(e.target.value))} placeholder="7" />
                <div className="desc">存在 N 天内过期的额度即提醒（天）。</div>
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>去重间隔 notify.throttle_hours</label>
                <input type="text" value={numStr('notify.throttle_hours')} onChange={(e) => update('notify.throttle_hours', numOrUndefined(e.target.value))} placeholder="6" />
                <div className="desc">同一账号同类事件的最小提醒间隔（小时）。</div>
              </div>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 2 }}>
                <label>额度扫描时刻 notify.scan_hours（逗号分隔整点，留空=不扫描）</label>
                <input
                  type="text"
                  value={hoursToText(getPath(doc ?? {}, 'notify.scan_hours'))}
                  onChange={(e) => update('notify.scan_hours', textToHours(e.target.value))}
                  placeholder="10, 22"
                />
                <div className="desc">建议排在签到（9/21 点）之后一小时，读到的是刷新后的余额。</div>
              </div>
              <div className="field" style={{ flex: 1 }}>
                <label>待发队列 queue_size</label>
                <input type="text" value={numStr('notify.queue_size')} onChange={(e) => update('notify.queue_size', numOrUndefined(e.target.value))} placeholder="256" />
              </div>
            </div>
            <div className="row">
              <div className="field" style={{ flex: 1 }}>
                <label>事件开关</label>
                <label style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <input type="checkbox" checked={getPath(doc ?? {}, 'notify.events.credits_low') !== false} onChange={(e) => update('notify.events.credits_low', e.target.checked)} />
                  额度即将耗尽
                </label>
                <label style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <input type="checkbox" checked={getPath(doc ?? {}, 'notify.events.exhausted') !== false} onChange={(e) => update('notify.events.exhausted', e.target.checked)} />
                  额度已耗尽
                </label>
                <label style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <input type="checkbox" checked={getPath(doc ?? {}, 'notify.events.account_switch') !== false} onChange={(e) => update('notify.events.account_switch', e.target.checked)} />
                  账号路由切换
                </label>
              </div>
              <div className="field" style={{ flex: 2 }}>
                <label>测试发送</label>
                <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                  <button
                    className="btn"
                    type="button"
                    disabled={testingNotify || writeDisabled}
                    onClick={() => void sendNotifyTest()}
                    title="用当前磁盘上的配置发送一封测试邮件（需先保存并重启网关）"
                  >
                    {testingNotify ? <Spinner /> : '📧'} 发送测试邮件
                  </button>
                  {notifyTestMsg && <span className={notifyTestOK ? 'text-ok' : 'text-danger'}>{notifyTestMsg}</span>}
                </div>
                <div className="desc">
                  测试发送使用的是<strong>网关当前已加载的配置</strong>：请先「保存配置」并在系统页重启网关，再点此验证。
                </div>
              </div>
            </div>
          </div>

          <div className="page-actions">
            <button className="btn btn-primary" onClick={() => void saveForm()} disabled={saving || writeDisabled}>
              {saving ? <Spinner /> : '💾'} 保存配置
            </button>
            <button className="btn" onClick={() => void load()} disabled={saving}>
              放弃修改
            </button>
          </div>
        </>
      )}

      {tab === 'json' && (
        <div className="card">
          <div className="card-head">
            <h2>JSON 源码</h2>
            <span className="hint">适合编辑表单未覆盖的自定义字段</span>
          </div>
          {jsonError && <Alert kind="error">JSON 语法错误：{jsonError}</Alert>}
          <textarea
            rows={26}
            value={jsonText}
            onChange={(e) => {
              setJsonText(e.target.value)
              setJsonError(null)
            }}
            spellCheck={false}
            style={{ minHeight: 460 }}
          />
          <div className="page-actions" style={{ marginTop: 13 }}>
            <button className="btn btn-primary" onClick={() => void saveJSON()} disabled={saving || writeDisabled}>
              {saving ? <Spinner /> : '💾'} 保存配置
            </button>
            <button className="btn" onClick={() => setJsonText(JSON.stringify(doc ?? {}, null, 2))}>
              还原为已加载内容
            </button>
          </div>
        </div>
      )}

      {confirmReset && (
        <ConfirmDialog
          title="从备份恢复配置"
          danger
          confirmText="确认恢复"
          busy={resetting}
          onCancel={() => setConfirmReset(false)}
          onConfirm={() => void doReset()}
          message={
            <>
              将用首次保存前的备份文件覆盖当前 <span className="mono">{meta?.path}</span>。
              <br />
              备份路径：<span className="mono">{meta?.backup_path}</span>
            </>
          }
        />
      )}
    </>
  )
}
