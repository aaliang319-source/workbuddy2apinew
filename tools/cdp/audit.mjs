// probe3.mjs —— 全面体检：所有页面里的 (a) 行内筛选标签换行 (b) 横向溢出的表格
// (c) 按钮内图标与文字重叠。一次性把要修的清单打出来。
const BASE = 'http://localhost:5173'

const ROUTES = [
  ['/', '仪表盘'],
  ['/accounts', '账号管理'],
  ['/keys', 'API Keys'],
  ['/stats', '请求统计'],
  ['/requests', '请求明细'],
  ['/automation', '自动化'],
  ['/login', '添加账号'],
  ['/playground', '聊天测试'],
  ['/config', '网关配置'],
  ['/system', '系统'],
]

export default async function (page) {
  await page.goto(BASE + '/login?p=1')
  if (await page.eval('!!document.querySelector(".login-card")')) {
    await page.type('#u', 'admin')
    await page.type('#p', 'admin123')
    await page.eval('document.querySelector("form").requestSubmit()')
    await page.waitFor('!!document.querySelector(".rail")')
  }

  for (const [path, name] of ROUTES) {
    await page.goto(BASE + path + '?p=' + encodeURIComponent(name), { settle: 2000 })

    const r = await page.eval(`(() => {
      const out = {}

      // (a) 行内筛选标签：高度明显超过单行 => 文字被挤成多行
      out.wrappedLabels = [...document.querySelectorAll('label')]
        .filter(l => l.querySelector('select,input'))
        .map(l => {
          const txt = (l.childNodes[0] && l.childNodes[0].nodeType === 3 ? l.childNodes[0].textContent : '').trim()
          const h = Math.round(l.getBoundingClientRect().height)
          return { txt, h }
        })
        .filter(x => x.h > 44 && x.txt)

      // (b) 横向溢出的表格
      out.overflowTables = [...document.querySelectorAll('table')].map(t => {
        const wrap = t.closest('.table-wrap') || t.parentElement
        const hidden = t.scrollWidth - wrap.clientWidth
        return hidden > 8 ? { hidden, cols: t.querySelectorAll('thead th').length,
                             last: (t.querySelector('thead th:last-child')||{}).innerText } : null
      }).filter(Boolean)

      // (c) 按钮内图标与文字重叠：icon 右边界 > 文本左边界
      out.overlapBtns = []
      document.querySelectorAll('button, a.btn').forEach(b => {
        const svg = b.querySelector('svg')
        if (!svg) return
        const sr = svg.getBoundingClientRect()
        // 找按钮里的文本节点，用一个 range 量它的左边界
        const walker = document.createTreeWalker(b, NodeFilter.SHOW_TEXT)
        let n, left = Infinity
        while ((n = walker.nextNode())) {
          if (!n.textContent.trim()) continue
          const rg = document.createRange(); rg.selectNodeContents(n)
          const rr = rg.getBoundingClientRect()
          if (rr.width > 0) left = Math.min(left, rr.left)
        }
        if (left === Infinity) return
        const clip = b.scrollWidth - b.clientWidth
        if (sr.right > left + 0.5 || clip > 1) {
          out.overlapBtns.push({ text: b.innerText.trim().slice(0,18), overlap: +(sr.right-left).toFixed(1), clip })
        }
      })

      // (d) 任何横向溢出的容器（非表格）
      out.overflowEls = [...document.querySelectorAll('.main *')]
        .filter(e => e.scrollWidth - e.clientWidth > 12 && !['TABLE','DIV'].includes(e.tagName)===false)
        .slice(0, 6)
        .map(e => ({ tag: e.tagName, cls: (e.className||'').toString().slice(0,40), over: e.scrollWidth - e.clientWidth }))

      return out
    })()`)

    const lines = []
    if (r.wrappedLabels.length) lines.push('  换行标签: ' + JSON.stringify(r.wrappedLabels))
    if (r.overflowTables.length) lines.push('  溢出表格: ' + JSON.stringify(r.overflowTables))
    if (r.overlapBtns.length) lines.push('  图标重叠: ' + JSON.stringify(r.overlapBtns))
    if (r.overflowEls.length) lines.push('  溢出容器: ' + JSON.stringify(r.overflowEls))
    console.log(`\n${name} (${path})`)
    console.log(lines.length ? lines.join('\n') : '  OK')
  }
}
