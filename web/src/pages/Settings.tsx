import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Alert, Button, Card, Descriptions, Form, Input, InputNumber, Modal, Popconfirm, Select, Space,
  Switch, Table, Tabs, Tag, Typography, message,
} from 'antd'
import { CloudUploadOutlined, KeyOutlined, LinkOutlined, ReloadOutlined } from '@ant-design/icons'
import { api } from '../api'
import type { BackupRun, APIKeyItem, ExternalSystem } from '../api'

// Settings 设置页（KEEPON M1）：数据备份 + 访问密钥 两个 Tab。
// 备份恢复不做网页按钮——恢复是高风险操作，仅通过 scripts/restore.sh 执行。

const statusColor: Record<string, string> = { running: 'processing', success: 'success', failed: 'error' }
const verifyColor: Record<string, string> = { skipped: 'default', passed: 'success', failed: 'error' }

function BackupTab() {
  const [msg, ctx] = message.useMessage()
  const qc = useQueryClient()
  const { data, isLoading, error } = useQuery({ queryKey: ['backups'], queryFn: api.backups, refetchInterval: 5000 })
  const disabled = !data // 后端未装配（AIOPS_BACKUPENABLED=false）时禁用操作

  const trigger = useMutation({
    mutationFn: api.triggerBackup,
    onSuccess: () => {
      msg.success('备份任务已启动，稍后刷新可见结果')
      qc.invalidateQueries({ queryKey: ['backups'] })
    },
    onError: (e: Error) => msg.error(`触发失败：${e.message}`),
  })

  const latest: BackupRun | undefined = data?.items?.[0]

  if (error) {
    return (
      <Alert
        type="info" showIcon
        message="备份未启用"
        description={`启动后端时设置 AIOPS_BACKUPENABLED=true 即可开启定时备份（当前返回：${error.message}）。`}
      />
    )
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      {latest && (
        <Card size="small" title="最近一次备份">
          <Descriptions column={2} size="small">
            <Descriptions.Item label="状态">
              <Tag color={statusColor[latest.status]}>{latest.status}</Tag>
            </Descriptions.Item>
            <Descriptions.Item label="触发">
              <Tag>{latest.trigger}</Tag>
            </Descriptions.Item>
            <Descriptions.Item label="开始时间">{latest.startedAt}</Descriptions.Item>
            <Descriptions.Item label="耗时">
              {latest.finishedAt ? `${Math.max(0, Math.round((new Date(latest.finishedAt).getTime() - new Date(latest.startedAt).getTime()) / 1000))}s` : '执行中…'}
            </Descriptions.Item>
            <Descriptions.Item label="文件">
              {latest.filePath ? `${latest.filePath}（${fmtSize(latest.fileSize ?? 0)}）` : '-'}
            </Descriptions.Item>
            <Descriptions.Item label="校验">
              <Tag color={verifyColor[latest.verifyStatus ?? 'skipped']}>{latest.verifyStatus ?? 'skipped'}</Tag>
              {latest.verifyDetail && latest.verifyStatus !== 'passed' && (
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>{latest.verifyDetail}</Typography.Text>
              )}
            </Descriptions.Item>
            {latest.sha256 && <Descriptions.Item label="SHA256"><Typography.Text code style={{ fontSize: 12 }}>{latest.sha256.slice(0, 16)}…</Typography.Text></Descriptions.Item>}
            {latest.mirrorPath && (
              <Descriptions.Item label="镜像">
                {latest.mirrorPath}{latest.mirrorEncrypted ? '（已加密 .aes）' : ''}
              </Descriptions.Item>
            )}
            {latest.errorDetail && (
              <Descriptions.Item label="失败原因" span={2}>
                <Typography.Text type="danger" style={{ fontSize: 12 }}>{latest.errorDetail}</Typography.Text>
              </Descriptions.Item>
            )}
          </Descriptions>
        </Card>
      )}
      <Card size="small" title="备份记录">
        <Space style={{ marginBottom: 12 }}>
          <Button type="primary" icon={<CloudUploadOutlined />} loading={trigger.isPending} disabled={disabled} onClick={() => trigger.mutate()}>
            立即备份
          </Button>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            定时备份（AIOPS_BACKUPENABLED）+ 手动触发；恢复仅通过 scripts/restore.sh（有确认保护）
          </Typography.Text>
        </Space>
        <Table<BackupRun>
          rowKey="id"
          size="small"
          loading={isLoading}
          dataSource={data?.items ?? []}
          pagination={false}
          columns={[
            { title: '状态', dataIndex: 'status', width: 90, render: (s: string) => <Tag color={statusColor[s]}>{s}</Tag> },
            { title: '触发', dataIndex: 'trigger', width: 90 },
            { title: '开始时间', dataIndex: 'startedAt', width: 190 },
            { title: '大小', dataIndex: 'fileSize', width: 90, render: (n?: number) => (n ? fmtSize(n) : '-') },
            {
              title: '校验', dataIndex: 'verifyStatus', width: 90,
              render: (s?: string) => <Tag color={verifyColor[s ?? 'skipped']}>{s ?? 'skipped'}</Tag>,
            },
            { title: '镜像', dataIndex: 'mirrorPath', render: (p?: string) => p ?? '-' },
          ]}
        />
      </Card>
      {ctx}
    </Space>
  )
}

