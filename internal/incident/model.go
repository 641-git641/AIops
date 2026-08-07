// Package incident 定义事故领域模型：Incident、关联表、编排器全局状态 IncidentState。
package incident

import (
	"encoding/json"
	"time"

	"aiops/internal/util"

	"gorm.io/datatypes"
)

// Incident 状态。
const (
	IncidentOpen            = "open"
	IncidentResolved        = "resolved"
	IncidentMerged          = "merged"
	IncidentPendingApproval = "pending_approval"
)

// Incident 事故（由规则引擎或 Agent 聚合告警产生）。
type Incident struct {
	ID              string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID        string         `gorm:"size:64;index:idx_inc_tenant_status,priority:1" json:"tenantId"`
	RuleFingerprint string         `gorm:"size:64;index:idx_inc_tenant_status,priority:2" json:"ruleFingerprint"` // 关联规则指纹（agent 路径为空）
	ClusterKey      string         `gorm:"size:64;index" json:"clusterKey"`                                       // AI 聚类分组键（source=ai 去重用）
	Name            string         `gorm:"size:255" json:"name"`
	AIGeneratedName string         `gorm:"size:255" json:"aiGeneratedName"`
	LLMSummary      string         `gorm:"type:text" json:"llmSummary"` // AI 聚类 LLM 增强摘要（可选，失败为空）
	Status          string         `gorm:"size:16;index:idx_inc_tenant_status,priority:3" json:"status"`
	Severity        string         `gorm:"size:16" json:"severity"`
	Summary         string         `gorm:"type:text" json:"summary"`
	Source          string         `gorm:"size:16" json:"source"` // rule（规则引擎）| agent（编排器）| ai（AI 建议）
	MergedInto      *string        `gorm:"size:36" json:"mergedInto,omitempty"`
	AgentStateID    *string        `gorm:"size:36" json:"agentStateId,omitempty"` // 关联编排器状态
	CreatedAt       time.Time      `json:"createdAt"`
	ResolvedAt      *time.Time     `json:"resolvedAt,omitempty"`
	Extra           datatypes.JSON `json:"extra,omitempty"`
}

// TableName 表名。
func (Incident) TableName() string { return "incidents" }

// AlertToIncident 告警↔事故关联表。一个活跃告警至多归属一个 incident。
type AlertToIncident struct {
	AlertID    string `gorm:"primaryKey;size:36" json:"alertId"`
	IncidentID string `gorm:"primaryKey;size:36;index:idx_ati_incident" json:"incidentId"`
	TenantID   string `gorm:"size:64" json:"tenantId"`
}

// TableName 表名。
func (AlertToIncident) TableName() string { return "alert_to_incidents" }

// StateStatus 编排器状态机状态。
const (
	StateOpen            = "open"             // 初始
	StateInvestigating   = "investigating"    // monitor/rca 进行中
	StateHealing         = "healing"          // heal 进行中
	StatePendingApproval = "pending_approval" // 等待审批
	StateResolved        = "resolved"         // 闭环
)

// State 编排器的全局状态对象（Agent 流水线共享上下文 + 检查点快照）。
type State struct {
	ID            string         `gorm:"primaryKey;size:36" json:"id"` // = incident_id
	TenantID      string         `gorm:"size:64;index:idx_state_tenant" json:"tenantId"`
	Status        string         `gorm:"size:32" json:"status"`
	AlertID       string         `gorm:"size:36;index:idx_state_alert" json:"alertId"`
	CorrelationID string         `gorm:"size:36;index:idx_state_corr" json:"correlationId"` // 贯穿 alert→rca→heal→change
	NodeResults   datatypes.JSON `json:"nodeResults"`                                       // {monitor:{}, rca:{}, heal:{}, change:{}}
	CurrentAgent  string         `gorm:"size:32" json:"currentAgent"`
	RetryCount    int            `json:"retryCount"`
	ErrorMessage  string         `gorm:"type:text" json:"errorMessage"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

// TableName 表名。
func (State) TableName() string { return "incident_states" }

// NewState 创建初始状态。
func NewState(tenantID, alertID string) *State {
	return &State{
		ID:            util.NewID(),
		TenantID:      tenantID,
		Status:        StateOpen,
		AlertID:       alertID,
		CorrelationID: util.CorrelationID(),
		NodeResults:   datatypes.JSON([]byte(`{}`)),
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
}

// SetNodeResult 写入某 agent 的结构化结果。
func (s *State) SetNodeResult(node string, result any) {
	m := map[string]any{}
	_ = json.Unmarshal(s.NodeResults, &m)
	m[node] = result
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	s.NodeResults = datatypes.JSON(b)
}

// NodeResult 返回某 agent 的结果（map 或 nil）。
func (s *State) NodeResult(node string) map[string]any {
	m := map[string]any{}
	if err := json.Unmarshal(s.NodeResults, &m); err != nil {
		return nil
	}
	if v, ok := m[node].(map[string]any); ok {
		return v
	}
	return nil
}
