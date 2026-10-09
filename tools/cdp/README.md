# GUI 浏览器验证工具

Pencil 的内嵌浏览器截图管线会卡死（`IPCError: Request 'browser' timed out after 60000ms`，
之后一直 `A capture is already running.`，连 `file://` 静态页都截不出来）。
这里用 Chrome DevTools Protocol 直连一个独立的无头 Chrome 替代它，
不依赖 Pencil，也不依赖任何 npm 包（Node 22 自带 `fetch` 与 `WebSocket`）。

## 用法

先起前端（指向真实网关 8787）：

```bash
cd gui/web && npm run dev          # http://localhost:5173
```

再跑：

```bash
cd tools/cdp
node cdp.mjs verify-gui.mjs        # 登录 admin/admin123，逐页截图 + 断言
node cdp.mjs verify-light.mjs      # 明色主题下复核
node cdp.mjs audit.mjs             # 全站缺陷普查（换行标签/溢出表格/图标重叠）
```

截图落在 `design/verify/`。

## 组成

- **`cdp.mjs`** —— 驱动器。拉起无头 Chrome（自动探测 Chrome/Edge 路径），
  暴露 `goto / eval / waitFor / screenshot / click / type / text / logs`。
  `type()` 用原生 value setter 写值，绕开 React 受控组件对 `.value` 的接管。
- **`verify-gui.mjs`** —— 冒烟：登录 → 10 条路由逐页截图 → 统计表格行数/空态/告警 →
  断言页面里不出现「加载失败」类文案与 JS 异常 → 明暗主题切换。
- **`audit.mjs`** —— 普查三类布局缺陷：行内筛选标签被挤成竖排、表格横向溢出、
  按钮内图标与文字重叠。改完 CSS 跑一遍，应当只剩已知的误报。
- **`verify-light.mjs`** —— 明色主题复核（暗色正常不代表明色正常，两次教训都出在这）。

## 已知误报

`audit.mjs` 会把「下一页 →」这类**图标在文字右**的按钮报成重叠。
检测器假设图标总在文字左侧；按文本节点左边界比较时，右侧图标必然被判为重叠。
（`clip` 为 0 且 overlap 恰为图标宽度时即可断定为该误报。）

## 为什么不用 Playwright

机器上装了 `playwright@1.59.1`，但为一次验证引入依赖不划算，且 CDP 直连能精确控制
无头 Chrome 的启动参数（窗口尺寸、禁用滚动条、强制缩放 1），截图可比性更好。
