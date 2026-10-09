// final-light.mjs —— 明色主题下复核两处修复（Keys 表格、请求明细筛选行）。
const BASE = 'http://localhost:5173'
const OUT = 'D:/workspace/magic/design/verify'

export default async function (page) {
  await page.goto(BASE + '/login?p=1')
  if (await page.eval('!!document.querySelector(".login-card")')) {
    await page.type('#u', 'admin')
    await page.type('#p', 'admin123')
    await page.eval('document.querySelector("form").requestSubmit()')
    await page.waitFor('!!document.querySelector(".rail")')
  }
  await page.eval(`localStorage.setItem('wb2api.theme','light')`)

  for (const [path, name] of [['/keys','keys-light'], ['/requests','requests-light'], ['/accounts','accounts-light'], ['/','dash-light']]) {
    await page.goto(BASE + path + '?p=' + name, { settle: 2400 })
    const m = await page.eval(`(() => {
      const t = document.querySelector('table')
      let over = null
      if (t) { const w = t.closest('.table-wrap') || t.parentElement; over = t.scrollWidth - w.clientWidth }
      const labs = [...document.querySelectorAll('label')].filter(l=>l.querySelector('select'))
        .map(l => {
          let tn = null
          for (const n of l.childNodes) if (n.nodeType===3 && n.textContent.trim()) { tn = n; break }
          if (!tn) return null
          const rg = document.createRange(); rg.selectNodeContents(tn)
          return { t: tn.textContent.trim(), lines: rg.getClientRects().length }
        }).filter(Boolean)
      return { theme: document.documentElement.dataset.theme, over, labels: labs,
               rows: t ? [...t.querySelectorAll('tbody tr')].slice(0,4).map(r=>Math.round(r.getBoundingClientRect().height)) : null }
    })()`)
    console.log(name.padEnd(16), JSON.stringify(m))
    await page.screenshot(`${OUT}/${name}.png`, { full: true })
  }
}
