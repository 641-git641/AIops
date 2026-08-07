import { Layout, Menu } from 'antd'
import { Link, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import {
  AlertOutlined,
  ApartmentOutlined,
  BugOutlined,
  DashboardOutlined,
  FileDoneOutlined,
  NodeIndexOutlined,
  NotificationOutlined,
  ToolOutlined,
} from '@ant-design/icons'
import { useRealtime } from './realtime'
import Alerts from './pages/Alerts'
import Incidents from './pages/Incidents'
import IncidentDetail from './pages/IncidentDetail'
import MaintenanceWindows from './pages/MaintenanceWindows'
import NotificationChannels from './pages/NotificationChannels'
import Playbooks from './pages/Playbooks'
import Topology from './pages/Topology'
import Workflows from './pages/Workflows'

const { Header, Sider, Content } = Layout

const navItems = [
  { key: '/incidents', icon: <DashboardOutlined />, label: <Link to="/incidents">Incidents</Link> },
  { key: '/alerts', icon: <AlertOutlined />, label: <Link to="/alerts">Alerts</Link> },
  { key: '/rules', icon: <BugOutlined />, label: <Link to="/rules">Rules</Link> },
  { key: '/playbooks', icon: <FileDoneOutlined />, label: <Link to="/playbooks">Playbooks</Link> },
  { key: '/maintenance', icon: <ToolOutlined />, label: <Link to="/maintenance">维护窗口</Link> },
  { key: '/workflows', icon: <NodeIndexOutlined />, label: <Link to="/workflows">工作流</Link> },
  { key: '/topology', icon: <ApartmentOutlined />, label: <Link to="/topology">拓扑</Link> },
  { key: '/notify-channels', icon: <NotificationOutlined />, label: <Link to="/notify-channels">通知渠道</Link> },
]

export default function App() {
  useRealtime()
  const location = useLocation()

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
          items={navItems}
        />
      </Sider>
      <Layout>
        <Header style={{ background: '#fff', padding: '0 24px', fontSize: 15, fontWeight: 500 }}>
          告警与事故管理
        </Header>
        <Content style={{ margin: 16 }}>
          <Routes>
            <Route path="/" element={<Navigate to="/incidents" replace />} />
            <Route path="/alerts" element={<Alerts />} />
            <Route path="/incidents" element={<Incidents />} />
            <Route path="/incidents/:id" element={<IncidentDetail />} />
            <Route path="/rules" element={<RulesPlaceholder />} />
            <Route path="/playbooks" element={<Playbooks />} />
            <Route path="/maintenance" element={<MaintenanceWindows />} />
            <Route path="/workflows" element={<Workflows />} />
            <Route path="/topology" element={<Topology />} />
            <Route path="/notify-channels" element={<NotificationChannels />} />
          </Routes>
        </Content>
      </Layout>
    </Layout>
  )
}

function RulesPlaceholder() {
  return <div style={{ color: '#999' }}>规则管理（Phase 2 提供编辑画布，当前展示后端列表即可）</div>
}
