import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Descriptions, Tag, Typography, Space, Button, Card, Table, Alert, Spin, Timeline, Modal, Input, Divider,
} from 'antd'
import { useParams, Link } from 'react-router-dom'
import { api, type IncidentDetail } from '../api'

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
      <NodeResults detail={detail} />
      <AuditTrail detail={detail} />

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