function fmtSize(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(0)} KB`
  return `${n} B`
}

function KeysTab() {
  const [msg, ctx] = message.useMessage()
  const qc = useQueryClient()
  const [newKey, setNewKey] = useState<{ apiKey: string; tenantId: string } | null>(null)
  const [name, setName] = useState('')

  const { data: status } = useQuery({ queryKey: ['auth-status'], queryFn: api.authStatus, staleTime: 60_000 })
  const { data, isLoading } = useQuery({ queryKey: ['api-keys'], queryFn: api.apiKeys })

  const create = useMutation({
    mutationFn: () => api.createAPIKey(name.trim() || 'script'),
    onSuccess: (r) => {
      setNewKey(r)
      setName('')
      qc.invalidateQueries({ queryKey: ['api-keys'] })
    },
    onError: (e: Error) => msg.error(`创建失败：${e.message}`),
  })
  const del = useMutation({
    mutationFn: (id: string) => api.deleteAPIKey(id),
    onSuccess: () => {
      msg.success('已撤销，立即失效')
      qc.invalidateQueries({ queryKey: ['api-keys'] })
    },
    onError: (e: Error) => msg.error(`撤销失败：${e.message}`),
  })
  const regen = useMutation({
    mutationFn: api.regenerateOwner,
    onSuccess: (r) => {
      setNewKey(r)
      qc.invalidateQueries({ queryKey: ['api-keys'] })
    },
    onError: (e: Error) => msg.error(`重新生成失败：${e.message}`),
  })

  if (status?.mode !== 'apikey') {
    return (
      <Alert
        type="info" showIcon
        message="当前为 noauth 模式，无需访问密钥"
        description="设置 AIOPS_AUTHTYPE=apikey 并重启后端后，此处可管理访问密钥（首次启动会自动生成 owner key）。"
      />
    )
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Card size="small" title="访问密钥">
        <Typography.Paragraph type="secondary" style={{ fontSize: 12, marginBottom: 12 }}>
          REST 请求携带 <Typography.Text code>X-API-Key</Typography.Text>，WebSocket 使用短期 ticket 握手。
          凭据仅在浏览器会话内保存；密钥明文只在创建时显示一次。
        </Typography.Paragraph>
        <Space style={{ marginBottom: 12 }}>
          <Input
            style={{ width: 200 }}
            placeholder="密钥用途（如 my-script）"
            value={name}
            onChange={(e) => setName(e.target.value)}
            onPressEnter={() => create.mutate()}
          />
          <Button type="primary" icon={<KeyOutlined />} loading={create.isPending} onClick={() => create.mutate()}>
            新建密钥
          </Button>
          <Button icon={<ReloadOutlined />} loading={regen.isPending} onClick={() => regen.mutate()}>
            重新生成 owner key
          </Button>
        </Space>
        <Table<APIKeyItem>
          rowKey="id"
          size="small"
          loading={isLoading}
          dataSource={data?.items ?? []}
          pagination={false}
          columns={[
            { title: '名称', dataIndex: 'name', render: (n: string, r) => (<>{n}{r.isOwner && <Tag color="gold" style={{ marginLeft: 8 }}>owner</Tag>}</>) },
            { title: '最后使用', dataIndex: 'lastUsedAt', render: (v?: string) => v ?? '从未' },
            { title: '创建时间', dataIndex: 'createdAt' },
            {
              title: '操作', width: 90,
              render: (_: unknown, r) => (
                <Popconfirm title="撤销后立即失效，确认？" onConfirm={() => del.mutate(r.id)}>
                  <Button size="small" danger>撤销</Button>
                </Popconfirm>
              ),
            },
          ]}
        />
      </Card>
      <Modal
        open={!!newKey}
        title="密钥已生成（仅显示这一次）"
        onCancel={() => setNewKey(null)}
        footer={[
          <Button key="copy" type="primary" onClick={() => {
            if (newKey) {
              void navigator.clipboard.writeText(newKey.apiKey)
              msg.success('已复制')
            }
          }}>
            复制并关闭
          </Button>,
        ]}
      >
        <Alert type="warning" showIcon style={{ marginBottom: 12 }}
          message="关闭后无法再次查看明文。请立即保存到密码管理器或环境变量。" />
        <Typography.Paragraph>
          <Typography.Text code copyable>{newKey?.apiKey}</Typography.Text>
        </Typography.Paragraph>
      </Modal>
      {ctx}
    </Space>
  )
}

// ExternalSystemsTab 外部可观测系统管理（KEEPON 6.2）：侧边栏快捷跳转。
// 保存后立即生效（DB 为准，无需重启）；仅允许 http/https URL。
function ExternalSystemsTab() {
  const [msg, ctx] = message.useMessage()
  const qc = useQueryClient()
  const [editing, setEditing] = useState<ExternalSystem | null>(null)
  const [modalOpen, setModalOpen] = useState(false)
  const [form] = Form.useForm()

  const { data, isLoading } = useQuery({ queryKey: ['external-systems'], queryFn: api.externalSystems })

  const invalidate = () => qc.invalidateQueries({ queryKey: ['external-systems'] })
  const save = useMutation({
    mutationFn: (values: Partial<ExternalSystem>) =>
      editing ? api.updateExternalSystem(editing.id, values) : api.createExternalSystem(values),
    onSuccess: () => {
      msg.success('已保存，侧边栏立即生效')
      setModalOpen(false)
      setEditing(null)
      invalidate()
    },
    onError: (e: Error) => msg.error(`保存失败：${e.message}`),
  })
  const del = useMutation({
    mutationFn: (id: string) => api.deleteExternalSystem(id),
    onSuccess: () => {
      msg.success('已删除')
      invalidate()
    },
    onError: (e: Error) => msg.error(`删除失败：${e.message}`),
  })

  const typeColor: Record<string, string> = { grafana: 'orange', prometheus: 'volcano', logs: 'blue', other: 'default' }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Card size="small" title="外部系统">
        <Typography.Paragraph type="secondary" style={{ fontSize: 12, marginBottom: 12 }}>
          配置后出现在侧边栏「外部系统」分组，点击以新标签页打开。环境变量{' '}
          <Typography.Text code>AIOPS_EXTERNALSYSTEMS</Typography.Text>（JSON 数组）仅首次启动播种，之后以这里为准。
        </Typography.Paragraph>
        <Space style={{ marginBottom: 12 }}>
          <Button
            type="primary" icon={<LinkOutlined />}
            onClick={() => {
              setEditing(null)
              form.resetFields()
              setModalOpen(true)
            }}
          >
            新增系统
          </Button>
        </Space>
        <Table<ExternalSystem>
          rowKey="id"
          size="small"
          loading={isLoading}
          dataSource={data?.items ?? []}
          pagination={false}
          columns={[
            { title: '名称', dataIndex: 'name', width: 160 },
            {
              title: '类型', dataIndex: 'type', width: 110,
              render: (t: string) => <Tag color={typeColor[t]}>{t}</Tag>,
            },
            { title: '地址', dataIndex: 'url', render: (u: string) => <Typography.Text code style={{ fontSize: 12 }}>{u}</Typography.Text> },
            { title: '排序', dataIndex: 'sortOrder', width: 70 },
            {
              title: '启用', dataIndex: 'enabled', width: 80,
              render: (e: boolean) => (e ? <Tag color="green">启用</Tag> : <Tag>停用</Tag>),
            },
            {
              title: '操作', width: 130,
              render: (_: unknown, r) => (
                <Space size={4}>
                  <Button
                    size="small"
                    onClick={() => {
                      setEditing(r)
                      form.setFieldsValue(r)
                      setModalOpen(true)
                    }}
                  >
                    编辑
                  </Button>
                  <Popconfirm title={`删除「${r.name}」？`} onConfirm={() => del.mutate(r.id)}>
                    <Button size="small" danger>删除</Button>
                  </Popconfirm>
                </Space>
              ),
            },
          ]}
        />
      </Card>
      <Modal
        open={modalOpen}
        title={editing ? `编辑 ${editing.name}` : '新增外部系统'}
        onCancel={() => setModalOpen(false)}
        onOk={() => form.submit()}
        confirmLoading={save.isPending}
        destroyOnClose
      >
        <Form form={form} layout="vertical" onFinish={(v) => save.mutate(v)} initialValues={{ type: 'grafana', enabled: true, sortOrder: 0 }}>
          <Form.Item name="name" label="名称" rules={[{ required: true, message: '请输入名称' }]}>
            <Input placeholder="如 Grafana / Prometheus / 日志系统" />
          </Form.Item>
          <Form.Item
            name="url" label="地址"
            rules={[
              { required: true, message: '请输入 URL' },
              { pattern: /^https?:\/\/.+/i, message: '仅支持 http/https 地址' },
            ]}
          >
            <Input placeholder="https://grafana.example.com" />
          </Form.Item>
          <Form.Item name="type" label="类型" rules={[{ required: true }]}>
            <Select
              options={[
                { value: 'grafana', label: 'Grafana' },
                { value: 'prometheus', label: 'Prometheus' },
                { value: 'logs', label: '日志' },
                { value: 'other', label: '其他' },
              ]}
            />
          </Form.Item>
          <Space size="large">
            <Form.Item name="enabled" label="启用" valuePropName="checked">
              <Switch />
            </Form.Item>
            <Form.Item name="sortOrder" label="排序（小在前）">
              <InputNumber min={0} max={99} />
            </Form.Item>
          </Space>
        </Form>
      </Modal>
      {ctx}
    </Space>
  )
}

export default function Settings() {
  return (
    <Tabs
      defaultActiveKey="backup"
      items={[
        { key: 'backup', label: '数据备份', children: <BackupTab /> },
        { key: 'keys', label: '访问密钥', children: <KeysTab /> },
        { key: 'external', label: '外部系统', children: <ExternalSystemsTab /> },
      ]}
    />
  )
}
