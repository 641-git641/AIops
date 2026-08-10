// Package external 外部可观测系统入口（KEEPON 6.2）：
// 侧边栏快捷跳转 Grafana/Prometheus/日志系统等。「env 首启种子、DB 为准、保存后热生效」——
// env 只在表空时播种，之后所有读写走 DB（无需重启）。
package external

import "time"

// 系统类型（预设图标；不做图标上传）。
const (
	TypeGrafana    = "grafana"
	TypePrometheus = "prometheus"
	TypeLogs       = "logs"
	TypeOther      = "other"
)

// TypeLabel 类型展示名。
var TypeLabel = map[string]string{
	TypeGrafana: "Grafana", TypePrometheus: "Prometheus",
	TypeLogs: "日志", TypeOther: "其他",
}

// ValidType 类型白名单。
func ValidType(t string) bool {
	switch t {
	case TypeGrafana, TypePrometheus, TypeLogs, TypeOther:
		return true
	}
	return false
}

// System 外部系统条目（无敏感字段，管理不设独立开关）。
type System struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID  string    `gorm:"size:64;uniqueIndex:uk_ext_tenant_name,priority:1" json:"tenantId"`
	Name      string    `gorm:"size:64;uniqueIndex:uk_ext_tenant_name,priority:2" json:"name"`
	URL       string    `gorm:"size:512" json:"url"`
	Type      string    `gorm:"size:16" json:"type"` // grafana | prometheus | logs | other
	Enabled   bool      `json:"enabled"`
	SortOrder int       `json:"sortOrder"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TableName 表名。
func (System) TableName() string { return "external_systems" }
