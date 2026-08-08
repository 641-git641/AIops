// Package provider 定义数据源接入抽象与注册表。
// 统一抽象 + 注册表 + 编译期聚合。
package provider

import (
	"time"

	"gorm.io/datatypes"
)

// ProviderConfig 已安装的数据源配置（敏感字段 Config 加密后入库）。
// 命名避开 interface Provider（运行时代码路径用），模型层用 ProviderConfig。
type ProviderConfig struct {
	ID        string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID  string         `gorm:"size:64;uniqueIndex:uk_prov_tenant_kind,priority:1" json:"tenantId"`
	Kind      string         `gorm:"size:64;uniqueIndex:uk_prov_tenant_kind,priority:2" json:"kind"` // prometheus/aliyun/tencent/huawei/llm
	Name      string         `gorm:"size:128" json:"name"`
	Config    datatypes.JSON `json:"config"` // JSON 配置（secret 加密）
	Enabled   bool           `json:"enabled"`
	LastPull  *time.Time     `json:"lastPull,omitempty"` // PULL 型 provider 上次拉取时间（节流）
	CreatedAt time.Time      `json:"createdAt"`
}

// TableName 表名。
func (ProviderConfig) TableName() string { return "providers" }
