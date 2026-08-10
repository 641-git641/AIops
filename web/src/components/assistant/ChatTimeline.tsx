import { lazy, Suspense, useState } from 'react'
import { Button, Spin, Tag, Tooltip, Typography, message } from 'antd'
import { CheckOutlined, CloseOutlined, LinkOutlined, RobotOutlined, UserOutlined } from '@ant-design/icons'
import { useNavigate } from 'react-router-dom'
import { api } from '../../api'
import type { ActionRequest, Citation } from '../../assistant/types'
import type { AssistantMessage } from '../../assistant/types'
import type { StreamState } from '../../assistant/streamReducer'
import ActionApprovalCard from './ActionApprovalCard'
import ToolStepCard from './ToolStepCard'

// Markdown 渲染按需加载（AGENT_CHAT_PLAN §11.3：重组件懒加载；react-markdown 默认不渲染 raw HTML）。
const Markdown = lazy(() => import('react-markdown'))

// citationHref 引用跳转路由（后端生成 url 优先；否则按 sourceType 兜底）。
function citationHref(c: Citation): string {
  if (c.url) return c.url
  if (c.sourceType === 'incident') return `/incidents/${c.sourceId}`
  if (c.sourceType === 'playbook') return '/playbooks'
  return '/knowledge'
}

// CitationLinks 引用卡（来源链接 + 可信等级；untrusted 数据仅展示）。
function CitationLinks({ citations }: { citations: Citation[] }) {
  const navigate = useNavigate()
  return (
    <div style={{ marginTop: 8, display: 'flex', flexWrap: 'wrap', gap: 6 }}>
      {citations.map((c, i) => (
        <Tooltip key={`${c.sourceType}-${c.sourceId}-${i}`} title={`来源：${c.sourceType} · ${c.trustLevel ?? ''}`}>
          <Button
            size="small"
            type="dashed"
            icon={<LinkOutlined />}
            onClick={(e) => {
              e.stopPropagation()
              navigate(citationHref(c))
            }}
            style={{ fontSize: 12 }}
          >
            {c.title || c.sourceId}
            {c.trustLevel && <span style={{ color: '#999', marginLeft: 4 }}>{c.trustLevel}</span>}
          </Button>
        </Tooltip>
      ))}
    </div>
  )
}

// FeedbackButtons 回答反馈（有用/没用；只对最终 assistant 消息出现一次）。
function FeedbackButtons({ msg }: { msg: AssistantMessage }) {
  const [rated, setRated] = useState<'useful' | 'not_useful' | undefined>()
  if (!msg.citations && !msg.content) return null
  const submit = async (rating: 'useful' | 'not_useful') => {
    setRated(rating)
    try {
      await api.assistantFeedback(msg.id, rating)
    } catch {
      message.error('反馈提交失败')
      setRated(undefined)
    }
  }
  return (
    <div style={{ marginTop: 6, fontSize: 12, color: '#999' }}>
      {!rated ? (
        <>
          <span style={{ marginRight: 6 }}>这个回答有帮助吗？</span>
          <Button size="small" type="text" icon={<CheckOutlined />} onClick={() => submit('useful')}>
            有用
          </Button>
          <Button size="small" type="text" icon={<CloseOutlined />} onClick={() => submit('not_useful')}>
            没用
          </Button>
        </>
      ) : (
        <span>{rated === 'useful' ? '已标记有用 ✓' : '已标记没用'}</span>
      )}
    </div>
  )
}

