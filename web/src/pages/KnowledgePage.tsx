import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Alert, Button, Card, Descriptions, Input, Modal, Popconfirm, Select, Space, Spin, Switch, Table, Tabs,
  Tag, Typography, Upload, message,
} from 'antd'
import { CloudUploadOutlined, DeleteOutlined, PlayCircleOutlined } from '@ant-design/icons'
import { api } from '../api'
import type { GoldenQuery, SourcePreview, SourceRow } from '../api'

// KnowledgePage 知识库（KEEPON M3）：个人知识源管理 + 检索 golden set 评测 + 检索状态。
// 全部端点走 AIOPS_RAGADMINAPIS 管理开关（403 时顶部警告，仿 ModelConfigs）。

const srcStatusColor: Record<string, string> = {
  draft: 'default', published: 'green', deleted: 'red', quarantined: 'orange',
}
const trustColor: Record<string, string> = { T0: 'default', T1: 'blue', T2: 'orange', T3: 'green' }

function fmtTime(v?: string): string {
  if (!v) return '-'
  return new Date(v).toLocaleString('zh-CN', { hour12: false })
}

// ---- Tab1 知识源 ----

function SourcesTab() {
  const qc = useQueryClient()
  const [msg, ctx] = message.useMessage()
  const [preview, setPreview] = useState<SourcePreview | null>(null)
  const [pendingContent, setPendingContent] = useState<{ name: string; content: string } | null>(null)
  const [editTarget, setEditTarget] = useState<SourceRow | null>(null)
  const [editText, setEditText] = useState('')
  const [editName, setEditName] = useState('')

  const { data, isLoading, error } = useQuery({ queryKey: ['sources'], queryFn: api.sources })
  const forbidden = error?.message.includes('403')

  const create = useMutation({
    mutationFn: () => api.createSource(pendingContent!.name, pendingContent!.content),
    onSuccess: (src) => {
      msg.success(`草稿「${src.name}」已创建，发布后进入知识库索引`)
      setPreview(null)
      setPendingContent(null)
      qc.invalidateQueries({ queryKey: ['sources'] })
    },
    onError: (e: Error) => msg.error(`创建失败：${e.message}`),
  })
  const publish = useMutation({
    mutationFn: (id: string) => api.publishSource(id),
    onSuccess: () => {
      msg.success('已发布，索引中…')
      qc.invalidateQueries({ queryKey: ['sources'] })
    },
    onError: (e: Error) => msg.error(`发布失败：${e.message}`),
  })
  const update = useMutation({
    mutationFn: ({ id, name, content }: { id: string; name: string; content: string }) =>
      api.updateSource(id, name, content),
    onSuccess: (src) => {
      msg.success(src.version > 0 ? `已保存并发布新版本 v${src.version}` : '草稿已更新')
      setEditTarget(null)
      qc.invalidateQueries({ queryKey: ['sources'] })
    },
    onError: (e: Error) => msg.error(`保存失败：${e.message}`),
  })
  const del = useMutation({
    mutationFn: (id: string) => api.deleteSource(id),
    onSuccess: () => {
      msg.success('已删除，知识文档同步撤销')
      qc.invalidateQueries({ queryKey: ['sources'] })
    },
    onError: (e: Error) => msg.error(`删除失败：${e.message}`),
  })

  // 拖拽/选择文件 → 读内容 → 预览（免 multipart：前端读文件走 JSON API）
  const onFile = async (file: File) => {
    if (file.size > 1 << 20) {
      msg.error('文件超过 1MB 上限')
      return false
    }
    const content = await file.text()
    try {
      const p = await api.sourcePreview(file.name, content)
      setPendingContent({ name: file.name, content })
      setPreview(p)
    } catch (e) {
      msg.error(`预览失败：${(e as Error).message}`)
    }
    return false // 拦截 Upload 自动上传
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      {forbidden && (
        <Alert type="warning" showIcon message="管理权限不足"
          description="知识库管理需要 AIOPS_RAGADMINAPIS=true（apikey 模式还需 AIOPS_RAGADMINTENANTS 白名单）与 AIOPS_RAGENABLED=true。" />
      )}
      {!forbidden && !isLoading && !data && (
        <Alert type="info" showIcon message="RAG 未启用"
          description="启动后端时设置 AIOPS_RAGENABLED=true AIOPS_RAGMODE=readonly 后可使用知识库管理。" />
      )}
      <Card size="small" title="个人知识源（runbook / SOP）">
        <Upload.Dragger
          accept=".md,.markdown,.txt"
          multiple={false}
          showUploadList={false}
          beforeUpload={(f) => { void onFile(f); return false }}
          disabled={forbidden || !data}
        >
          <p className="ant-upload-drag-icon"><CloudUploadOutlined /></p>
          <p className="ant-upload-text">拖拽 .md 文件到这里，或点击选择</p>
          <p className="ant-upload-hint">支持 front matter（title/service/tags/updated_at）；发布前预览解析与脱敏检查</p>
        </Upload.Dragger>
        <Table<SourceRow>
          rowKey="id"
          size="small"
          style={{ marginTop: 12 }}
          loading={isLoading}
          dataSource={data?.items ?? []}
          pagination={false}
          columns={[
            { title: '名称', dataIndex: 'name' },
            { title: '服务', dataIndex: 'service', width: 100, render: (v?: string) => v || '-' },
            {
              title: '状态', dataIndex: 'status', width: 110,
              render: (s: string) => <Tag color={srcStatusColor[s] || 'default'}>{s}</Tag>,
            },
            { title: '版本', dataIndex: 'version', width: 70, render: (v: number) => (v > 0 ? `v${v}` : '-') },
            {
              title: '索引状态', width: 200,
              render: (_: unknown, r: SourceRow) => {
                if (r.jobStatus === 'pending' || r.jobStatus === 'running') {
                  return <Space size={6}><Spin size="small" />索引中 v{r.jobVersion}</Space>
                }
                if (r.jobStatus === 'failed' || r.jobStatus === 'dead') {
                  return <Typography.Text type="danger" style={{ fontSize: 12 }} title={r.jobError}>索引失败（{r.jobStatus}）</Typography.Text>
                }
                if (r.docStatus === 'active') {
                  return <Space size={6}>
                    <Tag color="green">已索引</Tag>
                    {r.trustLevel && <Tag color={trustColor[r.trustLevel] || 'default'}>{r.trustLevel}</Tag>}
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>v{r.docVersion}</Typography.Text>
                  </Space>
                }
                if (r.docStatus === 'quarantined') {
                  return <Tag color="orange">已隔离（unsafe 反馈）</Tag>
                }
                return <Typography.Text type="secondary" style={{ fontSize: 12 }}>{r.status === 'draft' ? '未发布' : '—'}</Typography.Text>
              },
            },
            { title: '更新时间', dataIndex: 'updatedAt', width: 160, render: fmtTime },
            {
              title: '操作', width: 210,
              render: (_: unknown, r: SourceRow) => (
                <Space size={4}>
                  {(r.status === 'draft' || r.status === 'quarantined') && (
                    <Button size="small" type="primary" icon={<PlayCircleOutlined />} loading={publish.isPending} onClick={() => publish.mutate(r.id)}>
                      发布
                    </Button>
                  )}
                  <Button size="small" onClick={async () => {
                    setEditTarget(r)
                    setEditName(r.name)
                    setEditText('')
                    try {
                      const src = await api.source(r.id) // 加载原文回显
                      setEditName(src.name)
                      setEditText(src.content)
                    } catch { /* 加载失败保持空框 */ }
                  }}>
                    编辑
                  </Button>
                  <Popconfirm title="删除后知识文档同步撤销，确认？" onConfirm={() => del.mutate(r.id)}>
                    <Button size="small" danger icon={<DeleteOutlined />} />
                  </Popconfirm>
                </Space>
              ),
            },
          ]}
        />
      </Card>

      {/* 预览 Modal（front matter 解析 + 脱敏警告） */}
      <Modal
        title="知识源预览"
        open={!!preview}
        onCancel={() => setPreview(null)}
        width={760}
        okText="创建草稿"
        onOk={() => create.mutate()}
        confirmLoading={create.isPending}
      >
        {preview && (
          <Space direction="vertical" style={{ width: '100%' }} size="middle">
            {preview.warning && <Alert type="warning" showIcon message={preview.warning} />}
            {preview.redactions.length > 0 && (
              <Space wrap>
                {preview.redactions.map((r) => (
                  <Tag key={r.category} color="red">{r.category} × {r.count}</Tag>
                ))}
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>发布后自动脱敏</Typography.Text>
              </Space>
            )}
            <Descriptions column={2} size="small" bordered>
              <Descriptions.Item label="标题">{preview.frontMatter.title || '（文件名）'}</Descriptions.Item>
              <Descriptions.Item label="服务">{preview.frontMatter.service || '-'}</Descriptions.Item>
              <Descriptions.Item label="标签">{preview.frontMatter.tags?.join(', ') || '-'}</Descriptions.Item>
              <Descriptions.Item label="大小">{Math.ceil(preview.bytes / 1024)} KB</Descriptions.Item>
            </Descriptions>
            <Card size="small" title="原文预览">
              <Typography.Paragraph style={{ maxHeight: 240, overflow: 'auto', whiteSpace: 'pre-wrap', fontSize: 12, marginBottom: 0 }}>
                {preview.body.slice(0, 2000)}{preview.body.length > 2000 ? '\n…（截断预览）' : ''}
              </Typography.Paragraph>
            </Card>
          </Space>
        )}
      </Modal>

      {/* 编辑 Modal（已发布源二次确认生成新版本） */}
      <Modal
        title={editTarget ? `编辑「${editTarget.name}」` : ''}
        open={!!editTarget}
        onCancel={() => setEditTarget(null)}
        width={640}
        okText={editTarget?.status === 'published' ? '保存并发布新版本' : '保存草稿'}
        onOk={() => {
          if (editTarget) update.mutate({ id: editTarget.id, name: editName, content: editText })
        }}
        confirmLoading={update.isPending}
      >
        <Space direction="vertical" style={{ width: '100%' }}>
          {editTarget?.status === 'published' && (
            <Alert type="info" showIcon message={`已发布源：保存将生成新版本 v${editTarget.version + 1} 并重新索引`} />
          )}
          <Input value={editName} onChange={(e) => setEditName(e.target.value)} placeholder="文件名" />
          <Input.TextArea rows={12} value={editText} onChange={(e) => setEditText(e.target.value)} placeholder="markdown 内容（含 front matter）" />
        </Space>
      </Modal>
      {ctx}
    </Space>
  )
}

