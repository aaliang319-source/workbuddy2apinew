// App.tsx 应用外壳：登录门禁 + 左侧导航 + 路由。
import { useCallback, useEffect, useMemo, useState } from 'react'
import { NavLink, Navigate, Route, Routes, useNavigate } from 'react-router-dom'
import {
  Activity,
  ChartColumn,
  KeyRound,
  LayoutDashboard,
  LogOut,
  MessageSquare,
  Moon,
  RefreshCw,
  ScrollText,
  Settings,
  Sun,
  UserPlus,
  Users,
  type LucideIcon,
} from 'lucide-react'
import { api, setUnauthorizedHandler } from './api'
import type { SessionInfo } from './types'
import { Alert, BrandMark, Spinner } from './ui'
import { useTheme, type Theme } from './theme'
import Login from './pages/Login'
import Dashboard from './pages/Dashboard'
import StatsPage from './pages/StatsPage'
import Requests from './pages/Requests'
import Accounts from './pages/Accounts'
import LoginWizard from './pages/LoginWizard'
import Playground from './pages/Playground'
import ConfigPage from './pages/ConfigPage'
import System from './pages/System'
import Keys from './pages/Keys'
import Automation from './pages/Automation'

type NavItem = { to: string; label: string; icon: LucideIcon; end?: boolean }

const NAV: NavItem[] = [
  { to: '/', label: '仪表盘', icon: LayoutDashboard, end: true },
  { to: '/accounts', label: '账号管理', icon: Users },
  { to: '/keys', label: 'API Keys', icon: KeyRound },
  { to: '/stats', label: '请求统计', icon: ChartColumn },
  { to: '/requests', label: '请求明细', icon: ScrollText },
  { to: '/automation', label: '自动化', icon: RefreshCw },
  { to: '/login', label: '添加账号', icon: UserPlus },
  { to: '/playground', label: '聊天测试', icon: MessageSquare },
  { to: '/config', label: '网关配置', icon: Settings },
  { to: '/system', label: '系统', icon: Activity },
]

export default function App() {
  const [session, setSession] = useState<SessionInfo | null>(null)
  const [loading, setLoading] = useState(true)
  const [banner, setBanner] = useState<string | null>(null)
  const navigate = useNavigate()
  const [theme, toggleTheme] = useTheme()

  const refreshSession = useCallback(async () => {
    try {
      const s = await api.session()
      setSession(s)
      return s
    } catch {
      // 会话接口失败（后端未起来）也要结束 loading，否则页面永久空白。
      setSession(null)
      return null
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void refreshSession()
  }, [refreshSession])

  // 任何接口返回 401 时统一回到登录页，避免用户在坏会话里反复点。
  useEffect(() => {
    setUnauthorizedHandler(() => {
      setSession((prev) => (prev ? { ...prev, authenticated: false } : prev))
      setBanner('会话已过期，请重新登录')
      navigate('/')
    })
  }, [navigate])

  const handleLoggedOut = useCallback(async () => {
    await api.logout().catch(() => undefined)
    setSession((s) => (s ? { ...s, authenticated: false, username: '' } : s))
  }, [])

  if (loading) {
    return (
      <div className="login-wrap">
        <div className="login-glow" />
        <Spinner label="正在连接服务…" />
      </div>
    )
  }

  if (!session?.authenticated) {
    return <Login info={session} banner={banner} onCloseBanner={() => setBanner(null)} onSuccess={refreshSession} />
  }

  return (
    <Shell
      session={session}
      theme={theme}
      onToggleTheme={toggleTheme}
      onLogout={handleLoggedOut}
      onSessionRefresh={refreshSession}
    />
  )
}

function Shell({
  session,
  theme,
  onToggleTheme,
  onLogout,
  onSessionRefresh,
}: {
  session: SessionInfo
  theme: Theme
  onToggleTheme: () => void
  onLogout: () => void
  onSessionRefresh: () => Promise<SessionInfo | null>
}) {
  const warnings = useMemo(() => {
    const list: string[] = []
    if (session.using_default_password) {
      list.push('面板正在使用默认口令（admin / workbuddy），建议在服务端配置中修改 ui.password')
    }
    if (session.read_only) {
      list.push('服务端处于只读模式：登录、签到、改配置等写操作全部禁用')
    }
    return list
  }, [session])

  const ThemeIcon = theme === 'dark' ? Moon : Sun

  return (
    <div className="app">
      <aside className="rail">
        <div className="brand">
          <BrandMark size={28} />
          <div className="brand-text">
            <strong>WorkBuddy</strong>
            <span>2APInew 控制台</span>
          </div>
        </div>

        {NAV.map((item) => {
          const Icon = item.icon
          return (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => `nav-item ${isActive ? 'active' : ''}`}
            >
              <span className="nav-icon">
                <Icon size={15} strokeWidth={1.75} />
              </span>
              <span className="nav-label">{item.label}</span>
            </NavLink>
          )
        })}

        <div className="nav-spacer" />

        <div className="nav-foot">
          <div className="who">
            <LogOut size={12} strokeWidth={1.75} style={{ flex: '0 0 auto' }} />
            <span className="mono">{(session.username || '—') + (session.read_only ? ' · 只读' : '')}</span>
          </div>
          <button className="btn btn-sm" style={{ width: '100%' }} onClick={onLogout}>
            退出登录
          </button>
        </div>

        <button className="nav-item theme-toggle" onClick={onToggleTheme}>
          <span className="nav-icon">
            <ThemeIcon size={15} strokeWidth={1.75} />
          </span>
          <span className="nav-label">主题 · {theme === 'dark' ? '暗色' : '明色'}</span>
        </button>
      </aside>

      <main className="main">
        {warnings.length > 0 && (
          <div className="shell-alerts">
            {warnings.map((w) => (
              <Alert key={w} kind="warn">
                {w}
              </Alert>
            ))}
          </div>
        )}
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/accounts" element={<Accounts session={session} />} />
          <Route path="/keys" element={<Keys writable={!session.read_only} />} />
          <Route path="/stats" element={<StatsPage session={session} />} />
          <Route path="/automation" element={<Automation session={session} />} />
          <Route path="/requests" element={<Requests session={session} />} />
          <Route path="/login" element={<LoginWizard session={session} onDone={onSessionRefresh} />} />
          <Route path="/playground" element={<Playground />} />
          <Route path="/config" element={<ConfigPage session={session} />} />
          <Route path="/system" element={<System session={session} onSessionRefresh={onSessionRefresh} />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
  )
}
