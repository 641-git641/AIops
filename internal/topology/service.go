package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aiops/internal/util"

	"gorm.io/gorm"
)

// Service 拓扑服务：提供依赖链查询与影响分数计算（RCA Agent 的事实来源）。
type Service struct {
	db *gorm.DB
}

// NewService 创建拓扑服务。
func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// Create 创建拓扑节点，并同步归一化依赖边（影响面查询的事实来源）。
func (s *Service) Create(ctx context.Context, t *TopologyService) error {
	if t.ID == "" {
		t.ID = util.NewID()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = time.Now().UTC()
	}
	if err := s.db.WithContext(ctx).Create(t).Error; err != nil {
		return err
	}
	return s.syncEdges(ctx, t.TenantID, t.Name, t.dependsOn())
}

// SyncAllEdges 全量重建某租户的归一化边（幂等：先删后插）。
// 用于启动时回填存量拓扑（JSONB 时代的数据没有边表投影）。
func (s *Service) SyncAllEdges(ctx context.Context, tenantID string) error {
	all, err := s.All(ctx, tenantID)
	if err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Delete(&Edge{}).Error; err != nil {
		return err
	}
	for _, t := range all {
		if err := s.insertEdges(ctx, tenantID, t.Name, t.dependsOn()); err != nil {
			return err
		}
	}
	return nil
}

// syncEdges 重写某服务的依赖边（先删后插，保证与 JSONB 一致）。
func (s *Service) syncEdges(ctx context.Context, tenantID, service string, deps []string) error {
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND service = ?", tenantID, service).Delete(&Edge{}).Error; err != nil {
		return err
	}
	return s.insertEdges(ctx, tenantID, service, deps)
}

