import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Table, Tag, Button, Space, Card, Modal, Form, Input, Select, Switch, Popconfirm, DatePicker, Empty,
} from 'antd'
import { PlusOutlined } from '@ant-design/icons'
import type { Dayjs } from 'dayjs'
import dayjs from 'dayjs'
import { api, type MaintenanceWindow } from '../api'

interface MWForm {
  name: string
  fingerprint?: string
  service?: string
  severity?: string
  startsAt: Dayjs
  endsAt: Dayjs
  reason?: string
  enabled?: boolean
}

const fmt = (t: string) => dayjs(t).format('YYYY-MM-DD HH:mm')

function isActive(w: MaintenanceWindow) {
  const now = dayjs()
  return w.enabled && now.isAfter(dayjs(w.startsAt)) && now.isBefore(dayjs(w.endsAt))
}

export default function MaintenanceWindows() {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<MaintenanceWindow | null>(null)
  const [form] = Form.useForm<MWForm>()

  const { data, isLoading } = useQuery({
    queryKey: ['maintenance-windows'],
    queryFn: () => api.maintenanceWindows(),
  })

  const invalidate = () => qc.invalidateQueries({ queryKey: ['maintenance-windows'] })

  const save = useMutation({
    mutationFn: (values: MWForm) => {
      const payload = {
        name: values.name,
        fingerprint: values.fingerprint ?? '',
        service: values.service ?? '',
        severity: values.severity ?? '',
        startsAt: values.startsAt.toISOString(),
        endsAt: values.endsAt.toISOString(),
        reason: values.reason ?? '',
        enabled: values.enabled ?? false,
      }
      return editing
        ? api.updateMaintenanceWindow(editing.id, payload)
        : api.createMaintenanceWindow(payload)
    },
    onSuccess: () => {
      setOpen(false)
      setEditing(null)
      form.resetFields()
      invalidate()
    },
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteMaintenanceWindow(id),
    onSuccess: invalidate,
  })

  const openCreate = () => {
    setEditing(null)
    const now = dayjs()
    form.setFieldsValue({
      enabled: true,
      startsAt: now.startOf('hour'),
      endsAt: now.add(2, 'hour'),
    })
    setOpen(true)
  }

  const openEdit = (w: MaintenanceWindow) => {
    setEditing(w)
    form.setFieldsValue({
      name: w.name, fingerprint: w.fingerprint, service: w.service, severity: w.severity,
      startsAt: dayjs(w.startsAt), endsAt: dayjs(w.endsAt),
      reason: w.reason, enabled: w.enabled,
    })
    setOpen(true)
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="middle">
      <Card
        size="small"
        title={`维护窗口（${data?.total ?? 0}）`}
        extra={<Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>新建窗口</Button>}
      >
        <Table<MaintenanceWindow>
          rowKey="id"
          size="small"
          loading={isLoading}
          dataSource={data?.items ?? []}
          pagination={false}
          locale={{ emptyText: <Empty description="还没有维护窗口。创建窗口后，匹配的告警在窗口期内以 suppressed 状态入库，不触发事故与通知" /> }}
          columns={[
            {
              title: '名称', dataIndex: 'name',
              render: (v: string, r) => (
                <Space>
                  <span>{v}</span>
                  {isActive(r) ? <Tag color="green">生效中</Tag> : r.enabled ? <Tag>已排程</Tag> : <Tag color="red">已停用</Tag>}
                </Space>
              ),
            },
            {
              title: '时间', key: 'time',
              render: (_, r) => `${fmt(r.startsAt)} → ${fmt(r.endsAt)}`,
            },
            {
              title: '匹配条件', key: 'match',
              render: (_, r) => (
                <Space size={4} wrap>
                  {r.fingerprint && <Tag color="blue">fp: {r.fingerprint}</Tag>}
                  {r.service && <Tag color="geekblue">svc: {r.service}</Tag>}
                  {r.severity && <Tag color="purple">≤ {r.severity}</Tag>}
                  {!r.fingerprint && !r.service && !r.severity && <span style={{ color: '#999' }}>—</span>}
                </Space>
              ),
            },
            {
              title: '原因', dataIndex: 'reason', ellipsis: true,
              render: (v: string) => v || <span style={{ color: '#999' }}>—</span>,
            },
            {
              title: '启用', dataIndex: 'enabled',
              render: (v: boolean) => (v ? <Tag color="green">是</Tag> : <Tag color="red">否</Tag>),
            },
            {
              title: '操作', key: 'op',
              render: (_, r) => (
                <Space>
                  <Button size="small" onClick={() => openEdit(r)}>编辑</Button>
                  <Popconfirm title="确认删除该维护窗口？" onConfirm={() => remove.mutate(r.id)}>
                    <Button size="small" danger>删除</Button>
                  </Popconfirm>
                </Space>
              ),
            },
          ]}
        />
        <p style={{ color: '#999', fontSize: 12, marginTop: 8 }}>
          热路径过滤：窗口生效期间，匹配的 firing 告警以 <Tag style={{ fontSize: 11 }}>suppressed</Tag>{' '}
          状态入库（跳过规则 / Incident / Agent），恢复类告警不受影响。
        </p>
      </Card>

      <Modal
        title={editing ? '编辑维护窗口' : '新建维护窗口'}
        open={open}
        onOk={() => form.submit()}
        confirmLoading={save.isPending}
        onCancel={() => setOpen(false)}
        destroyOnClose
      >
        <Form<MWForm> form={form} layout="vertical" onFinish={(v) => save.mutate(v)}>
          <Form.Item name="name" label="窗口名称" rules={[{ required: true }]}>
            <Input placeholder="如：支付服务周三 02:00 维护" />
          </Form.Item>
          <Form.Item label="匹配条件" style={{ marginBottom: 0 }}>
            <Space direction="vertical" style={{ width: '100%' }}>
              <Form.Item name="fingerprint" label="精确指纹（alert.fingerprint，可选）">
                <Input placeholder="prometheus:pay-slow" />
              </Form.Item>
              <Form.Item name="service" label="服务名（labels.service，可选）">
                <Input placeholder="pay" />
              </Form.Item>
              <Form.Item name="severity" label="严重度上限（覆盖 ≤ 该级别告警，可选）">
                <Select
                  allowClear
                  placeholder="不限制"
                  options={[
                    { value: 'critical', label: 'critical — 覆盖全部' },
                    { value: 'high', label: 'high' },
                    { value: 'medium', label: 'medium' },
                    { value: 'low', label: 'low' },
                    { value: 'info', label: 'info — 仅 info' },
                  ]}
                />
              </Form.Item>
            </Space>
          </Form.Item>
          <Space style={{ width: '100%' }}>
            <Form.Item name="startsAt" label="开始时间" rules={[{ required: true }]} style={{ flex: 1 }}>
              <DatePicker showTime style={{ width: '100%' }} />
            </Form.Item>
            <Form.Item name="endsAt" label="结束时间" rules={[{ required: true }]} style={{ flex: 1 }}>
              <DatePicker showTime style={{ width: '100%' }} />
            </Form.Item>
          </Space>
          <Form.Item name="reason" label="维护原因">
            <Input.TextArea rows={2} placeholder="上线升级 / 数据库扩容等" />
          </Form.Item>
          <Form.Item name="enabled" label="启用" valuePropName="checked">
            <Switch />
          </Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
