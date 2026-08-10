import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Table, Tag, Button, Space, Card, Modal, Form, Input, Switch, Popconfirm, message, Empty } from 'antd'
import { PlusOutlined, PlayCircleOutlined } from '@ant-design/icons'
import { api, type Workflow, type WorkflowRun } from '../api'

const DEFAULT_YAML = `workflow:
  id: my-workflow
  name: 我的工作流
  description: 监听 pay 服务中危以上告警并接入 Agent 编排
  triggers:
    - type: alert
      filters:
        - key: service
          value: pay
        - key: severity
          value: ">=medium"
  steps:
    - name: enrich
      provider:
        type: log
        with:
          message: "工作流命中 {{.alert.name}}，即将接入 Agent 编排"
  actions:
    - name: run-agent
      provider:
        type: agent
`

const statusColor: Record<string, string> = {
  running: 'processing',
  succeeded: 'success',
  failed: 'error',
}

export default function Workflows() {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<Workflow | null>(null)
  const [form] = Form.useForm<{ name: string; description: string; yaml: string; enabled: boolean }>()

  const { data, isLoading } = useQuery({
    queryKey: ['workflows'],
    queryFn: () => api.workflows(),
  })

  const { data: runs } = useQuery({
    queryKey: ['workflow-runs'],
    queryFn: () => api.workflowRuns({ limit: '10' }),
  })

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['workflows'] })
    qc.invalidateQueries({ queryKey: ['workflow-runs'] })
  }

  const save = useMutation({
    mutationFn: (values: { name: string; description: string; yaml: string; enabled: boolean }) =>
      editing
        ? api.updateWorkflow(editing.id, { yaml: values.yaml, enabled: values.enabled })
        : api.createWorkflow(values),
    onSuccess: () => {
      setOpen(false)
      setEditing(null)
      form.resetFields()
      invalidate()
    },
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteWorkflow(id),
    onSuccess: invalidate,
  })

  const run = useMutation({
    mutationFn: (id: string) => api.runWorkflow(id),
    onSuccess: () => {
      message.success('已触发手动运行，请查看最近运行')
      invalidate()
    },
    onError: (err: Error) => message.error(`触发失败：${err.message}`),
  })

  const openCreate = () => {
    setEditing(null)
    form.setFieldsValue({ name: '', description: '', yaml: DEFAULT_YAML, enabled: true })
    setOpen(true)
  }

  const openEdit = (w: Workflow) => {
    setEditing(w)
    form.setFieldsValue({ name: w.name, description: w.description, yaml: w.yaml, enabled: w.enabled })
    setOpen(true)
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="middle">
      <Card
        size="small"
        title={`工作流（${data?.total ?? 0}）`}
        extra={<Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>新建工作流</Button>}
      >
        <Table<Workflow>
          rowKey="id"
          size="small"
          loading={isLoading}
          dataSource={data?.items ?? []}
          pagination={false}
          locale={{ emptyText: <Empty description="还没有工作流。点击「新建工作流」用 YAML 定义告警触发的自动化处置（模板已预填）" /> }}
          columns={[
            {
              title: '名称', dataIndex: 'name',
              render: (v: string, r) => (
                <Space>
                  <span>{v}</span>
                  {r.enabled ? <Tag color="green">启用</Tag> : <Tag color="red">停用</Tag>}
                </Space>
              ),
            },
            { title: '描述', dataIndex: 'description', ellipsis: true, render: (v: string) => v || '—' },
            { title: 'ID', dataIndex: 'id', ellipsis: true, width: 220, render: (v: string) => <code style={{ fontSize: 12 }}>{v.slice(0, 8)}…</code> },
            { title: '更新', dataIndex: 'updatedAt', width: 150, render: (v: string) => new Date(v).toLocaleString() },
            {
              title: '操作', key: 'op', width: 190,
              render: (_, r) => (
                <Space>
                  <Button
                    size="small"
                    type="primary"
                    ghost
                    icon={<PlayCircleOutlined />}
                    loading={run.isPending && run.variables === r.id}
                    onClick={() => run.mutate(r.id)}
                  >
                    运行
                  </Button>
                  <Button size="small" onClick={() => openEdit(r)}>编辑</Button>
                  <Popconfirm title="确认删除该工作流？" onConfirm={() => remove.mutate(r.id)}>
                    <Button size="small" danger>删除</Button>
                  </Popconfirm>
                </Space>
              ),
            },
          ]}
        />
        <p style={{ color: '#999', fontSize: 12, marginTop: 8 }}>
          YAML 工作流：triggers（alert 告警命中 / interval 定时 / manual 手动「运行」按钮）时异步执行
          steps/actions（provider: log | http | agent；http 支持 method/headers/auth）。同一工作流对同一告警只执行一次
          （workflow_runs 唯一键防并发）。
        </p>
      </Card>

      <Card size="small" title={`最近运行（${runs?.total ?? 0}）`}>
        <Table<WorkflowRun>
          rowKey="id"
          size="small"
          dataSource={runs?.items ?? []}
          pagination={false}
          columns={[
            { title: '工作流', dataIndex: 'workflowId', render: (v: string) => <code style={{ fontSize: 12 }}>{v.slice(0, 8)}…</code> },
            {
              title: '触发', dataIndex: 'trigger',
              render: (v: string) => (v ? <Tag color={v === 'manual' ? 'blue' : v === 'interval' ? 'purple' : 'default'}>{v}</Tag> : '—'),
            },
            {
              title: '状态', dataIndex: 'status',
              render: (v: string) => <Tag color={statusColor[v] || 'default'}>{v}</Tag>,
            },
            { title: '开始', dataIndex: 'startedAt', render: (v: string) => new Date(v).toLocaleString() },
            {
              title: '结果', dataIndex: 'result',
              render: (v: Record<string, any>) => (v ? <code style={{ fontSize: 11 }}>{JSON.stringify(v)}</code> : '—'),
            },
          ]}
        />
      </Card>

      <Modal
        title={editing ? '编辑工作流' : '新建工作流'}
        open={open}
        onOk={() => form.submit()}
        confirmLoading={save.isPending}
        onCancel={() => setOpen(false)}
        destroyOnClose
        width={720}
      >
        <Form<{ name: string; description: string; yaml: string; enabled: boolean }>
          form={form}
          layout="vertical"
          onFinish={(v) => save.mutate(v)}
        >
          <Space style={{ width: '100%' }} align="baseline">
            <Form.Item name="name" label="名称" rules={[{ required: true }]} style={{ flex: 2 }}>
              <Input placeholder="如：pay 告警接入 Agent" disabled={!!editing} />
            </Form.Item>
            <Form.Item name="enabled" label="启用" valuePropName="checked" style={{ flex: 1 }}>
              <Switch />
            </Form.Item>
          </Space>
          <Form.Item name="description" label="描述">
            <Input placeholder="工作流说明（可选）" disabled={!!editing} />
          </Form.Item>
          <Form.Item name="yaml" label="YAML 定义" rules={[{ required: true, message: '请填写 YAML' }]}>
            <Input.TextArea
              rows={16}
              style={{ fontFamily: 'monospace', fontSize: 12 }}
              spellCheck={false}
            />
          </Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
