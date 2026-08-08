// Package workflow 实现 YAML 工作流引擎。
// 用户以 YAML 声明 triggers（监听告警）+ steps/actions（provider 执行，provider: agent 可调 Agent 编排），
// 命中告警时异步执行；workflow_runs 以 (tenant_id, workflow_id, dedup_key) 唯一约束实现「并发防重」——
// 同一工作流对同一告警（fingerprint）只执行一次，并发首达时由 DB 唯一索引裁决。
package workflow

import (
	"time"

	"gorm.io/datatypes"
)

// 运行状态。
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

// Workflow 工作流定义（原始 YAML 存储，触发时解析；「定义即配置」）。
type Workflow struct {
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID    string    `gorm:"size:64" json:"tenantId"`
	Name        string    `gorm:"size:128" json:"name"`
	Description string    `gorm:"size:512" json:"description"`
	YAML        string    `gorm:"type:text" json:"yaml"` // 原始定义
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// TableName 表名。
func (Workflow) TableName() string { return "workflows" }

// WorkflowRun 一次工作流执行记录（并发防重的落库点）。
type WorkflowRun struct {
	ID         string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID   string         `gorm:"size:64;uniqueIndex:uk_wfrun_tenant_workflow_key,priority:1" json:"tenantId"`
	WorkflowID string         `gorm:"size:36;uniqueIndex:uk_wfrun_tenant_workflow_key,priority:2" json:"workflowId"`
	DedupKey   string         `gorm:"size:64;uniqueIndex:uk_wfrun_tenant_workflow_key,priority:3" json:"dedupKey"` // alert: SHA256(workflow_id:alert.fingerprint)；interval/manual: 每次触发唯一
	Trigger    string         `gorm:"size:16" json:"trigger"`                                                      // 触发来源：alert|interval|manual
	Status     string         `gorm:"size:16" json:"status"`
	Result     datatypes.JSON `json:"result"` // 各步骤结果（map[stepName]any）
	Error      string         `gorm:"size:1024" json:"error"`
	StartedAt  time.Time      `gorm:"index:idx_wfrun_started" json:"startedAt"` // 保留策略分批清理按此列删除
	FinishedAt time.Time      `json:"finishedAt"`
}

// TableName 表名。
func (WorkflowRun) TableName() string { return "workflow_runs" }
