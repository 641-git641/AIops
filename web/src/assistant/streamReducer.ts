// assistant/streamReducer.ts：SSE 事件流的确定性 reducer（AGENT_CHAT_PLAN §11.3）。
// 按 (runId, sequence) 去重（乱序/重复/重连幂等）；message.delta 50-100ms 合并后落 UI；
// 终止事件收束流式状态。React Query 只管 REST 快照，SSE 增量只进此 reducer。

import type { ActionRequest, Citation, SSEEvent } from './types'

export interface StreamState {
  runId: string
  status: 'running' | 'completed' | 'failed' | 'cancelled'
  content: string // 当前 run 的流式文本（最终并入稳定消息）
  tools: ToolStepState[]
  citations: Citation[] // 本轮命中证据（citation.added 增量聚合）
  actions: ActionRequest[] // 本轮动作请求（action.required 增量聚合；resolved 更新状态）
  error?: string
  lastSequence: number // 已消费的最大 sequence（断线重连带此值）
  totalTokens?: number
}

export interface ToolStepState {
  name: string
  args?: string
  status: 'running' | 'ok' | 'failed'
  resultSummary?: string
  error?: string
}

export type StreamAction =
  | { type: 'reset'; runId: string }
  | { type: 'event'; event: SSEEvent }
  | { type: 'finish'; status: 'completed' | 'failed' | 'cancelled'; error?: string }

export function initialState(runId: string): StreamState {
  return { runId, status: 'running', content: '', tools: [], citations: [], actions: [], lastSequence: 0 }
}

export function streamReducer(state: StreamState, action: StreamAction): StreamState {
  switch (action.type) {
    case 'reset':
      return initialState(action.runId)
    case 'event': {
      const ev = action.event
      if (ev.sequence <= state.lastSequence) return state // 乱序/重复跳过
      let next = { ...state, lastSequence: ev.sequence }
      switch (ev.type) {
        case 'message.delta':
          next.content += ev.data?.content ?? ''
          break
        case 'tool.started':
          next.tools = [...next.tools, { name: ev.data?.tool ?? '?', args: ev.data?.args, status: 'running' }]
          break
        case 'tool.completed': {
          const i = next.tools.findIndex((t) => t.name === ev.data?.tool)
          if (i >= 0) {
            const tools = [...next.tools]
            tools[i] = { ...tools[i], status: 'ok', resultSummary: ev.data?.resultSummary }
            next.tools = tools
          }
          break
        }
        case 'tool.failed': {
          const i = next.tools.findIndex((t) => t.name === ev.data?.tool)
          if (i >= 0) {
            const tools = [...next.tools]
            tools[i] = { ...tools[i], status: 'failed', error: ev.data?.error }
            next.tools = tools
          }
          break
        }
        case 'citation.added': {
          const c = ev.data?.citation
          if (c) {
            // 按 (sourceType,sourceId) 去重（同一证据跨工具轮可能重复命中）
            const dup = next.citations.some((x) => x.sourceType === c.sourceType && x.sourceId === c.sourceId)
            if (!dup) next.citations = [...next.citations, c]
          }
          break
        }
        case 'action.required': {
          const ar = ev.data?.actionRequest
          if (ar && !next.actions.some((a) => a.id === ar.id)) {
            next.actions = [...next.actions, ar]
          }
          break
        }
        case 'action.resolved': {
          const ar = ev.data?.actionRequest
          if (ar) {
            next.actions = next.actions.map((a) => (a.id === ar.id ? ar : a))
          }
          break
        }
        case 'usage.updated':
          next.totalTokens = ev.data?.totalTokens
          break
        case 'run.completed':
        case 'run.failed':
        case 'run.cancelled':
          next.status = ev.type === 'run.completed' ? 'completed' : ev.type === 'run.failed' ? 'failed' : 'cancelled'
          next.error = ev.data?.error
          break
        case 'message.completed':
          // 最终消息内容已在 delta 中累计；若携带 citations 以最终版为准（工具轮去重后的全集）
          if (ev.data?.citations?.length) next.citations = ev.data.citations
          break
        default:
          break // heartbeat / run.started 忽略
      }
      return next
    }
    case 'finish':
      return { ...state, status: action.status, error: action.error }
    default:
      return state
  }
}
