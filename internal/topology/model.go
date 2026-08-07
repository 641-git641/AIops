// Package topology 实现知识层：服务拓扑 + 依赖关系 + 近期变更。
// 参考 multi-agent-aiops 的知识图谱：MVP 用 SQLite/Postgres 表 + 内存图遍历（BFS），Neo4j 后置可选。
package topology

import (
	"time"

	"gorm.io/datatypes"
)

// TopologyService 服务拓扑节点。
type TopologyService struct {
	ID            string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID      string         `gorm:"size:64;uniqueIndex:uk_topo_tenant_name,priority:1" json:"tenantId"`
	Name          string         `gorm:"size:128;uniqueIndex:uk_topo_tenant_name,priority:2" json:"name"` // order-service
	Kind          string         `gorm:"size:32" json:"kind"`                                             // service|pod|node|db
	DependsOn     datatypes.JSON `json:"dependsOn"`                                                       // []string 直接依赖
	RecentChanges datatypes.JSON `json:"recentChanges"`                                                   // []string 近期变更（CMDB 同步）
	ImpactScore   float64        `gorm:"default:0" json:"impactScore"`                                    // 被依赖数/总数，定期重算
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

// TableName 表名。
func (TopologyService) TableName() string { return "topology_services" }
