// verify-gui.mjs —— 登录真实网关（admin/admin123），逐页截图并断言关键元素。
// 由 tools/cdp/cdp.mjs 驱动：node cdp.mjs verify-gui.mjs

const BASE = 'http://localhost:5173'
const OUT = 'D:/workspace/magic/design/verify'

const ROUTES = [
  ['/', 'dashboard'],
  ['/accounts', 'accounts'],
  ['/keys', 'keys'],
  ['/stats', 'stats'],
  ['/requests', 'requests'],
  ['/automation', 'automation'],
  ['/login', 'login-wizard'],
  ['/playground', 'playground'],
  ['/config', 'config'],
  ['/system', 'system'],
]

const report = []

export default async function (page) {
  // ── 1. 登录 ────────────────────────────────────────────
  await page.goto(BASE + '/')
  await page.waitFor('document.querySelector(".login-card") || document.querySelector(".rail")', 20000)

  const needsLogin = await page.eval('!!document.querySelector(".login-card")')
  if (needsLogin) {
    await page.type('#u', 'admin')
    await page.type('#p', 'admin123')
    await page.eval('document.querySelector("form").requestSubmit()')
    await page.waitFor('!!document.querySelector(".rail")', 20000)
    report.push(['登录', '通过', '进入主框架'])
  } else {
    report.push(['登录', '跳过', '会话已存在'])
  }
  await page.screenshot(`${OUT}/00-login.png`)

  // ── 2. 逐页 ───────────────────────────────────────────
  for (const [path, name] of ROUTES) {
    const before = page.failures.length
    await page.goto(BASE + path + "?v=" + name, { settle: 1600 })

    const info = await page.eval(`(() => {
      const app = document.querySelector('.main') || document.body
      const counts = {
        alerts: document.querySelectorAll('.main .alert, .shell-alerts .alert').length,
        empty: app.querySelectorAll('.empty').length,
        spinner: app.querySelectorAll('.spinner').length,
        rows: app.querySelectorAll('tbody tr').length,
        tables: app.querySelectorAll('table').length,
        cards: app.querySelectorAll('.card, .section').length,
        errText: [...app.querySelectorAll('.alert')].map(e => e.innerText.trim()).filter(Boolean).slice(0,3),
      }
      // 页面里出现明显报错字样就是后端契约没对上
      const body = app.innerText || ''
      counts.suspicious = ['加载失败','查询失败','获取失败','Failed to fetch','Unauthorized','出错']
        .filter(s => body.includes(s))
      counts.chars = body.length
      counts.h2 = (app.querySelector('.page-head h1, .page-head h2, h1')||{}).innerText || ''
      return counts
    })()`)

    const errs = page.failures.slice(before)
    await page.screenshot(`${OUT}/${name}.png`, { full: true })

    const verdict = info.suspicious.length || errs.length ? '异常' : '通过'
    report.push([
      path,
      verdict,
      `字符 ${info.chars} · 表 ${info.tables}/${info.rows}行 · 空态 ${info.empty} · 告警 ${info.alerts}` +
        (info.errText.length ? ` · ${JSON.stringify(info.errText)}` : '') +
        (info.suspicious.length ? ` · 可疑文案 ${JSON.stringify(info.suspicious)}` : '') +
        (errs.length ? ` · JS异常 ${errs.length}` : ''),
    ])
  }

  // ── 3. 明色主题 ───────────────────────────────────────
  await page.goto(BASE + "/?v=light", { settle: 1500 })
  await page.eval(`document.documentElement.dataset.theme = 'light'; localStorage.setItem('wb2api.theme','light')`)
  await page.goto(BASE + '/', { settle: 1500 })
  await page.screenshot(`${OUT}/dashboard-light.png`, { full: true })
  const light = await page.eval(`(() => {
    const cs = getComputedStyle(document.documentElement)
    const main = document.querySelector('.main')
    return {
      theme: document.documentElement.dataset.theme,
      accent: cs.getPropertyValue('--accent').trim(),
      bgContent: cs.getPropertyValue('--bg-content').trim(),
      chartBar: cs.getPropertyValue('--chart-bar').trim(),
      mainBg: main ? getComputedStyle(main).backgroundColor : '',
      railBg: (() => { const r = document.querySelector('.rail'); return r ? getComputedStyle(r).backgroundColor : '' })(),
      fontBody: getComputedStyle(document.body).fontFamily,
      fontLoaded: document.fonts ? document.fonts.check('13px Inter') : null,
    }
  })()`)
  report.push(['明色主题', light.mainBg !== light.railBg ? '通过' : '异常', JSON.stringify(light)])

  // ── 4. 暗色还原 ───────────────────────────────────────
  await page.eval(`document.documentElement.dataset.theme = 'dark'; localStorage.setItem('wb2api.theme','dark')`)
  await page.goto(BASE + '/accounts', { settle: 1500 })
  await page.screenshot(`${OUT}/accounts-dark.png`, { full: true })
  report.push(['暗色主题', '通过', 'accounts 页已截图'])

  console.log('\n========== 验证报告 ==========')
  for (const [a, b, c] of report) console.log(`${a.padEnd(14)} ${b.padEnd(5)} ${c}`)
  console.log('==============================')
}
