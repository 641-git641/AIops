import { Tag } from 'antd'
import type { AssistantRun } from '../../assistant/types'

const statusMap: Record<string, { color: string; label: string }> = {
  queued: { color: 'default', label: '排队中' },
  running: { color: 'processing', label: '运行中' },
  completed: { color: 'success', label: '已完成' },
  failed: { color: 'error', label: '失败' },
  cancelled: { color: 'warning', label: '已取消' },
}

// RunStatus 当前 run 状态条（状态 + 模型 + 耗时/错误码）。
export default function RunStatus({ run }: { run?: AssistantRun }) {
  if (!run) return null
  const s = statusMap[run.status] ?? { color: 'default', label: run.status }
  return (
    <div style={{ padding: '6px 16px', borderBottom: '1px solid #f0f0f0', fontSize: 13, color: '#666' }}>
      <Tag color={s.color}>{s.label}</Tag>
      <span style={{ marginRight: 12 }}>模型：{run.model}</span>
      {run.status === 'completed' && run.latencyMs !== undefined && <span>耗时 {run.latencyMs}ms</span>}
      {run.status === 'failed' && run.errorCode && <span style={{ color: '#f5222d' }}>{run.errorCode}</span>}
    </div>
  )
}
