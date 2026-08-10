import { Collapse, Tag } from 'antd'
import { CheckCircleOutlined, CloseCircleOutlined, LoadingOutlined, ToolOutlined } from '@ant-design/icons'
import type { ToolStepState } from '../../assistant/streamReducer'

// 检索类工具：结果 JSON 透出 degradedReason（向量降级等）→ 展示提示。
const KNOWLEDGE_TOOLS = new Set(['search_knowledge', 'get_similar_incidents'])

function degradedOf(tool: ToolStepState): string | undefined {
  if (!KNOWLEDGE_TOOLS.has(tool.name) || !tool.resultSummary) return undefined
  try {
    const parsed = JSON.parse(String(tool.resultSummary))
    return parsed?.degradedReason || undefined
  } catch {
    return undefined
  }
}

// ToolStepCard 工具调用折叠卡（名称/参数摘要/状态；失败红色标注；检索降级橙色提示）。
export default function ToolStepCard({ tool }: { tool: ToolStepState }) {
  const icon =
    tool.status === 'running' ? (
      <LoadingOutlined />
    ) : tool.status === 'ok' ? (
      <CheckCircleOutlined style={{ color: '#52c41a' }} />
    ) : (
      <CloseCircleOutlined style={{ color: '#f5222d' }} />
    )
  const degraded = degradedOf(tool)
  return (
    <Collapse
      size="small"
      ghost
      style={{ marginBottom: 6, background: '#fafafa', borderRadius: 8 }}
      items={[
        {
          key: tool.name,
          label: (
            <span style={{ fontSize: 13 }}>
              {icon} <ToolOutlined /> {tool.name}
              {degraded && (
                <Tag color="orange" style={{ marginLeft: 8, fontSize: 12 }}>
                  检索降级：{degraded}
                </Tag>
              )}
              {tool.status === 'failed' && tool.error && (
                <span style={{ color: '#f5222d', marginLeft: 8 }}>{tool.error}</span>
              )}
            </span>
          ),
          children: (
            <div style={{ fontSize: 12, color: '#666' }}>
              {tool.args && <div style={{ marginBottom: 4 }}>参数：{tool.args}</div>}
              {tool.resultSummary && (
                <div style={{ whiteSpace: 'pre-wrap' }}>结果：{tool.resultSummary}</div>
              )}
            </div>
          ),
        },
      ]}
    />
  )
}
