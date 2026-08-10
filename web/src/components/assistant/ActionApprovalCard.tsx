import { useState } from 'react'
import { Button, Popconfirm, Tag, Typography, message } from 'antd'
import {
  CheckCircleOutlined, ClockCircleOutlined, ExclamationCircleOutlined,
  SafetyOutlined, StopOutlined,
} from '@ant-design/icons'
import { api } from '../../api'
import type { ActionRequest } from '../../assistant/types'

// ActionApprovalCard 人工确认动作卡（Phase 3，AGENT_CHAT_PLAN §11.2）：
// 展示模型提出的动作目标/参数/风险；pending 提供确认/拒绝（确认后才执行领域副作用）；
// 已处理状态展示结果与原因。决策后通过 onChanged 通知上层刷新。

const riskColor: Record<string, string> = { low: 'green', medium: 'orange', high: 'red' }

const statusText: Record<string, string> = {
  pending: '等待确认', executing: '执行中', succeeded: '已执行',
  failed: '执行失败', rejected: '已拒绝', expired: '已过期',
}

// toolLabel 动作工具展示名（与后端 tools_action.go 对齐）。
const toolLabel: Record<string, string> = {
  ack_alert: '确认告警', resolve_incident: '解决事故', approve_incident_action: '审批自愈动作',
}

export default function ActionApprovalCard({
  action,
  onChanged,
}: {
  action: ActionRequest
  onChanged?: () => void
}) {
  const [deciding, setDeciding] = useState(false)
  const isPending = action.status === 'pending'

  const decide = async (decision: 'confirm' | 'reject') => {
    setDeciding(true)
    try {
      await api.assistantActionDecision(action.id, decision)
      message.success(decision === 'confirm' ? '动作已确认并执行' : '动作已拒绝')
      onChanged?.()
    } catch (err) {
      message.error(`操作失败：${(err as Error).message}`)
    } finally {
      setDeciding(false)
    }
  }

  const icon =
    action.status === 'pending' ? (
      <ClockCircleOutlined style={{ color: '#fa8c16' }} />
    ) : action.status === 'succeeded' ? (
      <CheckCircleOutlined style={{ color: '#52c41a' }} />
    ) : action.status === 'failed' || action.status === 'expired' ? (
      <ExclamationCircleOutlined style={{ color: '#f5222d' }} />
    ) : (
      <StopOutlined style={{ color: '#999' }} />
    )

  return (
    <div
      style={{
        border: isPending ? '1px solid #fa8c16' : '1px solid #f0f0f0',
        background: isPending ? '#fffbe6' : '#fafafa',
        borderRadius: 10, padding: '10px 14px', marginBottom: 10, fontSize: 13,
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        {icon}
        <SafetyOutlined />
        <b>{toolLabel[action.toolName] || action.toolName}</b>
        <Tag color={riskColor[action.riskLevel] || 'default'} style={{ marginInlineEnd: 0 }}>
          {action.riskLevel === 'high' ? '高风险' : action.riskLevel === 'medium' ? '中风险' : '低风险'}
        </Tag>
        <Tag color={isPending ? 'orange' : 'default'} style={{ marginInlineEnd: 0 }}>
          {statusText[action.status] || action.status}
        </Tag>
        {action.decidedBy && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            决策人：{action.decidedBy}
          </Typography.Text>
        )}
      </div>
      <div style={{ marginTop: 6, color: '#666' }}>
        <div style={{ wordBreak: 'break-all' }}>
          目标：{JSON.stringify(action.arguments ?? {})}
        </div>
        {action.decisionReason && (
          <div style={{ marginTop: 4, color: action.status === 'failed' ? '#f5222d' : '#888' }}>
            {action.status === 'failed' ? '失败原因：' : '说明：'}{action.decisionReason}
          </div>
        )}
      </div>
      {isPending && (
        <div style={{ marginTop: 8, display: 'flex', gap: 8 }}>
          <Popconfirm
            title="确认执行该动作？"
            description="动作将在确认后立即执行，且不可撤销。"
            onConfirm={() => decide('confirm')}
          >
            <Button size="small" type="primary" danger={action.riskLevel === 'high'} loading={deciding}>
              确认执行
            </Button>
          </Popconfirm>
          <Popconfirm title="拒绝该动作？" description="拒绝后不会执行任何操作。" onConfirm={() => decide('reject')}>
            <Button size="small" loading={deciding}>拒绝</Button>
          </Popconfirm>
        </div>
      )}
    </div>
  )
}