// ---- Tab2 评测集 ----

function GoldenQueriesTab() {
  const qc = useQueryClient()
  const [msg, ctx] = message.useMessage()
  const [open, setOpen] = useState(false)
  const [form, setForm] = useState<Partial<GoldenQuery>>({ enabled: true })

  const { data, isLoading, error } = useQuery({ queryKey: ['golden-queries'], queryFn: () => api.goldenQueries() })
  const forbidden = error?.message.includes('403')

  const save = useMutation({
    mutationFn: () => {
      const g: Partial<GoldenQuery> = {
        ...form,
        expectedSources: (form.expectedSources ?? []),
      }
      return form.id ? api.updateGoldenQuery(form.id, g) : api.createGoldenQuery(g)
    },
    onSuccess: () => {
      msg.success('已保存')
      setOpen(false)
      setForm({ enabled: true })
      qc.invalidateQueries({ queryKey: ['golden-queries'] })
    },
    onError: (e: Error) => msg.error(`保存失败：${e.message}`),
  })
  const del = useMutation({
    mutationFn: (id: string) => api.deleteGoldenQuery(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['golden-queries'] })
    },
    onError: (e: Error) => msg.error(`删除失败：${e.message}`),
  })
  const setUseful = useMutation({
    mutationFn: ({ id, useful }: { id: string; useful: boolean | null }) =>
      api.updateGoldenQuery(id, { useful }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['golden-queries'] }),
  })

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      {forbidden && (
        <Alert type="warning" showIcon message="管理权限不足"
          description="评测集管理需要 AIOPS_RAGADMINAPIS=true 与 AIOPS_RAGENABLED=true。" />
      )}
      <Card size="small">
        <Typography.Paragraph style={{ marginBottom: 8 }}>
          <Typography.Text strong>检索 golden set 评测</Typography.Text>
          <Typography.Text type="secondary" style={{ display: 'block', fontSize: 12 }}>
            个人真实查询组成评测集（期望命中的事故/runbook），调参前后运行同一评测集对比指标：
          </Typography.Text>
        </Typography.Paragraph>
        <Typography.Paragraph copyable style={{ marginBottom: 0 }}>
          <Typography.Text code style={{ fontSize: 12 }}>
            go run ./cmd/rag-eval -topk-final 6 -json .bench/eval1.json
            {'\n'}go run ./cmd/rag-eval -topk-final 10 -json .bench/eval2.json -baseline .bench/eval1.json
          </Typography.Text>
        </Typography.Paragraph>
      </Card>
      <Card size="small" title="评测查询">
        <Button type="primary" style={{ marginBottom: 12 }} onClick={() => { setForm({ enabled: true }); setOpen(true) }}>
          新增评测查询
        </Button>
        <Table<GoldenQuery>
          rowKey="id"
          size="small"
          loading={isLoading}
          dataSource={data?.items ?? []}
          pagination={false}
          columns={[
            { title: '查询', dataIndex: 'query', ellipsis: true },
            { title: '期望', width: 80, render: (_: unknown, r) => r.expectedSources?.length ?? 0 },
            { title: '分类', dataIndex: 'tags', width: 120, render: (v?: string) => v || '-' },
            {
              title: '启用', dataIndex: 'enabled', width: 70,
              render: (v: boolean, r) => (
                <Switch size="small" checked={v} onChange={(en) => void api.updateGoldenQuery(r.id, { enabled: en }).then(() => qc.invalidateQueries({ queryKey: ['golden-queries'] }))} />
              ),
            },
            {
              title: 'useful', width: 130,
              render: (_: unknown, r) => (
                <Select value={r.useful === null || r.useful === undefined ? 'unset' : r.useful ? 'yes' : 'no'}
                  size="small" style={{ width: 100 }}
                  options={[
                    { value: 'unset', label: '未评估' },
                    { value: 'yes', label: '有用' },
                    { value: 'no', label: '没用' },
                  ]}
                  onChange={(v) => setUseful.mutate({ id: r.id, useful: v === 'unset' ? null : v === 'yes' })}
                />
              ),
            },
            { title: '更新时间', dataIndex: 'updatedAt', width: 150, render: fmtTime },
            {
              title: '操作', width: 100,
              render: (_: unknown, r) => (
                <Space size={4}>
                  <Button size="small" onClick={() => { setForm(r); setOpen(true) }}>编辑</Button>
                  <Popconfirm title="删除该评测查询？" onConfirm={() => del.mutate(r.id)}>
                    <Button size="small" danger icon={<DeleteOutlined />} />
                  </Popconfirm>
                </Space>
              ),
            },
          ]}
        />
      </Card>
      <Modal
        title={form.id ? '编辑评测查询' : '新增评测查询'}
        open={open}
        onCancel={() => setOpen(false)}
        onOk={() => save.mutate()}
        confirmLoading={save.isPending}
        width={620}
      >
        <Space direction="vertical" style={{ width: '100%' }}>
          <Input.TextArea rows={2} placeholder="真实查询文本（如：连接池耗尽怎么办）" value={form.query}
            onChange={(e) => setForm({ ...form, query: e.target.value })} />
          <Space style={{ display: 'flex' }}>
            <Input placeholder="服务（可选）" value={form.service} onChange={(e) => setForm({ ...form, service: e.target.value })} style={{ flex: 1 }} />
            <Input placeholder="严重度（可选）" value={form.severity} onChange={(e) => setForm({ ...form, severity: e.target.value })} style={{ width: 120 }} />
          </Space>
          <Input.TextArea rows={3} placeholder="期望命中的 source_id（每行一个；事故 ID 或知识源 ID）"
            value={(form.expectedSources ?? []).join('\n')}
            onChange={(e) => setForm({ ...form, expectedSources: e.target.value.split('\n').map((s) => s.trim()).filter(Boolean) })} />
          <Space style={{ display: 'flex' }}>
            <Input placeholder="分类 tags（如 connection-pool）" value={form.tags} onChange={(e) => setForm({ ...form, tags: e.target.value })} style={{ flex: 1 }} />
            <Input placeholder="备注" value={form.note} onChange={(e) => setForm({ ...form, note: e.target.value })} style={{ flex: 1 }} />
          </Space>
          <Switch checked={form.enabled} onChange={(v) => setForm({ ...form, enabled: v })} checkedChildren="启用" unCheckedChildren="停用" />
        </Space>
      </Modal>
      {ctx}
    </Space>
  )
}

