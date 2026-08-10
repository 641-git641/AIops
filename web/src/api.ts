// REST 客户端：所有请求走 /api/v1（Vite 代理 → :8080）。
export interface AlertDTO {
  id: string
  provider: string
  name: string
  severity: string
  status: string
  labels: Record<string, unknown>
  event?: Record<string, unknown>
  fingerprint: string
  startedAt: string
  lastSeenAt: string
}

export interface Incident {
  id: string
  tenantId: string
  name: string
  severity: string
  status: string
  source: string
  summary?: string
  aiGeneratedName?: string
  llmSummary?: string
  createdAt: string
}

export interface IncidentState {
  status: string
  currentAgent: string
  retryCount: number
  errorMessage: string
}

export interface IncidentDetail extends Incident {
  alerts: AlertDTO[]
  state?: IncidentState
  nodeResults?: Record<string, any>
  resolution?: Resolution
}

// 结案→发布→索引三态（KEEPON M3）。
export interface KnowledgeState {
  resolved: boolean
  resolutionFilled: boolean
  indexed: boolean
  resolutionVersion?: number
  resolutionUpdatedAt?: string
  document?: {
    documentId: string
    title: string
    trustLevel: string
    verificationStatus: string
    status: string // active|superseded|deleted|failed|quarantined
    sourceVersion: number
    updatedAt: string
  }
  job?: {
    operation: string
    status: string
    sourceVersion: number
    attempts: number
    lastError?: string
    updatedAt: string
  }
}

// 结案数据契约（RAG Phase 0）：人工确认根因与处置结果。
export interface Resolution {
  id: string
  incidentId: string
  confirmedRootCause: string
  resolution: string
  outcome: string // success|partial|failed|false_positive
  verification: string
  verificationStatus: string // verified|unverified|rejected
  postmortemUrl?: string
  resolvedBy?: string
  resolvedAt: string
  version: number
}

export interface SimilarItem {
  documentId: string
  chunkId: string
  sourceType: string // incident|playbook
  sourceId: string
  title: string
  section: string
  content?: string
  service?: string
  severity?: string
  trustLevel: string // T0|T1|T2|T3
  time: string
  score: number
  reasons: string[]
  confirmedRootCause?: string
  resolution?: string
  outcome?: string
  verificationStatus?: string
}

export interface SimilarResponse {
  enabled: boolean
  traceId?: string
  degradedReason?: string
  latencyMs?: Record<string, number>
  items: SimilarItem[]
}

export interface Playbook {
  id: string
  name: string
  level: string // L0|L1|L2
  commandTemplate: string
  rollbackCommand: string
  blastRadiusFactor: number
  dryRunFirst: boolean
  enabled: boolean
  createdAt: string
}

export interface MaintenanceWindow {
  id: string
  name: string
  fingerprint: string
  service: string
  severity: string
  startsAt: string // RFC3339
  endsAt: string
  reason: string
  enabled: boolean
  createdAt: string
}

export interface AuditLog {
  id: string
  incidentId: string
  correlationId: string
  actor: string
  action: string // approval.granted | heal.executed | ...
  target: string
  reason: string
  metadata?: Record<string, any>
  createdAt: string
}

export interface Paged<T> {
  items: T[]
  total: number
  limit: number
  offset: number
}

export interface AiClusterResult {
  scanned: number
  clusters: number
  created: Incident[]
  merged: number
  skipped: number
}

export interface Workflow {
  id: string
  name: string
  description: string
  yaml: string
  enabled: boolean
  createdAt: string
  updatedAt: string
}

export interface WorkflowRun {
  id: string
  workflowId: string
  dedupKey: string
  trigger: string // alert|interval|manual
  status: string // running|succeeded|failed
  result?: Record<string, any>
  error?: string
  startedAt: string
  finishedAt: string
}

export interface WorkflowRunResult {
  runId: string
  workflowId: string
  trigger: string
  status: string
}

export interface NotifyChannel {
  name: string
  enabled: boolean
  webhook: string
  updatedAt?: string
}

export interface NotifyTestResult {
  channel: string
  ok: boolean
  error?: string
}

export interface TopologyService {
  id: string
  tenantId: string
  name: string
  kind: string
  dependsOn?: string[]
  recentChanges?: string[]
  impactScore: number
  createdAt: string
  updatedAt: string
}

