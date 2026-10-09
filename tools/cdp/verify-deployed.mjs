// verify-deployed.mjs —— 验证【已部署的容器】(localhost:8787)，
// 不是 dev server。逐页截图 + 断言 + 明暗主题。
const BASE = 'http://localhost:8787'
const OUT = 'D:/workspace/magic/design/verify/deployed'
// 期望嵌在容器里的 JS 产物名（gui/internal/webui/dist/assets/ 下的实际文件名）。
// 每次 `npm run build` 会换哈希——从 dist 目录取值，别把旧哈希写死在这里。
const EXPECT_ASSET = process.env.EXPECT_ASSET || 'index-'

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
  await page.goto(BASE + '/?d=1')
  await page.waitFor('document.querySelector(".login-card") || document.querySelector(".rail")', 20000)
  if (await page.eval('!!document.querySelector(".login-card")')) {
    await page.type('#u', 'admin')
    await page.type('#p', 'admin123')
    await page.eval('document.querySelector("form").requestSubmit()')
    await page.waitFor('!!document.querySelector(".rail")', 20000)
    report.push(['登录', '通过', '进入主框架'])
  } else {
    report.push(['登录', '跳过', '会话已存在'])
  }

  // 资产版本：确认容器给的就是本地新产物（哈希随每次构建变化，比对本地 dist 实际值）
  const asset = await page.eval(`(document.querySelector('script[src*=index-]')||{}).src || ''`)
  report.push(['资产版本', asset.includes(EXPECT_ASSET) ? '通过' : '异常', asset.split('/').pop()])

  for (const [path, name] of ROUTES) {
    const before = page.failures.length
    await page.goto(BASE + path + '?d=' + name, { settle: 1800 })
    const info = await page.eval(`(() => {
      const app = document.querySelector('.main') || document.body
      const t = app.querySelector('table'); const w = t && (t.closest('.table-wrap')||t.parentElement)
      const labs = [...app.querySelectorAll('label')].filter(l=>l.querySelector('select')).map(l => {
        let tn=null; for (const n of l.childNodes) if (n.nodeType===3 && n.textContent.trim()) { tn=n; break }
        if (!tn) return null
        const rg=document.createRange(); rg.selectNodeContents(tn)
        return { t: tn.textContent.trim(), lines: rg.getClientRects().length }
      }).filter(Boolean)
      const body = app.innerText || ''
      return {
        rows: app.querySelectorAll('tbody tr').length,
        tables: app.querySelectorAll('table').length,
        overflow: t ? t.scrollWidth - w.clientWidth : 0,
        wrapped: labs.filter(x=>x.lines>1),
        suspicious: ['加载失败','查询失败','获取失败','Failed to fetch','Unauthorized'].filter(s=>body.includes(s)),
        chars: body.length,
      }
    })()`)
    const errs = page.failures.slice(before)
    await page.screenshot(`${OUT}/${name}.png`, { full: true })
    const bad = info.suspicious.length || errs.length || info.wrapped.length || info.overflow > 8
    report.push([path, bad ? '异常' : '通过',
      `表 ${info.tables}/${info.rows}行 · 溢出 ${info.overflow}` +
      (info.wrapped.length ? ` · 换行标签 ${JSON.stringify(info.wrapped)}` : '') +
      (info.suspicious.length ? ` · 可疑 ${JSON.stringify(info.suspicious)}` : '') +
      (errs.length ? ` · JS异常 ${errs.length}` : '')])
  }

  // 明色
  await page.eval(`localStorage.setItem('wb2api.theme','light')`)
  await page.goto(BASE + '/?d=light', { settle: 1800 })
  await page.screenshot(`${OUT}/dashboard-light.png`, { full: true })
  const L = await page.eval(`(() => { const m=document.querySelector('.main'), r=document.querySelector('.rail')
    return { theme: document.documentElement.dataset.theme,
             main: getComputedStyle(m).backgroundColor, rail: r?getComputedStyle(r).backgroundColor:'',
             accent: getComputedStyle(document.documentElement).getPropertyValue('--accent').trim(),
             font: document.fonts.check('13px Inter') } })()`)
  report.push(['明色', L.main !== L.rail && L.font ? '通过' : '异常', JSON.stringify(L)])

  await page.eval(`localStorage.setItem('wb2api.theme','dark')`)
  await page.goto(BASE + '/accounts?d=dark', { settle: 1800 })
  await page.screenshot(`${OUT}/accounts-dark.png`, { full: true })

  console.log('\n===== 已部署容器验证 (localhost:8787) =====')
  for (const [a, b, c] of report) console.log(`${a.padEnd(13)} ${b.padEnd(5)} ${c}`)
  console.log('=========================================')
}
