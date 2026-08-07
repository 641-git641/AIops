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

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
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
}

// WS 推送消息结构（与后端 pusher.Event 对齐）。
export interface WSEvent {
  type: string // alert.new | alert.update | incident.upsert | incident.state ...
  data: unknown
}