export interface TopologyImpactNode {
  service: string
  depth: number
  path: string[] // 从被查服务出发的依赖链（首元素即直接受影响者）
}

export interface TopologyImpact {
  service: string
  depth: number
  affected: TopologyImpactNode[]
  total: number
}

// ---- 访问保护（KEEPON 4.3）：凭据仅存 sessionStorage，REST 统一携带 X-API-Key ----

const AUTH_KEY = 'aiops.apiKey'

/** 当前访问凭据（无则空串）。 */
export function getAPIKey(): string {
  return sessionStorage.getItem(AUTH_KEY) ?? ''
}

/** 解锁成功后保存凭据（仅当前标签页会话）。 */
export function setAPIKey(key: string): void {
  sessionStorage.setItem(AUTH_KEY, key)
}

/** 凭据失效（401）时清除并跳解锁页。 */
export function clearAPIKey(): void {
  sessionStorage.removeItem(AUTH_KEY)
}

export async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  const key = getAPIKey()
  if (key) headers['X-API-Key'] = key
  const res = await fetch(`/api/v1${path}`, { headers, ...init })
  if (res.status === 401 && !path.startsWith('/auth/')) {
    clearAPIKey()
    if (!location.pathname.startsWith('/unlock')) window.location.assign('/unlock')
    throw new Error(`401: 访问凭据无效，请重新解锁`)
  }
  if (!res.ok) {
    const body = await res.text().catch(() => res.statusText)
    throw new Error(`${res.status}: ${body}`)
  }
  return res.json() as Promise<T>
}

