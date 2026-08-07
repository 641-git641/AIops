import { useQuery } from '@tanstack/react-query'
import { Table, Tag, Typography, Space, Badge } from 'antd'
import { Link } from 'react-router-dom'
import { api, type Incident } from '../api'

const severityColor: Record<string, string> = {
  critical: 'red',
  high: 'volcano',
  medium: 'gold',
  low: 'blue',
}

const statusColor: Record<string, string> = {
  open: 'processing',
  investigating: 'warning',
  healing: 'gold',
  pending_approval: 'purple',
  resolved: 'success',
}

const sourceLabel: Record<string, string> = { rule: '规则', agent: 'Agent', ai: 'AI' }

export default function Incidents() {
  const { data, isFetching } = useQuery({
    queryKey: ['incidents'],
    queryFn: () => api.incidents(),
  })

  const columns = [
    {
      title: '名称',
      key: 'name',
      render: (_: unknown, r: Incident) => (
        <Link to={`/incidents/${r.id}`}>{r.name || r.aiGeneratedName || r.id}</Link>
      ),
    },
    {
      title: '严重度',
      dataIndex: 'severity',
      key: 'severity',
      render: (s: string) => <Tag color={severityColor[s] || 'default'}>{s}</Tag>,
    },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      render: (s: string) => <Badge color={statusColor[s]} text={s} />,
    },
    { title: '来源', dataIndex: 'source', key: 'source', render: (s: string) => sourceLabel[s] || s },
    { title: '创建时间', dataIndex: 'createdAt', key: 'createdAt', render: (v: string) => new Date(v).toLocaleString() },
  ]

  return (
    <Space direction="vertical" style={{ width: '100%' }}>
      <Typography.Title level={4} style={{ margin: 0 }}>
        Incidents
      </Typography.Title>
      <Table<Incident>
        rowKey="id"
        loading={isFetching}
        dataSource={data?.items}
        columns={columns}
        pagination={{ total: data?.total, defaultPageSize: 20, showTotal: (t) => `共 ${t} 条` }}
      />
    </Space>
  )
}