// MessageBubble 单条消息（用户/助手；助手消息 markdown 渲染 + 引用卡 + 反馈）。
function MessageBubble({ msg }: { msg: AssistantMessage }) {
  const isUser = msg.role === 'user'
  return (
    <div style={{ display: 'flex', gap: 10, marginBottom: 14, justifyContent: isUser ? 'flex-end' : 'flex-start' }}>
      {!isUser && (
        <div style={{ width: 30, height: 30, borderRadius: 15, background: '#1677ff', color: '#fff', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
          <RobotOutlined />
        </div>
      )}
      <div style={{ maxWidth: '78%' }}>
        <div
          style={{
            background: isUser ? '#1677ff' : '#f5f5f5',
            color: isUser ? '#fff' : 'rgba(0,0,0,0.88)',
            padding: '10px 14px',
            borderRadius: 10,
            fontSize: 14,
            lineHeight: 1.7,
            wordBreak: 'break-word',
          }}
        >
          {isUser ? (
            msg.content
          ) : (
            <Suspense fallback={<Spin size="small" />}>
              <Markdown>{msg.content}</Markdown>
            </Suspense>
          )}
        </div>
        {!isUser && msg.citations && msg.citations.length > 0 && <CitationLinks citations={msg.citations} />}
        {!isUser && <FeedbackButtons msg={msg} />}
      </div>
      {isUser && (
        <div style={{ width: 30, height: 30, borderRadius: 15, background: '#52c41a', color: '#fff', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
          <UserOutlined />
        </div>
      )}
    </div>
  )
}

// StreamCitations 流式中实时出现的引用（完成后并入稳定消息）。
function StreamCitations({ citations }: { citations: Citation[] }) {
  if (citations.length === 0) return null
  return (
    <div style={{ marginBottom: 12, display: 'flex', flexWrap: 'wrap', gap: 6, alignItems: 'center' }}>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        引用来源：
      </Typography.Text>
      {citations.map((c, i) => (
        <Tag key={`${c.sourceType}-${c.sourceId}-${i}`} color="blue" style={{ fontSize: 12 }}>
          {c.title || c.sourceId}
        </Tag>
      ))}
    </div>
  )
}

// ActionCards 动作请求卡（历史已处理 + 流式新增合并；按 id 去重）。
function ActionCards({
  history,
  streamActions,
  onChanged,
}: {
  history: ActionRequest[]
  streamActions: ActionRequest[]
  onChanged: () => void
}) {
  const merged = new Map<string, ActionRequest>()
  for (const a of history) merged.set(a.id, a)
  for (const a of streamActions) merged.set(a.id, a) // 流式状态优先（最新事件覆盖）
  if (merged.size === 0) return null
  return (
    <div style={{ marginBottom: 12 }}>
      {[...merged.values()].map((a) => (
        <ActionApprovalCard key={a.id} action={a} onChanged={onChanged} />
      ))}
    </div>
  )
}

// ChatTimeline 消息流 + 流式增量（临时态）+ 工具步骤卡 + 动作确认卡。
export default function ChatTimeline({
  messages,
  stream,
  actions = [],
  onActionsChanged,
}: {
  messages: AssistantMessage[]
  stream: StreamState
  actions?: ActionRequest[] // 线程历史动作请求（页面刷新恢复）
  onActionsChanged?: () => void
}) {
  const streaming = stream.status === 'running'
  return (
    <div style={{ flex: 1, overflow: 'auto', padding: '16px 20px' }}>
      {messages.length === 0 && !streaming && (
        <div style={{ color: '#999', textAlign: 'center', marginTop: 60 }}>
          <div style={{ fontSize: 40, marginBottom: 8 }}>🤖</div>
          <Typography.Text type="secondary">问我平台状态、告警、事故或拓扑影响；也能问知识库与相似历史事故</Typography.Text>
        </div>
      )}
      {messages.map((m) => (
        <MessageBubble key={m.id} msg={m} />
      ))}
      {stream.tools.length > 0 && (
        <div style={{ marginBottom: 12 }}>
          {stream.tools.map((t, i) => (
            <ToolStepCard key={`${t.name}-${i}`} tool={t} />
          ))}
        </div>
      )}
      <ActionCards
        history={actions}
        streamActions={stream.actions}
        onChanged={onActionsChanged ?? (() => {})}
      />
      {streaming && <StreamCitations citations={stream.citations} />}
      {streaming && (
        <div style={{ display: 'flex', gap: 10, alignItems: 'flex-start' }}>
          <div style={{ width: 30, height: 30, borderRadius: 15, background: '#1677ff', color: '#fff', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
            <RobotOutlined />
          </div>
          <div style={{ background: '#f5f5f5', padding: '10px 14px', borderRadius: 10, fontSize: 14, lineHeight: 1.7, whiteSpace: 'pre-wrap', maxWidth: '78%' }}>
            {stream.content || <Spin size="small" />}
          </div>
        </div>
      )}
      {stream.status === 'failed' && stream.error && (
        <Typography.Text type="danger" style={{ display: 'block', marginTop: 8 }}>
          运行失败：{stream.error}
        </Typography.Text>
      )}
    </div>
  )
}
