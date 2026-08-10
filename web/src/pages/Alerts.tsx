import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Table, Tag, Typography, Space, Badge, Select, message } from 'antd'
import { SendOutlined, ThunderboltOutlined } from '@ant-design/icons'
import { useState } from 'react'
import { api, type AlertDTO } from '../api'
import GuideCard from '../components/GuideCard'

const severityColor: Record<string, string> = {
  critical: 'red',
  high: 'volcano',
  medium: 'gold',
  low: 'blue',
  info: 'default',
}

const statusDot: Record<string, string> = {
  firing: 'error',
  acknowledged: 'warning',
  resolved: 'success',
  suppressed: 'default',
}

export default function Alerts() {
  const [severity, setSeverity] = useState<string>()
  const qc = useQueryClient()
  const { data, isFetching } = useQuery({
    queryKey: ['alerts', severity],
    queryFn: () => api.alerts(severity ? { severity } : {}),
  })

  const cluster = useMutation({
    mutationFn: () => api.aiCluster(),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ['incidents'] })
      qc.invalidateQueries({ queryKey: ['alerts'] })
      if (r.created.length > 0) {
        message.success(`聚类确认 ${r.clusters} 簇，生成 ${r.created.length} 个 Incident（合并 ${r.merged}）`)
      } else if (r.merged > 0) {
        message.info(`聚类 ${r.clusters} 簇，全部合并进既有 Incident`)
      } else {
        message.info(`扫描 ${r.scanned} 条告警，低于阈值（${r.skipped} 簇未确认）`)
      }
    },
    onError: (e: Error) => message.error(`聚类失败：${e.message}`),
  })

  const columns = [
    { title: '名称', dataIndex: 'name', key: 'name' },
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
      render: (s: string) => <Badge status={(statusDot[s] as any) || 'default'} text={s} />,
    },
    { title: '来源', dataIndex: 'provider', key: 'provider', width: 130 },
    { title: '服务', key: 'service', width: 140, render: (_: unknown, r: AlertDTO) => (r.labels?.service as string) || '-' },
    { title: '最近上报', dataIndex: 'lastSeenAt', key: 'lastSeenAt', width: 200, render: (v: string) => new Date(v).toLocaleString() },
  ]

  return (
    <Space direction="vertical" style={{ width: '100%' }}>
      <Space>
        <Typography.Title level={4} style={{ margin: 0 }}>
          告警
        </Typography.Title>
        <Select
          allowClear
          placeholder="按严重度过滤"
          style={{ width: 150 }}
          value={severity}
          onChange={setSeverity}
          options={['critical', 'high', 'medium', 'low'].map((s) => ({ label: s, value: s }))}
        />
        <Button
          icon={<ThunderboltOutlined />}
          loading={cluster.isPending}
          onClick={() => cluster.mutate()}
        >
          聚类生成事故
        </Button>
      </Space>
      <Table<AlertDTO>
        rowKey="id"
        loading={isFetching}
        dataSource={data?.items}
        columns={columns}
        pagination={{ total: data?.total, defaultPageSize: 20, showTotal: (t) => `共 ${t} 条` }}
        locale={{ emptyText: <AlertGuide /> }}
      />
    </Space>
  )
}

// AlertGuide 空状态接入向导（KEEPON 8.2 三步闭环第一步）：复制 curl + 发送测试告警。
function AlertGuide() {
  const qc = useQueryClient()
  const send = useMutation({
    mutationFn: api.sendTestAlert,
    onSuccess: (r) => {
      const e = r.events?.[0]
      message.success(
        `测试告警已发送${e ? `：归一化 severity=${e.severity}，指纹 ${e.fingerprint.slice(0, 8)}…` : ''}`,
      )
      qc.invalidateQueries({ queryKey: ['alerts'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (e: Error) => message.error(`发送失败：${e.message}`),
  })
  return (
    <GuideCard
      extra={
        <Button type="primary" icon={<SendOutlined />} loading={send.isPending} onClick={() => send.mutate()}>
          发送测试告警
        </Button>
      }
    />
  )
}
