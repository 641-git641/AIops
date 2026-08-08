package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"aiops/internal/incident"
	"aiops/internal/topology"
	"aiops/internal/util"

	"github.com/gin-gonic/gin"
)

// -------- 规则（correlation rules）--------

// ruleInput 创建规则的请求体。
type ruleInput struct {
	Name             string   `json:"name" binding:"required"`
	CELExpression    string   `json:"celExpression" binding:"required"`
	GroupingCriteria []string `json:"groupingCriteria"`
	Threshold        int      `json:"threshold"`
	RunAgent         bool     `json:"runAgent"`
	Enabled          bool     `json:"enabled"`
}

// handleListRules 规则列表。
func (s *Server) handleListRules(c *gin.Context) {
	items, err := s.svc.ListRules(c.Request.Context(), tenantID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// handleCreateRule 创建规则。
func (s *Server) handleCreateRule(c *gin.Context) {
	var in ruleInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	grouping, _ := json.Marshal(in.GroupingCriteria)
	if in.GroupingCriteria == nil {
		grouping = json.RawMessage(`[]`)
	}
	if in.Threshold <= 0 {
		in.Threshold = 1
	}
	now := time.Now().UTC()
	r := incident.Rule{
		ID:               util.NewID(),
		TenantID:         tenantID(c),
		Name:             in.Name,
		Fingerprint:      util.SHA256(in.Name + "|" + in.CELExpression),
		CELExpression:    in.CELExpression,
		GroupingCriteria: grouping,
		Threshold:        in.Threshold,
		RunAgent:         in.RunAgent,
		Enabled:          in.Enabled,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.svc.CreateRule(c.Request.Context(), &r); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 规则变化立即失效租户缓存（下次 Evaluate 重新查库，无需等 TTL）
	s.rules.Invalidate(tenantID(c))
	c.JSON(http.StatusCreated, r)
}

// -------- 拓扑 --------

// topologyInput 创建拓扑节点的请求体。
type topologyInput struct {
	Name          string   `json:"name" binding:"required"`
	Kind          string   `json:"kind"`
	DependsOn     []string `json:"dependsOn"`
	RecentChanges []string `json:"recentChanges"`
}

// handleListTopology 拓扑节点列表。
func (s *Server) handleListTopology(c *gin.Context) {
	items, err := s.topo.All(c.Request.Context(), tenantID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// handleCreateTopology 创建拓扑节点（RCA 知识层）。
func (s *Server) handleCreateTopology(c *gin.Context) {
	var in topologyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Kind == "" {
		in.Kind = "service"
	}
	deps, _ := json.Marshal(in.DependsOn)
	changes, _ := json.Marshal(in.RecentChanges)
	t := topology.TopologyService{
		ID: util.NewID(), TenantID: tenantID(c), Name: in.Name, Kind: in.Kind,
		DependsOn: deps, RecentChanges: changes,
	}
	if err := s.topo.Create(c.Request.Context(), &t); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = s.topo.RecomputeImpactScores(c.Request.Context(), tenantID(c))
	c.JSON(http.StatusCreated, t)
}

// handleTopologyImpact 影响面查询：service 故障时受影响的全部下游服务（谁依赖它）。
// 深度受限的 SQL 递归 CTE 遍历（topology_edges 边表）；Neo4j 后置时仅换后端实现。
func (s *Server) handleTopologyImpact(c *gin.Context) {
	name := c.Param("name")
	depth, _ := strconv.Atoi(c.DefaultQuery("depth", "5"))
	nodes, err := s.topo.Impact(c.Request.Context(), tenantID(c), name, depth)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"service": name, "depth": depth, "affected": nodes, "total": len(nodes)})
}

// -------- 其它 --------

// handleListProviders 已注册的 provider 列表。
func (s *Server) handleListProviders(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"items": s.reg.Kinds(), "total": len(s.reg.Kinds())})
}

// handleCreateAPIKey 创建 API key，返回明文（仅此一次）。
func (s *Server) handleCreateAPIKey(c *gin.Context) {
	var in struct {
		Name string `json:"name"`
	}
	_ = c.ShouldBindJSON(&in)
	secret, err := s.authSvc.Create(c.Request.Context(), tenantID(c), in.Name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"apiKey": secret, "tenantId": tenantID(c)})
}
