// Package api 实现 HTTP API 层：Gin 路由 + 租户中间件 + handlers + WS 端点。
package api

import (
	"net/http"

	"aiops/internal/ai"
	"aiops/internal/alert"
	"aiops/internal/auth"
	"aiops/internal/config"
	"aiops/internal/eventbus"
	"aiops/internal/incident"
	"aiops/internal/maintenance"
	"aiops/internal/metrics"
	"aiops/internal/notifier"
	"aiops/internal/orchestrator"
	"aiops/internal/pipeline"
	"aiops/internal/playbook"
	"aiops/internal/provider"
	"aiops/internal/pusher"
	"aiops/internal/topology"
	"aiops/internal/workflow"

	"github.com/gin-gonic/gin"
)

// Server API 服务：持有全部依赖并注册路由。
type Server struct {
	cfg      *config.Config
	alerts   *alert.Repository
	svc      *incident.Service
	rules    *incident.RuleEngine
	reg      *provider.Registry
	topo     *topology.Service
	authSvc  *auth.Service
	hub      *pusher.Hub
	push     pusher.Broadcaster
	pipe     *pipeline.Pipeline
	pbs      *playbook.Service
	audit    *eventbus.AuditService
	orch     *orchestrator.Orchestrator
	ntf      *notifier.Service
	channels *notifier.ConfigService // 通知渠道 DB 配置（管理 UI）
	mw       *maintenance.Service
	ai       *ai.Service
	wf       *workflow.Service
	m        *metrics.Registry // Prometheus 文本格式指标出口（压测观测）
}

// NewServer 创建 API 服务。
func NewServer(cfg *config.Config, alerts *alert.Repository, svc *incident.Service,
	rules *incident.RuleEngine, reg *provider.Registry, topo *topology.Service,
	authSvc *auth.Service, hub *pusher.Hub, pipe *pipeline.Pipeline,
	pbs *playbook.Service, audit *eventbus.AuditService, orch *orchestrator.Orchestrator,
	ntf *notifier.Service, channels *notifier.ConfigService, mw *maintenance.Service,
	aiSvc *ai.Service, wfSvc *workflow.Service) *Server {
	return &Server{
		cfg: cfg, alerts: alerts, svc: svc, rules: rules, reg: reg,
		topo: topo, authSvc: authSvc, hub: hub, push: hub, pipe: pipe,
		pbs: pbs, audit: audit, orch: orch, ntf: ntf, channels: channels,
		mw: mw, ai: aiSvc, wf: wfSvc,
	}
}

// SetMetrics 装配指标注册表（/metrics 出口；nil 时返回 404）。
func (s *Server) SetMetrics(m *metrics.Registry) { s.m = m }

// Register 注册全部路由。
func (s *Server) Register(g *gin.Engine) {
	g.GET("/healthz", s.handleHealth)
	g.GET("/metrics", s.handleMetrics) // Prometheus 抓取端点（不挂在租户鉴权组下）

	api := g.Group("/api/v1", s.authMiddleware())
	api.POST("/webhook/:provider", s.handleWebhook)
	api.GET("/alerts", s.handleListAlerts)
	api.GET("/alerts/:id", s.handleGetAlert)
	api.GET("/alerts/:id/history", s.handleAlertHistory)
	api.POST("/alerts/:id/ack", s.handleAckAlert)
	api.GET("/incidents", s.handleListIncidents)
	api.GET("/incidents/:id", s.handleGetIncident)
	api.POST("/incidents/:id/resolve", s.handleResolveIncident)
	api.POST("/incidents/:id/approval", s.handleApproveIncident)
	api.GET("/incidents/:id/audits", s.handleIncidentAudits)
	api.POST("/notify/test", s.handleNotifyTest)
	api.GET("/notify/channels", s.handleListNotifyChannels)
	api.PUT("/notify/channels/:name", s.handleUpdateNotifyChannel)
	api.POST("/notify/channels/:name/test", s.handleTestNotifyChannel)
	api.GET("/playbooks", s.handleListPlaybooks)
	api.POST("/playbooks", s.handleCreatePlaybook)
	api.PUT("/playbooks/:id", s.handleUpdatePlaybook)
	api.DELETE("/playbooks/:id", s.handleDeletePlaybook)
	api.GET("/maintenance-windows", s.handleListMaintenanceWindows)
	api.POST("/maintenance-windows", s.handleCreateMaintenanceWindow)
	api.PUT("/maintenance-windows/:id", s.handleUpdateMaintenanceWindow)
	api.DELETE("/maintenance-windows/:id", s.handleDeleteMaintenanceWindow)
	api.POST("/ai/cluster", s.handleAICluster)
	api.GET("/workflows", s.handleListWorkflows)
	api.POST("/workflows", s.handleCreateWorkflow)
	api.PUT("/workflows/:id", s.handleUpdateWorkflow)
	api.DELETE("/workflows/:id", s.handleDeleteWorkflow)
	api.POST("/workflows/:id/run", s.handleRunWorkflow)
	api.GET("/workflow-runs", s.handleListWorkflowRuns)
	api.GET("/rules", s.handleListRules)
	api.POST("/rules", s.handleCreateRule)
	api.GET("/providers", s.handleListProviders)
	api.GET("/topology", s.handleListTopology)
	api.POST("/topology", s.handleCreateTopology)
	api.GET("/topology/:name/impact", s.handleTopologyImpact)
	api.POST("/apikeys", s.handleCreateAPIKey)
	api.GET("/ws", s.handleWS)
}

// tenantID 从中间件上下文取当前租户。
func tenantID(c *gin.Context) string {
	return c.GetString(ctxTenant)
}

// handleHealth 健康检查（供 compose 探针/部署验证）。
func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "aiops"})
}

// handleMetrics Prometheus 文本格式指标（Phase 3 压测观测；未装配时 404）。
func (s *Server) handleMetrics(c *gin.Context) {
	if s.m == nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Content-Type", "text/plain; version=0.0.4")
	if err := s.m.Write(c.Writer); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// ctxTenant 中间件注入 tenant 的 context key。
const ctxTenant = "tenantID"
