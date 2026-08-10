import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Descriptions, Tag, Typography, Space, Button, Card, Table, Alert, Spin, Timeline, Modal, Input, Divider,
  Form, Select, List, Dropdown, message,
} from 'antd'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { RobotOutlined } from '@ant-design/icons'
import { api, type IncidentDetail, type KnowledgeState, type SimilarItem } from '../api'

const severityColor: Record<string, string> = {
  critical: 'red',
  high: 'volcano',
  medium: 'gold',
  low: 'blue',
}

const levelColor: Record<string, string> = { L0: 'green', L1: 'orange', L2: 'red' }
const statusColor: Record<string, string> = {
  dry_run: 'blue', awaiting_approval: 'orange', executed: 'green',
  rejected: 'red', blocked: 'volcano', no_playbook: 'default', no_suggested_action: 'default',
  disabled: 'default', proposed: 'blue',
}

// Heal 结果卡片。
function HealCard({ heal }: { heal: Record<string, any> }) {
  return (
    <Card size="small" title="Heal · 自愈动作">
      <Descriptions column={2} size="small">
        <Descriptions.Item label="动作">
          <Tag color="blue">{String(heal.action ?? '-')}</Tag>
        </Descriptions.Item>
        <Descriptions.Item label="级别">
          <Tag color={levelColor[String(heal.level)] || 'default'}>{String(heal.level ?? '-')}</Tag>
        </Descriptions.Item>
        <Descriptions.Item label="状态">
          <Tag color={statusColor[String(heal.status)] || 'default'}>{String(heal.status ?? '-')}</Tag>
        </Descriptions.Item>
        <Descriptions.Item label="爆炸半径">
          {heal.blastRadius != null ? `${(Number(heal.blastRadius) * 100).toFixed(0)}%` : '-'}
          {heal.downgraded && <Tag color="red" style={{ marginLeft: 8 }}>已降级 L2</Tag>}
        </Descriptions.Item>
        <Descriptions.Item label="需审批">
          {heal.approvalRequired ? <Tag color="orange">是</Tag> : <Tag color="green">否</Tag>}
        </Descriptions.Item>
        <Descriptions.Item label="DryRun">{heal.dryRun ? '是' : '否'}</Descriptions.Item>
      </Descriptions>
      {heal.command && (
        <Typography.Paragraph style={{ marginBottom: 4 }}>
          <Typography.Text type="secondary">执行命令：</Typography.Text>
          <Typography.Text code>{String(heal.command)}</Typography.Text>
        </Typography.Paragraph>
      )}
      {heal.rollbackCommand && heal.rollbackCommand !== 'true' && (
        <Typography.Paragraph style={{ marginBottom: 4 }}>
          <Typography.Text type="secondary">回滚命令：</Typography.Text>
          <Typography.Text code>{String(heal.rollbackCommand)}</Typography.Text>
        </Typography.Paragraph>
      )}
      {heal.reason && <Typography.Paragraph type="secondary">{String(heal.reason)}</Typography.Paragraph>}
    </Card>
  )
}

// Change 结果卡片：5 维风险评分。
function ChangeCard({ change }: { change: Record<string, any> }) {
  const dimLabels: Record<string, string> = {
    blastRadius: '爆炸半径', operation: '操作风险', time: '时间风险', history: '历史', criticality: '关键度',
  }
  return (
    <Card size="small" title="Change · 变更风险评估">
      <Descriptions column={2} size="small">
        <Descriptions.Item label="风险评分">
          <Typography.Text strong style={{ fontSize: 18, color: levelColor[String(change.level)] || undefined }}>
            {change.riskScore != null ? `${(Number(change.riskScore) * 100).toFixed(0)}` : '-'}
          </Typography.Text>
        </Descriptions.Item>
        <Descriptions.Item label="级别">
          <Tag color={levelColor[String(change.level)] || 'default'}>{String(change.level ?? '-')}</Tag>
        </Descriptions.Item>
        <Descriptions.Item label="决策">{String(change.decision ?? '-')}</Descriptions.Item>
        <Descriptions.Item label="评分时间">{String(change.scoredAt ?? '-')}</Descriptions.Item>
      </Descriptions>
      {change.breakdown && (
        <Table
          rowKey="k"
          size="small"
          pagination={false}
          dataSource={Object.entries(change.breakdown).map(([k, v]) => ({
            k, label: dimLabels[k] || k, value: v,
            weight: change.weights?.[k] ?? 0,
          }))}
          columns={[
            { title: '维度', dataIndex: 'label' },
            { title: '权重', dataIndex: 'weight', render: (v: number) => `${(v * 100).toFixed(0)}%` },
            { title: '风险', dataIndex: 'value', render: (v: number) => `${(v * 100).toFixed(0)}%` },
          ]}
        />
      )}
    </Card>
  )
}

