package topology

import (
	"time"
)

// Edge 归一化依赖边（Phase 3 规模化）：把 TopologyService.DependsOn（JSONB）投影成关系表，
// 供 SQL 递归 CTE 做影响面查询（反向遍历：谁依赖我）。Postgres 侧完成图遍历，百万节点内不依赖整图加载。
// Neo4j 后置可选：届时仅替换 Impact 的查询实现，模型与 API 不变。
type Edge struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID  string    `gorm:"size:64;uniqueIndex:uk_topo_edge,priority:1;index:idx_topo_edge_dep,priority:1" json:"tenantId"`
	Service   string    `gorm:"size:128;uniqueIndex:uk_topo_edge,priority:2" json:"service"`                                      // 依赖方（消费方/下游）
	DependsOn string    `gorm:"size:128;uniqueIndex:uk_topo_edge,priority:3;index:idx_topo_edge_dep,priority:2" json:"dependsOn"` // 被依赖方（上游）
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TableName 表名。
func (Edge) TableName() string { return "topology_edges" }
