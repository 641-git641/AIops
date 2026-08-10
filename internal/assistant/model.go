// Package assistant 对话式 Agent 会话编排域（AGENT_CHAT_PLAN §5.1）：
// 与现有 internal/agent（告警触发的固定状态机）职责分离——本包由用户触发，
// 负责理解问题、检索证据、调用白名单只读工具、生成带引用建议。
// 所有有副作用的动作（Phase 3）必须经服务端鉴权与人工确认后再调用现有领域 Service。
package assistant

import (
	"time"

	"gorm.io/datatypes"
)

// 运行状态机（对齐 AGENT_CHAT_PLAN §5.2）：queued → running → completed | failed | cancelled。
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunCompleted = "completed"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// 消息角色（对齐 internal/llm.Role，DB 用字符串）。
const (
	MsgUser      = "user"
	MsgAssistant = "assistant"
	MsgSystem    = "system"
	MsgTool      = "tool"
)

// Thread 状态。
const ThreadActive = "active"
const ThreadDeleted = "deleted"

// AssistantThread 会话（可绑定 incident/alert 上下文——Phase 2 启用 context_type/context_id）。
type AssistantThread struct {
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID    string    `gorm:"size:64;index:idx_asst_thread_tenant_upd,priority:1" json:"tenantId"`
	Title       string    `gorm:"size:200" json:"title"`
	Status      string    `gorm:"size:16" json:"status"` // active | deleted（软删）
	ContextType string    `gorm:"size:16" json:"contextType,omitempty"` // "" | incident | alert（Phase 2）
	ContextID   string    `gorm:"size:36" json:"contextId,omitempty"`
	CreatedBy   string    `gorm:"size:64" json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `gorm:"index:idx_asst_thread_tenant_upd,priority:2" json:"updatedAt"`
}

// TableName 表名。
func (AssistantThread) TableName() string { return "assistant_threads" }

// AssistantMessage 会话消息（最终 assistant 消息才作为后续上下文；工具事件进 steps/events）。
type AssistantMessage struct {
	ID            string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID      string         `gorm:"size:64;uniqueIndex:uk_asst_msg_thread_seq,priority:1" json:"tenantId"`
	ThreadID      string         `gorm:"size:36;uniqueIndex:uk_asst_msg_thread_seq,priority:2" json:"threadId"`
	RunID         string         `gorm:"size:36;index:idx_asst_msg_run" json:"runId,omitempty"`
	Role          string         `gorm:"size:16" json:"role"` // user | assistant | system | tool
	Content       string         `gorm:"type:text" json:"content"`
	ContentFormat string         `gorm:"size:16" json:"contentFormat"` // text | markdown
	Sequence      int            `gorm:"uniqueIndex:uk_asst_msg_thread_seq,priority:3" json:"sequence"`
	Status        string         `gorm:"size:16" json:"status"` // complete（流式完成前可 partial，本期恒 complete）
	Citations     datatypes.JSON `json:"citations,omitempty"`   // jsonb 数组（Phase 2）
	CreatedAt     time.Time      `json:"createdAt"`
}

// TableName 表名。
func (AssistantMessage) TableName() string { return "assistant_messages" }

// AssistantRun 一次模型运行（对齐 rag.KnowledgeJob 租约模式）。
type AssistantRun struct {
	ID             string     `gorm:"primaryKey;size:36" json:"id"`
	TenantID       string     `gorm:"size:64;index:idx_asst_run_tenant,priority:1;uniqueIndex:uk_asst_run_idem,priority:1" json:"tenantId"`
	ThreadID       string     `gorm:"size:36;index:idx_asst_run_tenant,priority:2" json:"threadId"`
	InputMessageID string     `gorm:"size:36" json:"inputMessageId"`
	Status         string     `gorm:"size:16;index:idx_asst_run_tenant,priority:3" json:"status"`
	Model          string     `gorm:"size:128" json:"model"`
	PromptVersion  string     `gorm:"size:16" json:"promptVersion"`
	IdempotencyKey string     `gorm:"size:64;uniqueIndex:uk_asst_run_idem,priority:2" json:"idempotencyKey"`
	StepCount      int        `json:"stepCount"`
	LatencyMS      int64      `json:"latencyMs"`
	ErrorCode      string     `gorm:"size:64" json:"errorCode,omitempty"` // model_not_configured | timeout | ...
	Attempts       int        `json:"attempts"`
	NextRunAt      time.Time  `json:"nextRunAt"`
	LockedAt       *time.Time `json:"lockedAt,omitempty"` // 租约：worker 崩溃后超时复活
	LockedBy       string     `gorm:"size:64" json:"lockedBy,omitempty"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// TableName 表名。
func (AssistantRun) TableName() string { return "assistant_runs" }

// StepKind 步骤类型。
const (
	StepModel    = "model"
	StepToolCall = "tool_call"
	StepToolRes  = "tool_result"
	StepPolicy   = "policy"
)

// AssistantStep 运行步骤（可回放/审计；敏感字段写入前脱敏，只存裁剪后摘要）。
type AssistantStep struct {
	ID            string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID      string         `gorm:"size:64" json:"tenantId"`
	RunID         string         `gorm:"size:36;index:idx_asst_step_run" json:"runId"`
	Ordinal       int            `json:"ordinal"`
	Kind          string         `gorm:"size:16" json:"kind"` // model | tool_call | tool_result | policy
	ToolName      string         `gorm:"size:64" json:"toolName,omitempty"`
	Arguments     datatypes.JSON `json:"arguments,omitempty"` // 裁剪后的工具参数
	ResultSummary datatypes.JSON `json:"resultSummary,omitempty"` // 工具结果摘要（≤8KB）
	Status        string         `gorm:"size:16" json:"status"`   // ok | failed | rejected
	DurationMS    int64          `json:"durationMs"`
	ErrorCode     string         `gorm:"size:64" json:"errorCode,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
}

// TableName 表名。
func (AssistantStep) TableName() string { return "assistant_steps" }

// 动作请求状态（Phase 3 启用；本期仅建 schema）。
const (
	ActionPending   = "pending"
	ActionConfirmed = "confirmed"
	ActionRejected  = "rejected"
)

// AssistantActionRequest 需人工确认的动作请求（模型只能提出，不能直接执行）。
type AssistantActionRequest struct {
	ID             string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID       string         `gorm:"size:64" json:"tenantId"`
	RunID          string         `gorm:"size:36;index:idx_asst_action_run" json:"runId"`
	ToolName       string         `gorm:"size:64" json:"toolName"`
	Arguments      datatypes.JSON `json:"arguments"` // 规范化后的参数
	RiskLevel      string         `gorm:"size:16" json:"riskLevel"` // low | medium | high
	Status         string         `gorm:"size:16" json:"status"`    // pending | confirmed | rejected | ...
	RequestedBy    string         `gorm:"size:64" json:"requestedBy"`
	DecidedBy      string         `gorm:"size:64" json:"decidedBy,omitempty"`
	DecisionReason string         `gorm:"size:512" json:"decisionReason,omitempty"`
	IdempotencyKey string         `gorm:"size:64;uniqueIndex:uk_asst_action_idem,priority:2" json:"idempotencyKey"`
	ExpiresAt      *time.Time     `json:"expiresAt,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
}

// TableName 表名。
func (AssistantActionRequest) TableName() string { return "assistant_action_requests" }

// Citation 引用（AGENT_CHAT_PLAN §8.2：回答绑定结构化 citation，UI 可跳转）。
// 来源信息来自知识库证据（untrusted 数据，仅展示用，无指令权）。
type Citation struct {
	SourceType string `json:"sourceType"` // incident | playbook | knowledge
	SourceID   string `json:"sourceId"`
	Title      string `json:"title"`
	TrustLevel string `json:"trustLevel,omitempty"`
	Time       string `json:"time,omitempty"`
	URL        string `json:"url,omitempty"` // 前端跳转路径（后端只对可定位来源生成）
}

// 事件类型（AGENT_CHAT_PLAN §7）。
const (
	EvRunStarted     = "run.started"
	EvRunCompleted   = "run.completed"
	EvRunFailed      = "run.failed"
	EvRunCancelled   = "run.cancelled"
	EvMessageDelta   = "message.delta"
	EvMessageDone    = "message.completed"
	EvToolStarted    = "tool.started"
	EvToolCompleted  = "tool.completed"
	EvToolFailed     = "tool.failed"
	EvCitationAdded  = "citation.added" // 工具命中证据（data: {citation}）
	EvUsageUpdated   = "usage.updated"
	EvHeartbeat      = "heartbeat"
	EvRunStatus      = "run.status" // SSE 重连快照头事件
)

// AssistantEvent 运行事件（SSE 与 DB 快照共用；sequence 单调递增，断线按最后 sequence 续传）。
type AssistantEvent struct {
	ID        string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID  string         `gorm:"size:64" json:"tenantId"`
	RunID     string         `gorm:"size:36;uniqueIndex:uk_asst_evt_run_seq,priority:1;index:idx_asst_evt_run" json:"runId"`
	Sequence  int64          `gorm:"uniqueIndex:uk_asst_evt_run_seq,priority:2" json:"sequence"`
	Type      string         `gorm:"size:32" json:"type"`
	Data      datatypes.JSON `json:"data,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

// TableName 表名。
func (AssistantEvent) TableName() string { return "assistant_events" }

// 反馈评分（AGENT_CHAT_PLAN §14.3 离线问题集/在线反馈；rating 白名单）。
const (
	FeedbackUseful    = "useful"
	FeedbackNotUseful = "not_useful"
)

// AssistantFeedback 消息反馈（Phase 2：回答「有用/没用」+ 备注；评价对象是最终 assistant 消息）。
type AssistantFeedback struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID  string    `gorm:"size:64" json:"tenantId"`
	MessageID string    `gorm:"size:36;index:idx_asst_fb_msg" json:"messageId"`
	RunID     string    `gorm:"size:36" json:"runId,omitempty"`
	Rating    string    `gorm:"size:16" json:"rating"` // useful | not_useful
	Note      string    `gorm:"size:1000" json:"note,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// TableName 表名。
func (AssistantFeedback) TableName() string { return "assistant_feedback" }
