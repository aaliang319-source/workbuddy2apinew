// cdp.mjs —— 用 Chrome DevTools Protocol 驱动一个独立的无头 Chrome，
// 用于在 Pencil 内嵌浏览器不可用时对 GUI 做真实截图与断言。
//
// 用法：node cdp.mjs <script.mjs> [--port 9333] [--width 1440] [--height 900]
// 脚本需导出 `export default async function (page) { ... }`。
// page 提供：goto / eval / waitFor / screenshot / click / type / text / logs。

import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync, mkdirSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { pathToFileURL } from 'node:url'

const CHROME_CANDIDATES = [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
]

const argv = process.argv.slice(2)
const scriptPath = argv[0]
if (!scriptPath) {
  console.error('usage: node cdp.mjs <script.mjs> [--port N] [--width N] [--height N]')
  process.exit(2)
}
function flag(name, def) {
  const i = argv.indexOf('--' + name)
  return i >= 0 ? Number(argv[i + 1]) : def
}
const PORT = flag('port', 9333)
const WIDTH = flag('width', 1440)
const HEIGHT = flag('height', 900)

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const chromeBin = CHROME_CANDIDATES.find((p) => existsSync(p)) || CHROME_CANDIDATES[0]

const profile = mkdtempSync(join(tmpdir(), 'cdp-prof-'))
const chrome = spawn(
  chromeBin,
  [
    '--headless=new',
    `--remote-debugging-port=${PORT}`,
    `--user-data-dir=${profile}`,
    `--window-size=${WIDTH},${HEIGHT}`,
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-gpu',
    '--hide-scrollbars',
    '--force-device-scale-factor=1',
    'about:blank',
  ],
  { stdio: ['ignore', 'pipe', 'pipe'] },
)

let chromeErr = ''
chrome.stderr.on('data', (d) => (chromeErr += d.toString()))

async function endpoint() {
  for (let i = 0; i < 100; i++) {
    try {
      const r = await fetch(`http://127.0.0.1:${PORT}/json/version`)
      if (r.ok) return await r.json()
    } catch {
      /* 还没起来 */
    }
    await sleep(100)
  }
  throw new Error('Chrome 调试端口未就绪。stderr:\n' + chromeErr)
}

class Page {
  constructor(ws) {
    this.ws = ws
    this.id = 0
    this.pending = new Map()
    this.logs = []
    this.failures = []
    ws.addEventListener('message', (ev) => {
      const msg = JSON.parse(ev.data)
      if (msg.id !== undefined) {
        const p = this.pending.get(msg.id)
        if (p) {
          this.pending.delete(msg.id)
          msg.error ? p.reject(new Error(msg.method + ': ' + JSON.stringify(msg.error))) : p.resolve(msg.result)
        }
        return
      }
      if (msg.method === 'Runtime.consoleAPICalled') {
        const text = (msg.params.args || [])
          .map((a) => (a.value !== undefined ? a.value : a.description || a.type))
          .join(' ')
        this.logs.push(`[${msg.params.type}] ${text}`)
      } else if (msg.method === 'Runtime.exceptionThrown') {
        const d = msg.params.exceptionDetails
        this.failures.push(d.exception?.description || d.text)
      } else if (msg.method === 'Page.javascriptDialogOpening') {
        this.logs.push(`[dialog] ${msg.params.type}: ${msg.params.message}`)
      }
    })
  }

  send(method, params = {}) {
    const id = ++this.id
    this.ws.send(JSON.stringify({ id, method, params }))
    return new Promise((res, rej) => this.pending.set(id, { resolve: res, reject: rej }))
  }

  async goto(url, { settle = 900 } = {}) {
    this.logs.length = 0
    await this.send('Page.navigate', { url })
    await this.waitFor('document.readyState === "complete"', 20000).catch(() => {})
    await sleep(settle)
  }

  /** 在页面里求值；表达式可以是 async IIFE，返回值按值取回。 */
  async eval(expression) {
    const r = await this.send('Runtime.evaluate', {
      expression,
      awaitPromise: true,
      returnByValue: true,
      userGesture: true,
    })
    if (r.exceptionDetails) {
      throw new Error('eval 异常: ' + (r.exceptionDetails.exception?.description || r.exceptionDetails.text))
    }
    return r.result.value
  }