export const api = {
  alerts: (params: Record<string, string> = {}) =>
    req<Paged<AlertDTO>>(`/alerts?${new URLSearchParams(params)}`),

  alert: (id: string) => req<AlertDTO>(`/alerts/${id}`),

  incidents: (params: Record<string, string> = {}) =>
    req<Paged<Incident>>(`/incidents?${new URLSearchParams(params)}`),

  incident: (id: string) => req<IncidentDetail>(`/incidents/${id}`),

  resolveIncident: (id: string) => req<{ id: string; status: string }>(`/incidents/${id}/resolve`, { method: 'POST' }),

  // 结案数据（RAG Phase 0）：已解决事故填写/更新确认根因与处置结果
  saveResolution: (id: string, r: Partial<Resolution>) =>
    req<{ id: string; version: number; verificationStatus: string }>(`/incidents/${id}/resolution`, {
      method: 'PUT',
      body: JSON.stringify(r),
    }),

  // RAG 相似历史事故（只读；RAG 关闭时返回 enabled=false）
  similarIncidents: (id: string) => req<SimilarResponse>(`/incidents/${id}/similar`),

  // RAG 检索反馈（useful|not_useful|wrong|unsafe；unsafe 触发文档 quarantine）
  ragFeedback: (id: string, fb: { rating: string; documentId: string; traceId?: string; correctedRootCause?: string }) =>
    req<{ id: string; rating: string; quarantined: boolean }>(`/incidents/${id}/rag-feedback`, {
      method: 'POST',
      body: JSON.stringify(fb),
    }),

  approveIncident: (id: string, decision: 'approve' | 'reject', reason: string) =>
    req<{ id: string; decision: string; status: string }>(`/incidents/${id}/approval`, {
      method: 'POST',
      body: JSON.stringify({ decision, reason }),
    }),

  audits: (incidentId: string) => req<Paged<AuditLog>>(`/incidents/${incidentId}/audits`),

  notifyTest: (title: string, content: string, severity: string) =>
    req<{ channels: string[]; results: NotifyTestResult[] }>('/notify/test', {
      method: 'POST',
      body: JSON.stringify({ title, content, severity }),
    }),

  notifyChannels: () => req<Paged<NotifyChannel>>('/notify/channels'),

  saveNotifyChannel: (name: string, ch: { enabled: boolean; webhook: string }) =>
    req<NotifyChannel>(`/notify/channels/${name}`, { method: 'PUT', body: JSON.stringify(ch) }),

  testNotifyChannel: (name: string) =>
    req<NotifyTestResult>(`/notify/channels/${name}/test`, { method: 'POST' }),

  playbooks: () => req<Paged<Playbook>>('/playbooks'),

  createPlaybook: (pb: Partial<Playbook>) =>
    req<Playbook>('/playbooks', { method: 'POST', body: JSON.stringify(pb) }),

  updatePlaybook: (id: string, pb: Partial<Playbook>) =>
    req<Playbook>(`/playbooks/${id}`, { method: 'PUT', body: JSON.stringify(pb) }),

  deletePlaybook: (id: string) =>
    req<{ id: string; deleted: boolean }>(`/playbooks/${id}`, { method: 'DELETE' }),

  maintenanceWindows: () => req<Paged<MaintenanceWindow>>('/maintenance-windows'),

  aiCluster: () => req<AiClusterResult>('/ai/cluster', { method: 'POST' }),

  workflows: () => req<Paged<Workflow>>('/workflows'),

  workflowRuns: (params: Record<string, string> = {}) =>
    req<Paged<WorkflowRun>>(`/workflow-runs?${new URLSearchParams(params)}`),

  createWorkflow: (w: Partial<Workflow>) =>
    req<Workflow>('/workflows', { method: 'POST', body: JSON.stringify(w) }),

  updateWorkflow: (id: string, w: Partial<Workflow>) =>
    req<Workflow>(`/workflows/${id}`, { method: 'PUT', body: JSON.stringify(w) }),

  deleteWorkflow: (id: string) =>
    req<{ id: string; deleted: boolean }>(`/workflows/${id}`, { method: 'DELETE' }),

  runWorkflow: (id: string, alertId?: string) =>
    req<WorkflowRunResult>(`/workflows/${id}/run`, {
      method: 'POST',
      body: JSON.stringify({ alert_id: alertId ?? '' }),
    }),

  createMaintenanceWindow: (w: Partial<MaintenanceWindow>) =>
    req<MaintenanceWindow>('/maintenance-windows', { method: 'POST', body: JSON.stringify(w) }),

  updateMaintenanceWindow: (id: string, w: Partial<MaintenanceWindow>) =>
    req<MaintenanceWindow>(`/maintenance-windows/${id}`, { method: 'PUT', body: JSON.stringify(w) }),

  deleteMaintenanceWindow: (id: string) =>
    req<{ id: string; deleted: boolean }>(`/maintenance-windows/${id}`, { method: 'DELETE' }),

  rules: () => req<Paged<unknown>>('/rules'),

  providers: () => req<{ items: string[]; total: number }>('/providers'),

  topology: () => req<Paged<TopologyService>>('/topology'),

  topologyImpact: (name: string, depth = 5) =>
    req<TopologyImpact>(`/topology/${encodeURIComponent(name)}/impact?depth=${depth}`),

  ackAlert: (id: string) => req<AlertDTO>(`/alerts/${id}/ack`, { method: 'POST' }),

  // 大模型 API 接入管理（管理权限 AIOPS_MODELCFGADMINAPIS）
  modelConfigs: () => req<Paged<ModelConfig>>('/model-configs'),

  saveModelConfig: (key: string, c: ModelConfigSave) =>
    req<ModelConfig>(`/model-configs/${key}`, { method: 'PUT', body: JSON.stringify(c) }),

  testModelConfig: (key: string, c: ModelConfigSave) =>
    req<ModelTestResult>(`/model-configs/${key}/test`, { method: 'POST', body: JSON.stringify(c) }),

  // 访问保护（KEEPON 4.3）
  authStatus: () => req<AuthStatus>('/auth/status'),

  wsTicket: () => req<WSTicket>('/ws/ticket'),

  apiKeys: () => req<Paged<APIKeyItem>>('/apikeys'),

  createAPIKey: (name: string) =>
    req<{ apiKey: string; tenantId: string }>('/apikeys', { method: 'POST', body: JSON.stringify({ name }) }),

  deleteAPIKey: (id: string) => req<{ deleted: string }>(`/apikeys/${id}`, { method: 'DELETE' }),

  regenerateOwner: () =>
    req<{ apiKey: string; tenantId: string; isOwner: boolean }>('/apikeys/owner/regenerate', { method: 'POST' }),

  // 数据备份（KEEPON 4.1；恢复仅限脚本，不做网页按钮）
  backups: () => req<Paged<BackupRun>>('/backups'),

  triggerBackup: () => req<BackupRun>('/backups', { method: 'POST' }),

  // 总览聚合（KEEPON 5.1；默认首页）
  overview: (window = '24h') => req<Overview>(`/overview?window=${window}`),

  // 结案→发布→索引三态（KEEPON M3）
  knowledgeStatus: (id: string) => req<KnowledgeState>(`/incidents/${id}/knowledge-status`),

  // 通用告警入口（KEEPON 5.2）：发送测试告警（接入向导按钮）
  sendTestAlert: () =>
    req<WebhookResult>(`/webhook/generic`, {
      method: 'POST',
      body: JSON.stringify({
        title: '测试告警', severity: 'warning', status: 'firing', source: 'web-ui',
        labels: { service: 'test' }, description: '来自接入向导的测试告警',
      }),
    }),

  // 检索 golden set 评测（KEEPON M3；管理权限）
  goldenQueries: (enabledOnly = false) =>
    req<Paged<GoldenQuery>>(`/rag/golden-queries${enabledOnly ? '?enabled=1' : ''}`),

  createGoldenQuery: (g: Partial<GoldenQuery>) =>
    req<GoldenQuery>('/rag/golden-queries', { method: 'POST', body: JSON.stringify(g) }),

  updateGoldenQuery: (id: string, g: Partial<GoldenQuery>) =>
    req<GoldenQuery>(`/rag/golden-queries/${id}`, { method: 'PUT', body: JSON.stringify(g) }),

  deleteGoldenQuery: (id: string) =>
    req<{ deleted: string }>(`/rag/golden-queries/${id}`, { method: 'DELETE' }),

  // 个人知识源（KEEPON M3；管理权限）
  sourcePreview: (name: string, content: string) =>
    req<SourcePreview>('/rag/sources/preview', { method: 'POST', body: JSON.stringify({ name, content }) }),

  sources: () => req<Paged<SourceRow>>('/rag/sources'),

  source: (id: string) => req<KnowledgeSource>(`/rag/sources/${id}`),

  createSource: (name: string, content: string) =>
    req<KnowledgeSource>('/rag/sources', { method: 'POST', body: JSON.stringify({ name, content }) }),

  updateSource: (id: string, name: string, content: string) =>
    req<KnowledgeSource>(`/rag/sources/${id}`, { method: 'PUT', body: JSON.stringify({ name, content }) }),

  publishSource: (id: string) =>
    req<KnowledgeSource>(`/rag/sources/${id}/publish`, { method: 'POST' }),

  deleteSource: (id: string) =>
    req<{ deleted: string }>(`/rag/sources/${id}`, { method: 'DELETE' }),

  ragStatus: () => req<Record<string, any>>('/rag/status'),

  // 基础设施指标（监控平板：主机/PostgreSQL/docker 容器）
  infraMetrics: () => req<InfraSnapshot>('/infra/metrics'),

  // ---- 对话式 Agent（AGENT_CHAT_PLAN；功能关闭时 404） ----
  assistantThreads: () => req<{ items: AssistantThread[] }>('/assistant/threads'),
  assistantCreateThread: (title: string, context?: { contextType: string; contextId: string }) =>
    req<AssistantThread>('/assistant/threads', {
      method: 'POST',
      body: JSON.stringify({ title, ...(context ?? {}) }),
    }),
  assistantThread: (id: string) => req<AssistantThread>(`/assistant/threads/${id}`),
  assistantMessages: (threadId: string) =>
    req<{ items: AssistantMessage[] }>(`/assistant/threads/${threadId}/messages`),
  assistantDeleteThread: (id: string) =>
    req<unknown>(`/assistant/threads/${id}`, { method: 'DELETE' }),
  assistantFeedback: (messageId: string, rating: 'useful' | 'not_useful', note?: string) =>
    req<unknown>(`/assistant/messages/${messageId}/feedback`, {
      method: 'POST',
      body: JSON.stringify({ rating, note: note ?? '' }),
    }),
  // ---- 异常检测（KEEPON 七：告警数量季节性基线，只读提示） ----
  alertAnomalies: (hours = 24, historyDays = 7) =>
    req<{ items: AlertAnomaly[] }>(`/anomaly/alerts?hours=${hours}&historyDays=${historyDays}`),

  // ---- 外部可观测系统（KEEPON 6.2） ----
  externalSystems: () => req<{ items: ExternalSystem[] }>('/external-systems'),
  createExternalSystem: (input: Partial<ExternalSystem>) =>
    req<ExternalSystem>('/external-systems', { method: 'POST', body: JSON.stringify(input) }),
  updateExternalSystem: (id: string, input: Partial<ExternalSystem>) =>
    req<ExternalSystem>(`/external-systems/${id}`, { method: 'PUT', body: JSON.stringify(input) }),
  deleteExternalSystem: (id: string) =>
    req<unknown>(`/external-systems/${id}`, { method: 'DELETE' }),

  assistantThreadActions: (threadId: string) =>
    req<{ items: AssistantActionRequest[] }>(`/assistant/threads/${threadId}/actions`),
  assistantActionDecision: (actionId: string, decision: 'confirm' | 'reject', reason?: string) =>
    req<AssistantActionRequest>(`/assistant/actions/${actionId}/${decision}`, {
      method: 'POST',
      body: JSON.stringify({ reason: reason ?? '' }),
    }),
  assistantSendMessage: (threadId: string, content: string, idemKey: string) =>
    req<SendMessageResult>(`/assistant/threads/${threadId}/messages`, {
      method: 'POST',
      headers: { 'Idempotency-Key': idemKey },
      body: JSON.stringify({ content }),
    }),
  assistantRun: (runId: string) => req<RunDetailResponse>(`/assistant/runs/${runId}`),
  assistantCancelRun: (runId: string) =>
    req<unknown>(`/assistant/runs/${runId}/cancel`, { method: 'POST' }),
}

