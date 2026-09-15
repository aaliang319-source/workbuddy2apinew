// ccswitch.ts CC Switch 深链生成（官方 v1 协议，见 cc-switch 手册 5.3-deeplink）：
//   ccswitch://v1/import?resource=provider&app=claude|codex&name=&endpoint=&apiKey=&model=...
// 用户点击链接 → 系统唤起本机 CC Switch → 确认弹窗 → 导入为供应商。
// 由 System 页与 Keys 页共用；网关双协议原生直连（Anthropic Messages + OpenAI
// Chat/Responses），导入即用，不依赖 CC Switch 的协议转换组件。

/** parseListenPort 从 config.listen（如 ":7863" / "0.0.0.0:7863"）解析端口，兜底 7863。 */
export function parseListenPort(listen: unknown): number {
  const s = typeof listen === 'string' ? listen : ''
  const m = s.match(/(\d+)\s*$/)
  return m ? parseInt(m[1], 10) : 7863
}

/** gatewayEndpoint 从网关 config 推导浏览器可达的 endpoint（面板的 gateway_url 是
 * 容器视角 host.docker.internal，宿主机浏览器打不开；同主机名的网关端口即可达）。 */
export function gatewayEndpoint(listen: unknown): string {
  return `http://${window.location.hostname}:${parseListenPort(listen)}`
}

/** CcSwitchFields 生成 deep link 所需的全部字段。 */
export interface CcSwitchFields {
  endpoint: string
  apiKey: string
  model: string
  haiku: string
  sonnet: string
  opus: string
  codexModel: string
}

/** buildCcSwitchLink 按官方 v1 协议拼 deep link（URLSearchParams 自动做百分号编码）。 */
export function buildCcSwitchLink(app: 'claude' | 'codex', f: CcSwitchFields, name = 'WorkBuddy2API'): string {
  const p = new URLSearchParams({
    resource: 'provider',
    app,
    name,
    endpoint: f.endpoint,
    apiKey: f.apiKey,
    notes: '由 workbuddy2api 面板生成',
  })
  // 空值不写入：Keys 页的最小字段集（模型留空 = 网关 anthropic 段兜底）。
  if (app === 'claude') {
    if (f.model) p.set('model', f.model)
    if (f.haiku) p.set('haikuModel', f.haiku)
    if (f.sonnet) p.set('sonnetModel', f.sonnet)
    if (f.opus) p.set('opusModel', f.opus)
  } else if (f.codexModel) {
    p.set('model', f.codexModel)
  }
  return `ccswitch://v1/import?${p.toString()}`
}

/** emptyCcSwitchFields 仅含 endpoint/apiKey 的最小字段集（Keys 页按 Key 生成深链用；
 * 模型档位留空 = 不指定，CC Switch 落盘由网关 anthropic 段兜底）。 */
export function minimalCcSwitchFields(endpoint: string, apiKey: string): CcSwitchFields {
  return { endpoint, apiKey, model: '', haiku: '', sonnet: '', opus: '', codexModel: '' }
}
