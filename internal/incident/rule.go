package incident

import (
	"time"

	"gorm.io/datatypes"
)

// Rule 关联规则（correlation rule）：告警匹配 → 聚合生成 Incident。
// 参考 keep 的 Rule 模型：CEL 表达式 + 分组字段 + create/resolve + threshold。
type Rule struct {
	ID               string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID         string         `gorm:"size:64;index:idx_rule_tenant" json:"tenantId"`
	Name             string         `gorm:"size:128" json:"name"`
	Fingerprint      string         `gorm:"size:64;index:idx_rule_fp" json:"fingerprint"` // 规则指纹（标识同一规则，改内容即变）
	CELExpression    string         `gorm:"type:text" json:"celExpression"`               // 告警匹配表达式，如 alert.severity == 'critical'
	GroupingCriteria datatypes.JSON `json:"groupingCriteria"`                             // 分组字段，如 ["provider","labels.host"]
	CreateOn         string         `gorm:"type:text" json:"createOn"`                    // 何时创建 incident（keep: any|all，简化：空 = 恒建）
	ResolveOn        string         `gorm:"type:text" json:"resolveOn"`                   // 何时解决（keep: first|last|all|never）
	Threshold        int            `json:"threshold"`                                    // 分组内命中次数 ≥ N 才建 incident
	RunAgent         bool           `json:"runAgent"`                                     // 命中后是否进入 Agent 流水线
	Enabled          bool           `json:"enabled"`
	CreatedAt        time.Time      `json:"createdAt"`
	UpdatedAt        time.Time      `json:"updatedAt"`
}

// TableName 表名。
func (Rule) TableName() string { return "rules" }