// ---- 基础设施指标类型 ----

export interface InfraSnapshot {
  host: {
    cpuPercent: number
    memTotalMb: number
    memUsedMb: number
    memPercent: number
    processCpuPercent: number
    goHeapMb: number
    goRoutines: number
    diskFreeGb: number
    uptimeSec: number
  }
  pg: {
    connections: number
    activeQueries: number
    xactCommit: number
    xactRollback: number
    deadlocks: number
    cacheHitRatio: number
    dbSizeMb: number
    backendMax: number
  }
  containers: {
    name: string
    cpuPercent: number
    memPercent: number
    memUsedMb: number
    pids: number
  }[]
  sampledAt: string
  history: { sampledAt: string; cpuPercent: number; memPercent: number; pgConnections: number }[]
}

// ---- 总览类型（KEEPON 5.1）----

/** 总览聚合 DTO（一次回答"现在有没有事"）。 */
export interface Overview {
  window: string
  counts: {
    activeAlerts: number
    activeIncidents: number
    runningTasks: number
    knowledgeDocs: number
  }
  trend: { hour: string; count: number }[]
  severityDist: Record<string, number>
  recentEvents: OverviewEvent[]
  system: {
    dbOk: boolean
    dbError?: string
    queueDepth: number
    ragBacklog: number
    ragLagSec: number
    uptimeSec: number
  }
  backupEnabled: boolean
  latestBackup?: {
    status: string
    verifyStatus: string
    startedAt: string
    fileSize: number
  }
}

