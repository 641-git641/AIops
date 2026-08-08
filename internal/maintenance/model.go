// Package maintenance 实现维护窗口（热路径静默）。
// 维护窗口按时间范围 + 匹配条件（fingerprint / service / severity 上限）
// 对 firing 告警静默：pipeline 热路径命中后以 suppressed 状态入库，
// 不触发规则关联 / Incident / Agent 路径。
package maintenance

import "time"

// Window 维护窗口。
type Window struct {
	ID          string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID    string    `gorm:"size:64;index:idx_mw_tenant_active,priority:1" json:"tenantId"`
	Name        string    `gorm:"size:128" json:"name"`       // 窗口名称（如「支付服务周三凌晨维护」）
	Fingerprint string    `gorm:"size:64" json:"fingerprint"` // 精确匹配 alert.fingerprint（可选）
	Service     string    `gorm:"size:128" json:"service"`    // 匹配 labels.service（可选）
	Severity    string    `gorm:"size:16" json:"severity"`    // 覆盖 <= 该严重度的告警（可选，留空不限制）
	StartsAt    time.Time `gorm:"index:idx_mw_tenant_active,priority:2" json:"startsAt"`
	EndsAt      time.Time `gorm:"index:idx_mw_tenant_active,priority:3" json:"endsAt"`
	Enabled     bool      `gorm:"index:idx_mw_tenant_active,priority:4" json:"enabled"`
	Reason      string    `gorm:"type:text" json:"reason"` // 维护原因（审计可见）
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// TableName 表名。
func (Window) TableName() string { return "maintenance_windows" }
