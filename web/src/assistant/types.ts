// assistant/types.ts：对话式 Agent 前端类型（与后端 internal/assistant DTO 对齐）。

export interface AssistantThread {
  id: string
  tenantId: string
  title: string
  status: string
  contextType?: string // "" | incident（绑定上下文，Phase 2）
  contextId?: string
  createdAt: string
  updatedAt: string
}

// Citation 回答引用（来源可点跳转；untrusted 展示数据）。
export interface Citation {
  sourceType: string // incident | playbook | runbook | knowledge
  sourceId: string
  title: string
  trustLevel?: string
  time?: string
  url?: string
}

export interface AssistantMessage {
  id: string
  threadId: string
  runId?: string
  role: 'user' | 'assistant' | 'system' | 'tool'
  content: string
  contentFormat: string
  sequence: number
  status: string
  citations?: Citation[]
  createdAt: string
}

export interface AssistantRun {
  id: string
  threadId: string
  status: 'queued' | 'running' | 'completed' | 'failed' | 'cancelled'
  model: string
  stepCount: number
  errorCode?: string
  latencyMs?: number
  createdAt: string
  updatedAt: string
}

export interface AssistantStep {
  id: string
  runId: string
  ordinal: number
  kind: 'model' | 'tool_call' | 'tool_result' | 'policy'
  toolName?: string
  arguments?: unknown
  resultSummary?: unknown
  status: 'ok' | 'failed' | 'rejected'
  durationMs?: number
  errorCode?: string
}

export interface SendMessageResult {
  threadId: string
  messageId: string
  runId: string
}

// ---- SSE 事件（与后端 assistant.Event 对齐） ----

export interface SSEEvent {
  runId: string
  sequence: number
  type:
    | 'run.started'
    | 'run.completed'
    | 'run.failed'
    | 'run.cancelled'
    | 'message.delta'
    | 'message.completed'
    | 'tool.started'
    | 'tool.completed'
    | 'tool.failed'
    | 'citation.added'
    | 'usage.updated'
    | 'heartbeat'
  data?: {
    content?: string
    tool?: string
    args?: string
    error?: string
    messageId?: string
    resultSummary?: string
    status?: string
    latencyMs?: number
    totalTokens?: number
    citation?: Citation
    citations?: Citation[] // message.completed 携带最终引用
  }
  timestamp: string
}

// 会话列表响应。
export interface ThreadListResponse {
  items: AssistantThread[]
}

// run 详情响应。
export interface RunDetailResponse {
  run: AssistantRun
  steps: AssistantStep[]
}