/** 最近事件（alert=告警变化 / audit=处置动作；DB 重查，刷新可恢复）。 */
export interface OverviewEvent {
  kind: 'alert' | 'audit'
  title: string
  severity?: string
  status?: string
  provider?: string
  action?: string
  actor?: string
  createdAt: string
}

/** webhook 回显（接入向导调试用；含归一化后 severity 与指纹）。 */
export interface WebhookResult {
  accepted: number
  processed: number
  provider: string
  events: {
    name: string
    severity: string
    status: string
    labels: Record<string, unknown>
    fingerprint: string
    dedupKey: string
  }[]
}

// ---- 知识库类型（KEEPON M3：个人知识源 + golden set 评测）----

/** 个人知识源（runbook/SOP 原文库）。 */
export interface KnowledgeSource {
  id: string
  tenantId: string
  name: string
  sourceType: string
  content: string
  service?: string
  status: string // draft|published|deleted|quarantined
  version: number
  updatedAt: string
}

/** 知识源列表行（含派生索引状态）。 */
export interface SourceRow {
  id: string
  name: string
  service?: string
  status: string
  version: number
  updatedAt: string
  jobStatus?: string
  jobError?: string
  jobVersion?: number
  docStatus?: string
  docVersion?: number
  trustLevel?: string
}