// ---- Tab3 检索状态 ----

function RagStatusTab() {
  const { data, isLoading, error } = useQuery({ queryKey: ['rag-status'], queryFn: api.ragStatus })
  const forbidden = error?.message.includes('403')
  if (forbidden) {
    return <Alert type="warning" showIcon message="管理权限不足" description="需要 AIOPS_RAGADMINAPIS=true。" />
  }
  if (isLoading) return <Spin />
  if (!data || data.enabled === false) {
    return <Alert type="info" showIcon message="RAG 未启用" description="设置 AIOPS_RAGENABLED=true AIOPS_RAGMODE=readonly 后查看检索状态。" />
  }
  const docs = data.documents ?? {}
  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Card size="small" title="知识库文档">
        <Space size="large" wrap>
          <span>总计 <b>{docs.total ?? 0}</b></span>
          <span>有效 <b>{docs.active ?? 0}</b></span>
          <span>历史版本 <b>{docs.superseded ?? 0}</b></span>
          <span>已隔离 <b>{docs.quarantined ?? 0}</b></span>
          <span>chunks <b>{docs.chunks ?? 0}</b></span>
          <span>embeddings <b>{docs.embeddings ?? 0}</b></span>
          {docs.bySourceType && Object.entries(docs.bySourceType).map(([t, n]) => (
            <Tag key={t} color="blue">{t}: {String(n)}</Tag>
          ))}
        </Space>
      </Card>
      <Card size="small" title="索引与向量">
        <Descriptions column={2} size="small">
          <Descriptions.Item label="模式">{data.mode}</Descriptions.Item>
          <Descriptions.Item label="索引版本">{data.configuredVersion}</Descriptions.Item>
          <Descriptions.Item label="向量路">
            {data.vectorEnabled ? <Tag color="green">启用</Tag> : <Tag color="default">降级</Tag>}
            {data.vectorReadyReason && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{data.vectorReadyReason}</Typography.Text>}
          </Descriptions.Item>
          <Descriptions.Item label="模型">{data.embeddingModel || '-'}（{data.embeddingDimension || 0} 维）</Descriptions.Item>
          <Descriptions.Item label="积压">{data.backlog ?? 0}</Descriptions.Item>
          <Descriptions.Item label="失败/死任务">{data.failedJobs ?? 0} / {data.deadJobs ?? 0}</Descriptions.Item>
          <Descriptions.Item label="上次成功">{data.lastSuccessAt ? fmtTime(data.lastSuccessAt) : '—'}</Descriptions.Item>
          <Descriptions.Item label="滞后">{data.lagSeconds ?? 0}s</Descriptions.Item>
        </Descriptions>
      </Card>
    </Space>
  )
}

export default function KnowledgePage() {
  return (
    <Tabs
      items={[
        { key: 'sources', label: '知识源', children: <SourcesTab /> },
        { key: 'golden', label: '评测集', children: <GoldenQueriesTab /> },
        { key: 'status', label: '检索状态', children: <RagStatusTab /> },
      ]}
    />
  )
}