// NodeResults：Monitor / RCA / Heal / Change 四段结果。
function NodeResults({ detail }: { detail: IncidentDetail }) {
  const nr = detail.nodeResults
  if (!nr) {
    return <Alert type="info" message="尚无 Agent 结果（该事故可能未进入编排路径）" />
  }
  const { monitor, rca, heal, change } = nr
  return (
    <Space direction="vertical" style={{ width: '100%' }}>
      {monitor && (
        <Card size="small" title="Monitor · 监控分析">
          <Descriptions column={2} size="small">
            <Descriptions.Item label="服务">{String(monitor.service ?? '-')}</Descriptions.Item>
            <Descriptions.Item label="严重度">{String(monitor.severity ?? '-')}</Descriptions.Item>
            <Descriptions.Item label="Provider">{String(monitor.provider ?? '-')}</Descriptions.Item>
            <Descriptions.Item label="分析时间">{String(monitor.analyzedAt ?? '-')}</Descriptions.Item>
          </Descriptions>
        </Card>
      )}
      {rca && (
        <Card size="small" title="RCA · 根因分析">
          <Descriptions column={2} size="small">
            <Descriptions.Item label="根因">
              <Tag color={rca.rootCause === 'unknown' ? 'orange' : 'red'}>{String(rca.rootCause)}</Tag>
            </Descriptions.Item>
            <Descriptions.Item label="置信度">
              {rca.confidence != null ? `${(Number(rca.confidence) * 100).toFixed(0)}%` : '-'}
            </Descriptions.Item>
          </Descriptions>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 4 }}>证据链：</Typography.Paragraph>
          <ul style={{ marginTop: 0 }}>
            {(rca.evidence as string[] | undefined)?.map((e: string, i: number) => <li key={i}>{e}</li>)}
          </ul>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 4 }}>建议动作：</Typography.Paragraph>
          <Space wrap>
            {(rca.suggestedActions as string[] | undefined)?.map((a: string) => (
              <Tag key={a} color="blue">{a}</Tag>
            ))}
          </Space>
          {rca.llmRootCause && (
            <>
              <Divider style={{ margin: '12px 0' }} />
              <Typography.Paragraph style={{ marginBottom: 4 }}>
                <Tag color="purple">LLM 补充分析</Tag>
              </Typography.Paragraph>
              <Typography.Paragraph style={{ marginBottom: 4 }}>
                <b>根因：</b>{String(rca.llmRootCause)}
              </Typography.Paragraph>
              {rca.llmRationale && (
                <Typography.Paragraph type="secondary" style={{ marginBottom: 4 }}>
                  {String(rca.llmRationale)}
                </Typography.Paragraph>
              )}
              {(rca.llmSuggestedActions as string[] | undefined)?.length ? (
                <>
                  <Typography.Paragraph type="secondary" style={{ marginBottom: 4 }}>建议动作：</Typography.Paragraph>
                  <Space wrap>
                    {(rca.llmSuggestedActions as string[]).map((a: string) => (
                      <Tag key={a} color="purple">{a}</Tag>
                    ))}
                  </Space>
                </>
              ) : null}
            </>
          )}
        </Card>
      )}
      {heal && <HealCard heal={heal} />}
      {change && <ChangeCard change={change} />}
    </Space>
  )
}