/** 上传预览（front matter 解析 + 脱敏检查结果）。 */
export interface SourcePreview {
  name: string
  frontMatter: { title?: string; service?: string; tags?: string[]; updatedAt?: string }
  body: string
  redactions: { category: string; count: number }[]
  injection: boolean
  bytes: number
  warning?: string
}

/** 检索评测查询（golden set）。 */
export interface GoldenQuery {
  id: string
  tenantId: string
  query: string
  service?: string
  severity?: string
  expectedSources: string[] // source_id 数组
  tags?: string
  enabled: boolean
  useful?: boolean | null
  note?: string
  createdAt: string
  updatedAt: string
}

// ---- 备份类型 ----

/** 备份执行记录（backup_runs 表；校验/镜像状态独立呈现）。 */
export interface BackupRun {
  id: string
  trigger: string // schedule | manual
  startedAt: string
  finishedAt?: string
  status: 'running' | 'success' | 'failed'
  filePath?: string
  fileSize?: number
  sha256?: string
  mirrorPath?: string
  mirrorEncrypted?: boolean
  verifyStatus?: string // skipped | passed | failed
  verifyDetail?: string
  errorDetail?: string
}

// ---- 访问保护类型 ----

/** 认证状态（前端路由守卫判定：noauth 直通 / apikey 需解锁）。 */
export interface AuthStatus {
  mode: 'noauth' | 'apikey'
  hasKeys: boolean
}

/** WebSocket 握手短期 ticket（30s TTL、单次使用）。 */
export interface WSTicket {
  ticket: string
  ttlSec: number
}

/** 访问密钥（不含哈希；isOwner 标记首把钥匙）。 */
export interface APIKeyItem {
  id: string
  tenantId: string
  name: string
  isOwner: boolean
  lastUsedAt?: string
  createdAt: string
}

// 大模型 API 接入配置（model_configs 表；仅 Chat LLM，apiKey 只回显是否已配置）。
export interface ModelConfig {
  key: string // chat
  kind: string
  enabled: boolean
  baseURL: string
  model: string
  timeoutSec: number
  stream: boolean
  apiKeySet: boolean
  updatedAt?: string
}

export interface ModelConfigSave {
  enabled: boolean
  baseURL: string
  apiKey: string // 空 = 保留旧值
  model: string
  timeoutSec: number
  stream: boolean
}

export interface ModelTestResult {
  key: string
  ok: boolean
  error?: string
}

// WS 推送消息结构（与后端 pusher.Event 对齐）。
export interface WSEvent {
  type: string // alert.new | alert.update | incident.upsert | incident.state ...
  data: unknown
}

// ---- 对话式 Agent 类型（与 internal/assistant DTO 对齐） ----

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

export interface AssistantRunDTO {
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

export interface SendMessageResult {
  threadId: string
  messageId: string
  runId: string
}

export interface RunDetailResponse {
  run: AssistantRunDTO
  steps: unknown[]
}

// AlertAnomaly 告警数量异常检测项（KEEPON 七）。
export interface AlertAnomaly {
  hour: string
  count: number
  baseline: number
  lower: number
  upper: number
  sigma: number
  anomaly: boolean
  reason?: string
}

// ExternalSystem 外部可观测系统（KEEPON 6.2）。
export interface ExternalSystem {
  id: string
  tenantId: string
  name: string
  url: string
  type: 'grafana' | 'prometheus' | 'logs' | 'other'
  enabled: boolean
  sortOrder: number
  createdAt: string
  updatedAt: string
}

// AssistantActionRequest 人工确认动作请求（Phase 3）。
export interface AssistantActionRequest {
  id: string
  tenantId: string
  runId: string
  toolName: string
  arguments: unknown
  riskLevel: 'low' | 'medium' | 'high'
  status: 'pending' | 'executing' | 'succeeded' | 'failed' | 'rejected' | 'expired'
  requestedBy?: string
  decidedBy?: string
  decisionReason?: string
  expiresAt?: string
  createdAt: string
  updatedAt: string
}