  async waitFor(expr, timeout = 15000) {
    const deadline = Date.now() + timeout
    for (;;) {
      if (await this.eval(`!!(${expr})`).catch(() => false)) return true
      if (Date.now() > deadline) throw new Error(`等待超时: ${expr}`)
      await sleep(120)
    }
  }

  /** 截图并写入文件。full=true 时截取整页。 */
  async screenshot(outPath, { full = false } = {}) {
    let params = { format: 'png', captureBeyondViewport: full }
    if (full) {
      const m = await this.send('Page.getLayoutMetrics')
      const c = m.cssContentSize || m.contentSize
      params.clip = { x: 0, y: 0, width: Math.ceil(c.width), height: Math.ceil(c.height), scale: 1 }
    }
    const { data } = await this.send('Page.captureScreenshot', params)
    mkdirSync(resolve(outPath, '..'), { recursive: true })
    writeFileSync(outPath, Buffer.from(data, 'base64'))
    return outPath
  }

  /** 用原生 setter 写值，绕开 React 受控组件对 .value 的接管。 */
  async type(selector, value) {
    return this.eval(`(() => {
      const el = document.querySelector(${JSON.stringify(selector)})
      if (!el) return 'no-element'
      const proto = el instanceof HTMLTextAreaElement ? HTMLTextAreaElement : HTMLInputElement
      const setter = Object.getOwnPropertyDescriptor(proto.prototype, 'value').set
      setter.call(el, ${JSON.stringify(value)})
      el.dispatchEvent(new Event('input', { bubbles: true }))
      el.dispatchEvent(new Event('change', { bubbles: true }))
      return 'ok'
    })()`)
  }

  async click(selector) {
    return this.eval(`(() => {
      const el = document.querySelector(${JSON.stringify(selector)})
      if (!el) return 'no-element'
      el.click()
      return 'ok'
    })()`)
  }

  /** 按可见文本点击按钮/链接。 */
  async clickText(text, tag = 'button,a') {
    return this.eval(`(() => {
      const want = ${JSON.stringify(text)}
      const els = [...document.querySelectorAll(${JSON.stringify(tag)})]
      const el = els.find((e) => (e.textContent || '').trim().includes(want))
      if (!el) return 'no-element'
      el.click()
      return 'ok'
    })()`)
  }

  async text(selector) {
    return this.eval(`(document.querySelector(${JSON.stringify(selector)})?.innerText || '')`)
  }
}

async function main() {
  await endpoint()
  // 直接连到 about:blank 那个 page 目标；连不上就新建一个。
  const targets = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json()
  const page0 =
    targets.find((t) => t.type === 'page' && t.url === 'about:blank') || targets.find((t) => t.type === 'page')
  const pws = new WebSocket(page0.webSocketDebuggerUrl)
  await new Promise((res, rej) => {
    pws.addEventListener('open', res, { once: true })
    pws.addEventListener('error', rej, { once: true })
  })

  const page = new Page(pws)
  await page.send('Page.enable')
  await page.send('Runtime.enable')
  await page.send('Log.enable')
  await page.send('Emulation.setDeviceMetricsOverride', {
    width: WIDTH,
    height: HEIGHT,
    deviceScaleFactor: 1,
    mobile: false,
  })

  const mod = await import(pathToFileURL(resolve(scriptPath)).href)
  try {
    await mod.default(page)
  } finally {
    console.log('\n=== console ===')
    console.log(page.logs.slice(-60).join('\n') || '(空)')
    if (page.failures.length) {
      console.log('\n=== JS 异常 ===')
      console.log(page.failures.join('\n'))
    }
  }
}

main()
  .then(() => {
    chrome.kill()
    try {
      rmSync(profile, { recursive: true, force: true })
    } catch {
      /* 句柄未释放就留着，系统会清 */
    }
    process.exit(0)
  })
  .catch((err) => {
    console.error(err.stack || String(err))
    chrome.kill()
    process.exit(1)
  })
