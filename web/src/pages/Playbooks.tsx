import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Table, Tag, Button, Space, Card, Modal, Form, Input, InputNumber, Select, Switch, Popconfirm, Empty,
} from 'antd'
import { PlusOutlined } from '@ant-design/icons'
import { api, type Playbook } from '../api'

const levelColor: Record<string, string> = { L0: 'green', L1: 'orange', L2: 'red' }

interface PBForm {
  name: string
  level: string
  commandTemplate: string
  rollbackCommand?: string
  blastRadiusFactor?: number
  dryRunFirst?: boolean
  enabled?: boolean
}

export default function Playbooks() {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<Playbook | null>(null)
  const [form] = Form.useForm<PBForm>()

  const { data, isLoading } = useQuery({
    queryKey: ['playbooks'],
    queryFn: () => api.playbooks(),
  })

  const invalidate = () => qc.invalidateQueries({ queryKey: ['playbooks'] })

  const save = useMutation({
    mutationFn: (values: PBForm) =>
      editing
        ? api.updatePlaybook(editing.id, values)
        : api.createPlaybook(values),
    onSuccess: () => {
      setOpen(false)
      setEditing(null)
      form.resetFields()
      invalidate()
    },
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.deletePlaybook(id),
    onSuccess: invalidate,
  })

  const openCreate = () => {
    setEditing(null)
    form.setFieldsValue({ level: 'L1', dryRunFirst: true, enabled: true, blastRadiusFactor: 0.1 })
    setOpen(true)
  }

  const openEdit = (pb: Playbook) => {
    setEditing(pb)
    form.setFieldsValue({
      name: pb.name, level: pb.level, commandTemplate: pb.commandTemplate,
      rollbackCommand: pb.rollbackCommand, blastRadiusFactor: pb.blastRadiusFactor,
      dryRunFirst: pb.dryRunFirst, enabled: pb.enabled,
    })
    setOpen(true)
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="middle">
      <Card
        size="small"
        title={`自愈剧本（${data?.total ?? 0}）`}
        extra={<Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>新建剧本</Button>}
      >
        <Table<Playbook>
          rowKey="id"
          size="small"
          loading={isLoading}
          dataSource={data?.items ?? []}
          pagination={false}
          locale={{ emptyText: <Empty description="还没有剧本。RCA 建议动作按剧本名匹配，可在上方创建（如 rollback / scale_up），缺省时自愈动作需人工确认" /> }}
          columns={[
            {
              title: '名称', dataIndex: 'name',
              render: (v: string, r) => (
                <Space>
                  <span>{v}</span>
                  <Tag color="blue" style={{ fontSize: 11 }}>{r.level === 'L0' ? '自动' : r.level === 'L1' ? 'oncall' : 'tech-lead'}</Tag>
                </Space>
              ),
            },
            {
              title: '级别', dataIndex: 'level',
              render: (v: string) => <Tag color={levelColor[v]}>{v}</Tag>,
            },
            {
              title: '爆炸半径', dataIndex: 'blastRadiusFactor',
              render: (v: number) => `${(v * 100).toFixed(0)}%`,
            },
            { title: '执行命令', dataIndex: 'commandTemplate', ellipsis: true },
            { title: '回滚命令', dataIndex: 'rollbackCommand', ellipsis: true },
            {
              title: 'DryRun', dataIndex: 'dryRunFirst',
              render: (v: boolean) => (v ? <Tag color="blue">是</Tag> : <Tag>否</Tag>),
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
                  <Popconfirm title="确认删除该剧本？" onConfirm={() => remove.mutate(r.id)}>
                    <Button size="small" danger>删除</Button>
                  </Popconfirm>
                </Space>
              ),
            },
          ]}
        />
        <TypographyNote />
      </Card>

      <Modal
        title={editing ? '编辑剧本' : '新建剧本'}
        open={open}
        onOk={() => form.submit()}
        confirmLoading={save.isPending}
        onCancel={() => setOpen(false)}
        destroyOnClose
      >
        <Form<PBForm> form={form} layout="vertical" onFinish={(v) => save.mutate(v)}>
          <Form.Item name="name" label="名称（对齐 RCA 建议动作，如 rollback/scale_up）" rules={[{ required: true }]}>
            <Input disabled={!!editing} placeholder="rollback" />
          </Form.Item>
          <Form.Item name="level" label="审批级别" rules={[{ required: true }]}>
            <Select
              options={[
                { value: 'L0', label: 'L0 — 自动执行（需 dryRun=false）' },
                { value: 'L1', label: 'L1 — oncall 确认' },
                { value: 'L2', label: 'L2 — tech-lead 人工审批' },
              ]}
            />
          </Form.Item>
          <Form.Item name="commandTemplate" label="执行命令模板" rules={[{ required: true }]}>
            <Input placeholder="kubectl rollout restart deployment/{{.service}}" />
          </Form.Item>
          <Form.Item name="rollbackCommand" label="回滚命令模板">
            <Input placeholder="kubectl rollout status deployment/{{.service}}" />
          </Form.Item>
          <Form.Item name="blastRadiusFactor" label="爆炸半径（0~1）">
            <InputNumber min={0} max={1} step={0.01} style={{ width: '100%' }} />
          </Form.Item>
          <Space>
            <Form.Item name="dryRunFirst" label="强制 DryRun" valuePropName="checked">
              <Switch />
            </Form.Item>
            <Form.Item name="enabled" label="启用" valuePropName="checked">
              <Switch />
            </Form.Item>
          </Space>
        </Form>
      </Modal>
    </Space>
  )
}

function TypographyNote() {
  return <p style={{ color: '#999', fontSize: 12, marginTop: 8 }}>
    安全护栏：自愈动作依次经过 dry-run → 爆炸半径（超 0.2 强制降级 L2）→ 熔断器 → 分级审批，全部通过才执行。
  </p>
}
