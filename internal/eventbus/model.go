// Package eventbus 实现事件总线抽象与审计日志。
// Agent 间事件解耦 + correlation_id 全链路追踪 + 可审计。
// MVP 用内存实现（编排器同步驱动 + 事件审计），Kafka 后置可选。
package eventbus

import (
	"time"

	"gorm.io/datatypes"
)

// Topic 约定（去 aiops. 前缀简化）。
const (
	TopicAlerts    = "alerts"
	TopicEvents    = "events"
	TopicCommands  = "commands"
	TopicAudit     = "audit"
	TopicDLQPrefix = "dlq"
)

// AuditLog 审计四元组（who/when/what/why），审批与处置的合规基线。
type AuditLog struct {
	ID            string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID      string         `gorm:"size:64;index:idx_audit_tenant" json:"tenantId"`
	IncidentID    string         `gorm:"size:36;index:idx_audit_incident" json:"incidentId"`
	CorrelationID string         `gorm:"size:36;index:idx_audit_corr" json:"correlationId"`
	Actor         string         `gorm:"size:64" json:"actor"`  // auto-system|oncall-engineer|tech-lead|user
	Action        string         `gorm:"size:64" json:"action"` // heal.executed|approval.granted|...
	Target        string         `gorm:"size:128" json:"target"`
	Reason        string         `gorm:"type:text" json:"reason"`
	Metadata      datatypes.JSON `json:"metadata,omitempty"`
	CreatedAt     time.Time      `gorm:"index:idx_audit_created" json:"createdAt"` // 保留策略分批清理按此列删除
}

// TableName 表名。
func (AuditLog) TableName() string { return "audit_logs" }