// 审批面板：仅在 pending_approval 时出现。
function ApprovalPanel({ detail }: { detail: IncidentDetail }) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [decision, setDecision] = useState<'approve' | 'reject'>('approve')
  const [reason, setReason] = useState('')

  const mutate = useMutation({
    mutationFn: () => api.approveIncident(detail.id, decision, reason),
    onSuccess: () => {
      setOpen(false)
      setReason('')
      qc.invalidateQueries({ queryKey: ['incidents'] })
    },
  })

  const heal = detail.nodeResults?.heal
  if (detail.state?.status !== 'pending_approval') return null

  return (
    <>
      <Alert
      type="warning"
      showIcon
      message="自愈动作待审批"
      description={
        <Space direction="vertical">
          <Space>
            建议动作：
            <Tag color="blue">{String(heal?.action ?? '-')}</Tag>
            级别：
            <Tag color={levelColor[String(heal?.level)] || 'default'}>{String(heal?.level ?? '-')}</Tag>
            需人工决策后方可执行（MVP 模拟执行，仅落审计）。
          </Space>
          <Space>
            <Button type="primary" onClick={() => { setDecision('approve'); setOpen(true) }}>
              批准
            </Button>
            <Button danger onClick={() => { setDecision('reject'); setOpen(true) }}>
              拒绝
            </Button>
          </Space>
        </Space>
      }
    />
    <Modal
        title={decision === 'approve' ? '批准自愈动作' : '拒绝自愈动作'}
        open={open}
        onOk={() => mutate.mutate()}
        confirmLoading={mutate.isPending}
        onCancel={() => setOpen(false)}
      >
        <Typography.Paragraph type="secondary">
          操作者：{decision === 'approve' ? 'oncall-engineer' : 'user'}（默认）
        </Typography.Paragraph>
        <Input.TextArea
          placeholder="审批理由（who/when/what/why 审计基线）"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
        />
      </Modal>
    </>
  )
}

// 测试通知：手动触发各通知渠道（验证钉钉/飞书等 webhook 配置）。
function NotifyTestButton() {
  const [open, setOpen] = useState(false)
  const [title, setTitle] = useState('')
  const [content, setContent] = useState('')
  const [result, setResult] = useState<string | null>(null)

  const send = async () => {
    setResult(null)
    try {
      const r = await api.notifyTest(
        title,
        content || '这是一条来自 AIops 的测试通知，用于验证渠道配置。',
        'critical',
      )
      if (r.channels.length === 0) {
        setResult('未配置通知渠道（AIOPS_NOTIFIERCHANNELS 为空），通知跳过。')
      } else {
        setResult(
          r.results
            .map((x) => (x.ok ? `✅ ${x.channel} 发送成功` : `❌ ${x.channel} 失败：${x.error || '未知错误'}`))
            .join('\n'),
        )
      }
    } catch (e) {
      setResult(`请求失败：${(e as Error).message}`)
    }
  }

  return (
    <>
      <Button onClick={() => setOpen(true)}>测试通知</Button>
      <Modal
        title="测试通知渠道"
        open={open}
        onOk={send}
        okText="发送"
        onCancel={() => setOpen(false)}
      >
        <Space direction="vertical" style={{ width: '100%' }}>
          <Input placeholder="标题（默认 AIops 通知测试）" value={title} onChange={(e) => setTitle(e.target.value)} />
          <Input.TextArea
            placeholder="正文（默认测试消息，markdown）"
            value={content}
            onChange={(e) => setContent(e.target.value)}
            rows={3}
          />
          {result && <Typography.Text type={result.includes('✅') ? 'success' : 'danger'} style={{ whiteSpace: 'pre-line' }}>{result}</Typography.Text>}
        </Space>
      </Modal>
    </>
  )
}