func (s *Service) insertEdges(ctx context.Context, tenantID, service string, deps []string) error {
	now := time.Now().UTC()
	for _, dep := range deps {
		e := Edge{
			ID: util.NewID(), TenantID: tenantID, Service: service, DependsOn: dep,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.db.WithContext(ctx).Create(&e).Error; err != nil {
			return err
		}
	}
	return nil
}

// All 返回某租户全部拓扑节点。
func (s *Service) All(ctx context.Context, tenantID string) ([]TopologyService, error) {
	var items []TopologyService
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Find(&items).Error
	return items, err
}

// Get 按名称查询服务。
func (s *Service) Get(ctx context.Context, tenantID, name string) (*TopologyService, error) {
	var t TopologyService
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND name = ?", tenantID, name).First(&t).Error
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Deps 返回某服务的直接依赖。
func (s *Service) Deps(ctx context.Context, tenantID, name string) ([]string, error) {
	t, err := s.Get(ctx, tenantID, name)
	if err != nil {
		return nil, err
	}
	return t.dependsOn(), nil
}

// DependencyPaths 正向 BFS：返回从 start 出发的所有依赖链（深度受限）。
func (s *Service) DependencyPaths(ctx context.Context, tenantID, start string, maxDepth int) ([]string, error) {
	if maxDepth <= 0 {
		maxDepth = 5
	}
	all, err := s.All(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	g := buildGraph(all)
	seen := map[string]bool{start: true}
	var paths []string
	var walk func(node string, path []string, depth int)
	walk = func(node string, path []string, depth int) {
		if depth >= maxDepth {
			return
		}
		for _, dep := range g[node] {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			np := append(append([]string{}, path...), dep)
			paths = append(paths, join(np))
			walk(dep, np, depth+1)
			delete(seen, dep)
		}
	}
	walk(start, []string{start}, 0)
	return paths, nil
}

// RecentChanges 返回某服务及其直接依赖的近期变更。
func (s *Service) RecentChanges(ctx context.Context, tenantID, name string) ([]string, error) {
	t, err := s.Get(ctx, tenantID, name)
	if err != nil {
		return nil, err
	}
	changes := t.recentChanges()
	deps, _ := s.Deps(ctx, tenantID, name)
	for _, dep := range deps {
		dt, err := s.Get(ctx, tenantID, dep)
		if err != nil {
			continue
		}
		changes = append(changes, dt.recentChanges()...)
	}
	return changes, nil
}

// RecomputeImpactScores 重算影响分数（被依赖数/总数）。
func (s *Service) RecomputeImpactScores(ctx context.Context, tenantID string) error {
	all, err := s.All(ctx, tenantID)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}
	dependents := map[string]int{}
	for _, t := range all {
		for _, dep := range t.dependsOn() {
			dependents[dep]++
		}
	}
	for _, t := range all {
		score := float64(dependents[t.Name]) / float64(len(all))
		if err := s.db.WithContext(ctx).Model(&TopologyService{}).
			Where("tenant_id = ? AND name = ?", tenantID, t.Name).
			Update("impact_score", score).Error; err != nil {
			return err
		}
	}
	return nil
}

// Graph 返回邻接表（RCA 用）。
func (s *Service) Graph(ctx context.Context, tenantID string) (map[string][]string, error) {
	all, err := s.All(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return buildGraph(all), nil
}

// ImpactNode 影响面中的一个下游服务（深度 + 依赖链路径）。
type ImpactNode struct {
	Service string   `json:"service"`
	Depth   int      `json:"depth"`
	Path    []string `json:"path"` // 从被查服务出发的依赖链（首元素即直接受影响者）
}

// Impact 返回 service 故障时受影响的下游服务（谁依赖它，反向遍历，深度受限）。
// SQL 递归 CTE 实现：Postgres 侧完成遍历（topology_edges 反向索引），路径随行累积，
// NOT e.service = ANY(path) 防环；超过 depth 的路径不再展开。Neo4j 后置时仅替换本查询。
func (s *Service) Impact(ctx context.Context, tenantID, service string, depth int) ([]ImpactNode, error) {
	if depth <= 0 {
		depth = 5
	}
	const q = `
WITH RECURSIVE reach AS (
	SELECT e.tenant_id AS t, e.service::text AS s, 1 AS depth, ARRAY[e.service::text] AS path
	FROM topology_edges e
	WHERE e.tenant_id = ? AND e.depends_on = ?
	UNION ALL
	SELECT e.tenant_id, e.service::text, r.depth + 1, r.path || e.service::text
	FROM topology_edges e
	JOIN reach r ON e.tenant_id = r.t AND e.depends_on = r.s
	WHERE r.depth < ? AND NOT e.service = ANY(r.path)
)
SELECT s AS service, depth, array_to_string(path, '→') AS path FROM reach ORDER BY depth, s`
	// 路径以 '→' 拼接字符串返回（pgx 对 text[] 的 Scan 支持不可靠），Go 侧拆回切片
	var rows []struct {
		Service string
		Depth   int
		Path    string
	}
	if err := s.db.WithContext(ctx).Raw(q, tenantID, service, depth).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("topology: impact %s: %w", service, err)
	}
	nodes := make([]ImpactNode, 0, len(rows))
	for _, r := range rows {
		var parts []string
		if r.Path != "" {
			parts = strings.Split(r.Path, "→")
		}
		nodes = append(nodes, ImpactNode{Service: r.Service, Depth: r.Depth, Path: parts})
	}
	return nodes, nil
}

func (t *TopologyService) dependsOn() []string {
	var deps []string
	_ = json.Unmarshal(t.DependsOn, &deps)
	return deps
}

func (t *TopologyService) recentChanges() []string {
	var changes []string
	_ = json.Unmarshal(t.RecentChanges, &changes)
	return changes
}

func buildGraph(all []TopologyService) map[string][]string {
	g := map[string][]string{}
	for _, t := range all {
		g[t.Name] = t.dependsOn()
	}
	return g
}

func join(items []string) string {
	out := items[0]
	for _, it := range items[1:] {
		out += " → " + it
	}
	return out
}
