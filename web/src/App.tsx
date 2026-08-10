import { Layout, Menu, Spin } from 'antd'
import { Link, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import {
  AlertOutlined,
  ApartmentOutlined,
  BookOutlined,
  BugOutlined,
  DashboardOutlined,
  FileDoneOutlined,
  HomeOutlined,
  LinkOutlined,
  NodeIndexOutlined,
  NotificationOutlined,
  RobotOutlined,
  SettingOutlined,
  ToolOutlined,
} from '@ant-design/icons'
import { useRealtime } from './realtime'
import { api, getAPIKey } from './api'
import Alerts from './pages/Alerts'
import Assistant from './pages/Assistant'
import Incidents from './pages/Incidents'
import IncidentDetail from './pages/IncidentDetail'
import KnowledgePage from './pages/KnowledgePage'
import MaintenanceWindows from './pages/MaintenanceWindows'
import ModelConfigs from './pages/ModelConfigs'
import NotificationChannels from './pages/NotificationChannels'
import Playbooks from './pages/Playbooks'
import Settings from './pages/Settings'
import Topology from './pages/Topology'
import Unlock from './pages/Unlock'
import Wallboard from './pages/Wallboard'
import Workflows from './pages/Workflows'

const { Header, Sider, Content } = Layout

const navItems = [
  { key: '/overview', icon: <HomeOutlined />, label: <Link to="/overview">总览</Link> },
  { key: '/incidents', icon: <DashboardOutlined />, label: <Link to="/incidents">Incidents</Link> },
  { key: '/alerts', icon: <AlertOutlined />, label: <Link to="/alerts">Alerts</Link> },
  { key: '/rules', icon: <BugOutlined />, label: <Link to="/rules">Rules</Link> },
  { key: '/playbooks', icon: <FileDoneOutlined />, label: <Link to="/playbooks">Playbooks</Link> },
  { key: '/maintenance', icon: <ToolOutlined />, label: <Link to="/maintenance">维护窗口</Link> },
  { key: '/workflows', icon: <NodeIndexOutlined />, label: <Link to="/workflows">工作流</Link> },
  { key: '/topology', icon: <ApartmentOutlined />, label: <Link to="/topology">拓扑</Link> },
  { key: '/notify-channels', icon: <NotificationOutlined />, label: <Link to="/notify-channels">通知渠道</Link> },
  { key: '/knowledge', icon: <BookOutlined />, label: <Link to="/knowledge">知识库</Link> },
  { key: '/assistant', icon: <RobotOutlined />, label: <Link to="/assistant">AI 助手</Link> },
  { key: '/model-configs', icon: <RobotOutlined />, label: <Link to="/model-configs">模型配置</Link> },
  { key: '/settings', icon: <SettingOutlined />, label: <Link to="/settings">设置</Link> },
]

export default function App() {
  // 访问保护守卫（KEEPON 4.3）：apikey 模式且无会话凭据 → 解锁页；noauth 直通
  const { data, isLoading } = useQuery({
    queryKey: ['auth-status'],
    queryFn: api.authStatus,
    retry: false,
    staleTime: 60_000,
  })
  if (isLoading) return <Spin fullscreen />
  if (data?.mode === 'apikey' && !getAPIKey()) return <Unlock hasKeys={data.hasKeys} />
  return <MainLayout />
}

function MainLayout() {
  useRealtime() // hooks 恒调用（顺序安全）后再条件渲染 wallboard
  const location = useLocation()
  if (location.pathname === '/wallboard') return <Wallboard />

  // 外部可观测系统（KEEPON 6.2）：enabled 条目进侧边栏分组，新标签页打开（noopener 防反向标签）
  const { data: extData } = useQuery({
    queryKey: ['external-systems'],
    queryFn: api.externalSystems,
    retry: false,
    staleTime: 60_000,
  })
  const externalItems = (extData?.items ?? [])
    .filter((s) => s.enabled)
    .map((s) => ({
      key: `ext:${s.id}`,
      icon: <LinkOutlined />,
      label: (
        <span
          onClick={() => window.open(s.url, '_blank', 'noopener,noreferrer')}
          style={{ cursor: 'pointer' }}
        >
          {s.name}
        </span>
      ),
    }))
  const menuItems = externalItems.length > 0
    ? [...navItems, { type: 'group' as const, label: '外部系统', children: externalItems }]
    : navItems

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider theme="dark" width={200}>
        <div style={{ color: '#fff', fontSize: 16, fontWeight: 600, padding: '16px 20px' }}>
          AIOps
        </div>
        <Menu
          theme="dark"
          mode="inline"
          selectedKeys={[location.pathname]}
          items={menuItems}
        />
      </Sider>
      <Layout>
        <Header style={{ background: '#fff', padding: '0 24px', fontSize: 15, fontWeight: 500 }}>
          告警与事故管理
        </Header>
        <Content style={{ margin: 16 }}>
          <Routes>
            <Route path="/" element={<Navigate to="/overview" replace />} />
            {/* 总览 = 监控大屏（embedded 保留侧边栏导航；全屏挂墙模式在 /wallboard） */}
            <Route path="/overview" element={<Wallboard embedded />} />
            <Route path="/alerts" element={<Alerts />} />
            <Route path="/incidents" element={<Incidents />} />
            <Route path="/incidents/:id" element={<IncidentDetail />} />
            <Route path="/rules" element={<RulesPlaceholder />} />
            <Route path="/playbooks" element={<Playbooks />} />
            <Route path="/maintenance" element={<MaintenanceWindows />} />
            <Route path="/workflows" element={<Workflows />} />
            <Route path="/topology" element={<Topology />} />
            <Route path="/notify-channels" element={<NotificationChannels />} />
            <Route path="/knowledge" element={<KnowledgePage />} />
            <Route path="/assistant" element={<Assistant />} />
            <Route path="/model-configs" element={<ModelConfigs />} />
            <Route path="/settings" element={<Settings />} />
          </Routes>
        </Content>
      </Layout>
    </Layout>
  )
}

function RulesPlaceholder() {
  return <div style={{ color: '#999' }}>规则管理（Phase 2 提供编辑画布，当前展示后端列表即可）</div>
}