const trustColor: Record<string, string> = { T3: 'green', T2: 'cyan', T1: 'default', T0: 'default' }
const outcomeColor: Record<string, string> = {
  success: 'green', partial: 'orange', failed: 'red', false_positive: 'default',
}
const verifColor: Record<string, string> = { verified: 'green', unverified: 'default', rejected: 'red' }

// 结案数据（RAG Phase 0 + KEEPON M3）：已解决事故填写/编辑人工确认根因与处置结果，
// 保存即发布到知识库（resHook 同事务入队）；下方展示三态（已解决/已结案/已索引）。
function ResolutionCard({ detail }: { detail: IncidentDetail }) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [form] = Form.useForm()
  const res = detail.resolution
  const resolved = detail.status === 'resolved'

  // 结案→发布→索引三态（KEEPON M3）；仅在"索引中"时 3s 轮询
  const ks = useQuery({
    queryKey: ['incidents', detail.id, 'knowledge-status'],
    queryFn: () => api.knowledgeStatus(detail.id),
    enabled: resolved || !!res,
    refetchInterval: (q) => {
      const st = q.state.data
      return st && !st.indexed && st.job && (st.job.status === 'pending' || st.job.status === 'running') ? 3000 : false
    },
  })

  const save = useMutation({
    mutationFn: (v: Record<string, any>) => api.saveResolution(detail.id, v),
    onSuccess: () => {
      setOpen(false)
      message.success('已发布到知识库（索引中…）')
      qc.invalidateQueries({ queryKey: ['incidents', detail.id] })
      qc.invalidateQueries({ queryKey: ['incidents', detail.id, 'knowledge-status'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (e: Error) => message.error(`保存失败：${e.message}`),
  })

  if (!resolved && !res) return null

  // RCA 带出区数据（detail 已有；确定性证据 vs LLM 推测明确标注来源）
  const rca = detail.nodeResults?.rca as Record<string, any> | undefined
  const detRoot = rca?.rootCause
  const llmRoot = rca?.llmRootCause

  return (
    <Card
      size="small"
      title={
        <Space>
          <Tag color="green">结案数据</Tag>
          <Typography.Text type="secondary">结案即发布到知识库（人工确认根因 → 相似事故检索证据）</Typography.Text>
        </Space>
      }
      extra={resolved && <Button size="small" onClick={() => { form.setFieldsValue(res ?? {}); setOpen(true) }}>{res ? '编辑' : '填写并发布'}</Button>}
    >
      {res ? (
        <Descriptions column={2} size="small">
          <Descriptions.Item label="确认根因" span={2}>{res.confirmedRootCause || '-'}</Descriptions.Item>
          <Descriptions.Item label="实际处置" span={2}>{res.resolution || '-'}</Descriptions.Item>
          <Descriptions.Item label="结果">
            <Tag color={outcomeColor[res.outcome] || 'default'}>{res.outcome || '-'}</Tag>
          </Descriptions.Item>
          <Descriptions.Item label="验证状态">
            <Tag color={verifColor[res.verificationStatus] || 'default'}>{res.verificationStatus || '-'}</Tag>
          </Descriptions.Item>
          <Descriptions.Item label="验证方法" span={2}>{res.verification || '-'}</Descriptions.Item>
          {res.postmortemUrl && <Descriptions.Item label="复盘地址" span={2}>{res.postmortemUrl}</Descriptions.Item>}
          <Descriptions.Item label="确认人">{res.resolvedBy || '-'}</Descriptions.Item>
          <Descriptions.Item label="版本">v{res.version}</Descriptions.Item>
        </Descriptions>
      ) : (
        <Typography.Text type="secondary">事故已解决，尚未填写结案数据——填写确认根因并发布后，进入相似事故知识库。</Typography.Text>
      )}
      {ks.data && <KnowledgeStatusBlock state={ks.data} />}
      <Modal
        title="结案并发布到知识库"
        open={open}
        onOk={() => form.submit()}
        confirmLoading={save.isPending}
        onCancel={() => setOpen(false)}
        width={620}
        okText="结案并发布到知识库"
      >
        {/* 只读带出区（KEEPON M3）：故障现象/关联告警/RCA 来源标注 */}
        {(detail.summary || detail.alerts.length > 0 || detRoot || llmRoot) && (
          <div style={{ marginBottom: 16, padding: '8px 12px', background: '#fafafa', border: '1px solid #f0f0f0', borderRadius: 6 }}>
            <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginBottom: 4 }}>事故背景（自动带出，仅展示）</Typography.Text>
            {detail.summary && (
              <div style={{ marginBottom: 4 }}>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>现象：</Typography.Text>
                <Typography.Text style={{ fontSize: 13 }}>{detail.summary}</Typography.Text>
              </div>
            )}
            {detail.alerts.length > 0 && (
              <div style={{ marginBottom: 4 }}>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>告警：</Typography.Text>
                {detail.alerts.slice(0, 5).map((a) => (
                  <Tag key={a.id} color={severityColor[a.severity] || 'default'} style={{ marginInlineEnd: 4 }}>{a.name}</Tag>
                ))}
                {detail.alerts.length > 5 && <Typography.Text type="secondary" style={{ fontSize: 12 }}>+{detail.alerts.length - 5}</Typography.Text>}
              </div>
            )}
            {detRoot && (
              <div style={{ marginBottom: 4 }}>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>根因：</Typography.Text>
                <Tag color="blue" style={{ marginInlineEnd: 4 }}>确定性证据</Tag>
                <Typography.Text style={{ fontSize: 13 }}>{String(detRoot)}</Typography.Text>
              </div>
            )}
            {llmRoot && (
              <div>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>根因：</Typography.Text>
                <Tag color="purple" style={{ marginInlineEnd: 4 }}>LLM 推测</Tag>
                <Typography.Text style={{ fontSize: 13 }}>{String(llmRoot)}</Typography.Text>
              </div>
            )}
          </div>
        )}
        <Form
          form={form}
          layout="vertical"
          onFinish={(v) => save.mutate(v)}
        >
          <Form.Item name="confirmedRootCause" label="确认根因" rules={[{ required: true, message: '必填（verified 前提）' }]}>
            <Input.TextArea rows={2} placeholder="人工确认的根因（区别于确定性证据与 LLM 推测）" />
          </Form.Item>
          <Form.Item name="resolution" label="实际处置" rules={[{ required: true, message: '必填（verified 前提）' }]}>
            <Input.TextArea rows={2} placeholder="实际执行的动作（如：回滚配置并重启连接池）" />
          </Form.Item>
          <Space size="large" style={{ display: 'flex' }}>
            <Form.Item name="outcome" label="处置结果" rules={[{ required: true }]} style={{ flex: 1 }}>
              <Select
                placeholder="选择"
                options={[
                  { value: 'success', label: 'success（成功）' },
                  { value: 'partial', label: 'partial（部分成功）' },
                  { value: 'failed', label: 'failed（失败）' },
                  { value: 'false_positive', label: 'false_positive（误报）' },
                ]}
              />
            </Form.Item>
            <Form.Item name="verificationStatus" label="验证状态" rules={[{ required: true }]} style={{ flex: 1 }}>
              <Select
                placeholder="选择"
                options={[
                  { value: 'verified', label: 'verified（已验证）' },
                  { value: 'unverified', label: 'unverified（未验证）' },
                  { value: 'rejected', label: 'rejected（确认有误）' },
                ]}
              />
            </Form.Item>
          </Space>
          <Form.Item name="verification" label="验证方法及结果">
            <Input.TextArea rows={2} placeholder="如：错误率恢复、连接池等待时间回落" />
          </Form.Item>
          <Space size="large" style={{ display: 'flex' }}>
            <Form.Item name="resolvedBy" label="确认人" style={{ flex: 1 }}>
              <Input placeholder="如：oncall-engineer" />
            </Form.Item>
            <Form.Item name="postmortemUrl" label="复盘地址" style={{ flex: 1 }}>
              <Input placeholder="https://...（可选）" />
            </Form.Item>
          </Space>
        </Form>
      </Modal>
    </Card>
  )
}

// KnowledgeStatusBlock 结案→发布→索引三态展示（KEEPON M3；trustColor 复用 SimilarIncidents 定义）。
function KnowledgeStatusBlock({ state }: { state: KnowledgeState }) {
  const d = state.document
  const j = state.job
  const indexing = !state.indexed && !!j && (j.status === 'pending' || j.status === 'running')

  return (
    <div style={{ marginTop: 10, padding: '8px 12px', background: '#f6ffed', border: '1px solid #b7eb8f', borderRadius: 6 }}>
      <Space size="middle" wrap>
        <Tag color={state.resolved ? 'green' : 'default'}>事故已解决</Tag>
        <Tag color={state.resolutionFilled ? 'green' : 'default'}>结案已填写</Tag>
        <Tag color={state.indexed ? 'blue' : 'default'}>知识已索引</Tag>
      </Space>
      <div style={{ marginTop: 6, fontSize: 13 }}>
        {!state.resolutionFilled ? (
          <Typography.Text type="secondary">填写结案数据即发布到知识库</Typography.Text>
        ) : indexing ? (
          <Space size={6}>
            <Spin size="small" />
            <Typography.Text>索引中…（知识版本 v{state.resolutionVersion}）</Typography.Text>
          </Space>
        ) : !state.indexed && d?.status === 'quarantined' ? (
          <Alert type="warning" showIcon style={{ padding: '4px 8px' }}
            message="已隔离（unsafe 反馈，已退出召回）——重新填写结案发布新版本可恢复" />
        ) : !state.indexed && j && (j.status === 'failed' || j.status === 'dead') ? (
          <Typography.Text type="danger">
            索引失败：{j.lastError || j.status}（尝试 {j.attempts} 次）
          </Typography.Text>
        ) : state.indexed && d ? (
          <Space size="middle" wrap>
            <Typography.Text strong>
              已发布 · <Tag color={trustColor[d.trustLevel] || 'default'} style={{ marginInlineEnd: 0 }}>{d.trustLevel}</Tag>
              {d.verificationStatus === 'verified' ? ' 已验证' : ` ${d.verificationStatus}`} · 索引完成 · 版本 v{d.sourceVersion}
            </Typography.Text>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              最后更新 {new Date(d.updatedAt).toLocaleString('zh-CN', { hour12: false })}
            </Typography.Text>
          </Space>
        ) : (
          <Typography.Text type="secondary">知识尚未建立（{d ? `文档状态：${d.status}` : '无文档记录'}）</Typography.Text>
        )}
      </div>
    </div>
  )
}

// 相似历史事故（RAG 只读检索）：展示来源/可信等级/处置结果 + 反馈按钮。
function SimilarIncidents({ detail }: { detail: IncidentDetail }) {
  const qc = useQueryClient()
  const [fed, setFed] = useState<Record<string, string>>({}) // chunkId → rating（已反馈）
  const { data, isLoading } = useQuery({
    queryKey: ['incidents', detail.id, 'similar'],
    queryFn: () => api.similarIncidents(detail.id),
  })

  const feedback = useMutation({
    mutationFn: (v: { item: SimilarItem; rating: string; traceId?: string }) =>
      api.ragFeedback(detail.id, { rating: v.rating, documentId: v.item.documentId, traceId: v.traceId }),
    onSuccess: (_r, v) => {
      setFed((m) => ({ ...m, [v.item.chunkId]: v.rating }))
      message.success(v.rating === 'unsafe' ? '已标记 unsafe：该文档已退出检索（quarantine）' : '反馈已记录')
      if (v.rating === 'unsafe') qc.invalidateQueries({ queryKey: ['incidents', detail.id, 'similar'] })
    },
    onError: (e: Error) => message.error(`反馈失败：${e.message}`),
  })

  if (!data?.enabled) return null
  return (
    <Card
      size="small"
      title={
        <Space>
          <Tag color="cyan">相似历史事故</Tag>
          <Typography.Text type="secondary">
            RAG 历史证据检索（{data.items.length} 条{data.degradedReason ? ` · 降级：${data.degradedReason}` : ''}）
          </Typography.Text>
        </Space>
      }
      loading={isLoading}
    >
      {data.items.length === 0 ? (
        <Typography.Text type="secondary">暂无相似历史证据（索引积压或知识库为空）</Typography.Text>
      ) : (
        <List
          size="small"
          dataSource={data.items}
          renderItem={(item) => (
            <List.Item
              actions={[
                <Dropdown
                  key="fb"
                  menu={{
                    items: [
                      { key: 'useful', label: '👍 有用' },
                      { key: 'not_useful', label: '👎 无用' },
                      { key: 'wrong', label: '⚠️ 根因错误' },
                      { key: 'unsafe', label: '🚫 不安全（立即隔离）' },
                    ],
                    onClick: ({ key }) => feedback.mutate({ item, rating: key, traceId: data.traceId }),
                  }}
                >
                  <Button size="small" disabled={!!fed[item.chunkId]}>
                    {fed[item.chunkId] ? '已反馈' : '反馈'}
                  </Button>
                </Dropdown>,
              ]}
            >
              <List.Item.Meta
                title={
                  <Space wrap>
                    <span>{item.title}</span>
                    <Tag color={item.sourceType === 'playbook' ? 'blue' : 'geekblue'}>
                      {item.sourceType === 'playbook' ? '剧本' : '事故'}
                    </Tag>
                    <Tag color={trustColor[item.trustLevel] || 'default'}>T{item.trustLevel.slice(1)}</Tag>
                    {item.outcome && <Tag color={outcomeColor[item.outcome] || 'default'}>{item.outcome}</Tag>}
                    {item.verificationStatus && (
                      <Tag color={verifColor[item.verificationStatus] || 'default'}>{item.verificationStatus}</Tag>
                    )}
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      {new Date(item.time).toLocaleDateString()} · 相似度 {(item.score * 100).toFixed(1)}%
                    </Typography.Text>
                  </Space>
                }
                description={
                  <Space direction="vertical" size={2} style={{ width: '100%' }}>
                    {item.confirmedRootCause && (
                      <Typography.Text>确认根因：{item.confirmedRootCause}</Typography.Text>
                    )}
                    {item.resolution && (
                      <Typography.Text type="secondary">处置：{item.resolution}</Typography.Text>
                    )}
                    {item.reasons.length > 0 && (
                      <Space size={4} wrap>
                        {item.reasons.map((r) => <Tag key={r} style={{ fontSize: 11 }}>{r}</Tag>)}
                      </Space>
                    )}
                  </Space>
                }
              />
            </List.Item>
          )}
        />
      )}
    </Card>
  )
}

// 审计轨迹。
function AuditTrail({ detail }: { detail: IncidentDetail }) {
  const { data } = useQuery({
    queryKey: ['incidents', detail.id, 'audits'],
    queryFn: () => api.audits(detail.id),
    enabled: detail.state?.status !== undefined,
  })
  const items = data?.items ?? []
  if (items.length === 0) return null
  const actionColor: Record<string, string> = {
    'approval.granted': 'green', 'approval.rejected': 'red', 'heal.executed': 'blue',
    'notify.sent': 'cyan', 'notify.failed': 'volcano', 'notify.skipped': 'default',
  }
  return (
    <Card size="small" title="审计轨迹（who/when/what/why）">
      <Timeline
        items={items.map((a) => ({
          color: actionColor[a.action] || 'gray',
          children: (
            <div>
              <Space>
                <Tag color={actionColor[a.action] || 'default'}>{a.action}</Tag>
                <Typography.Text type="secondary">actor={a.actor}</Typography.Text>
                <Typography.Text type="secondary">target={a.target}</Typography.Text>
                <Typography.Text type="secondary">{new Date(a.createdAt).toLocaleString()}</Typography.Text>
              </Space>
              {a.reason && <div><Typography.Text type="secondary">理由：{a.reason}</Typography.Text></div>}
              {a.metadata?.correlationId && (
                <div><Typography.Text code style={{ fontSize: 12 }}>correlationId: {a.metadata.correlationId}</Typography.Text></div>
              )}
            </div>
          ),
        }))}
      />
    </Card>
  )
}

export default function IncidentDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const { data: detail, isLoading } = useQuery({
    queryKey: ['incidents', id],
    queryFn: () => api.incident(id!),
  })

  const resolve = useMutation({
    mutationFn: () => api.resolveIncident(id!),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['incidents'] }),
  })

  if (isLoading) return <Spin />
  if (!detail) return <Alert type="error" message="事故不存在" />

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Card
        title={
          <Space>
            <Link to="/incidents">← Incidents</Link>
            <span>{detail.name || detail.id}</span>
            <Tag color={severityColor[detail.severity] || 'default'}>{detail.severity}</Tag>
            <Tag>{detail.status}</Tag>
          </Space>
        }
        extra={
          <>
            <NotifyTestButton />
            {/* 在 AI 助手中分析：绑定本事故上下文（AGENT_CHAT_PLAN Phase 2） */}
            <Button icon={<RobotOutlined />} onClick={() => navigate(`/assistant?incident=${id}`)}>
              在 AI 助手中分析
            </Button>
            {detail.status !== 'resolved' && (
              <Button type="primary" danger loading={resolve.isPending} onClick={() => resolve.mutate()}>
                解决事故
              </Button>
            )}
          </>
        }
      >
        <Descriptions column={3} size="small">
          <Descriptions.Item label="来源">{detail.source}</Descriptions.Item>
          <Descriptions.Item label="创建时间">{new Date(detail.createdAt).toLocaleString()}</Descriptions.Item>
          <Descriptions.Item label="当前 Agent">
            {detail.state?.currentAgent || '-'}
            {detail.state?.retryCount ? `（重试 ${detail.state.retryCount}）` : ''}
          </Descriptions.Item>
        </Descriptions>
        {detail.summary && <Typography.Paragraph>{detail.summary}</Typography.Paragraph>}
        {detail.llmSummary && (
          <Space direction="vertical" size={2} style={{ width: '100%', marginBottom: 8 }}>
            <Typography.Text type="secondary">
              <Tag color="purple">LLM 聚类摘要</Tag>
            </Typography.Text>
            <Typography.Paragraph style={{ marginBottom: 0 }}>{detail.llmSummary}</Typography.Paragraph>
          </Space>
        )}
        {detail.state?.errorMessage && (
          <Alert type="warning" showIcon message="节点错误" description={detail.state.errorMessage} />
        )}
      </Card>

      <ApprovalPanel detail={detail} />
      <ResolutionCard detail={detail} />
      <NodeResults detail={detail} />
      <AuditTrail detail={detail} />
      <SimilarIncidents detail={detail} />

      <Card size="small" title={`关联告警（${detail.alerts.length}）`}>
        <Table
          rowKey="id"
          size="small"
          dataSource={detail.alerts}
          pagination={false}
          columns={[
            { title: '名称', dataIndex: 'name' },
            {
              title: '严重度',
              dataIndex: 'severity',
              render: (s: string) => <Tag color={severityColor[s]}>{s}</Tag>,
            },
            { title: '状态', dataIndex: 'status' },
            { title: '最近上报', dataIndex: 'lastSeenAt', render: (v: string) => new Date(v).toLocaleString() },
          ]}
        />
      </Card>
    </Space>
  )
}
