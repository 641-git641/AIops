// Package api 实现 HTTP API 层：Gin 路由 + 租户中间件 + handlers + WS 端点。
package api

import (
	"net/http"

	"aiops/internal/ai"
	"aiops/internal/alert"
	"aiops/internal/assistant"
	"aiops/internal/auth"
	"aiops/internal/backup"
	"aiops/internal/config"
	"aiops/internal/eventbus"
	"aiops/internal/incident"
	"aiops/internal/infra"
	"aiops/internal/maintenance"
	"aiops/internal/metrics"
	"aiops/internal/modelcfg"
	"aiops/internal/notifier"
	"aiops/internal/orchestrator"
	"aiops/internal/overview"
	"aiops/internal/pipeline"
	"aiops/internal/playbook"
	"aiops/internal/provider"
	"aiops/internal/pusher"
	"aiops/internal/rag"
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
	rag      *rag.Service            // RAG 知识检索（RAG 关闭时为 nil，相关端点按禁用响应）
	mc       *modelcfg.ConfigService // 大模型 API 接入管理（模型配置端点）
	tickets  *auth.TicketStore       // WS 握手短期 ticket（apikey 模式）
	backups  *backup.Service         // 数据备份（KEEPON 4.1；BackupEnabled=false 时可能为 nil）
	ov       *overview.Service       // 总览聚合（KEEPON 5.1；未装配时 404）
	infra    *infra.Service          // 基础设施指标（监控平板数据源；未装配时 404）
	assist   *assistant.Service // 对话式 Agent（AGENT_CHAT_PLAN；功能关闭时为 nil，端点 404）
	assistBr *assistant.Broker  // run 事件 broker（SSE 数据源）
	m        *metrics.Registry       // Prometheus 文本格式指标出口（压测观测）
}

// NewServer 创建 API 服务。
func NewServer(cfg *config.Config, alerts *alert.Repository, svc *incident.Service,
	rules *incident.RuleEngine, reg *provider.Registry, topo *topology.Service,
	authSvc *auth.Service, hub *pusher.Hub, pipe *pipeline.Pipeline,
	pbs *playbook.Service, audit *eventbus.AuditService, orch *orchestrator.Orchestrator,
	ntf *notifier.Service, channels *notifier.ConfigService, mw *maintenance.Service,
	aiSvc *ai.Service, wfSvc *workflow.Service, ragSvc *rag.Service, mcSvc *modelcfg.ConfigService,
	tickets *auth.TicketStore, backupSvc *backup.Service, assistSvc *assistant.Service,
	assistBr *assistant.Broker) *Server {
	return &Server{
		cfg: cfg, alerts: alerts, svc: svc, rules: rules, reg: reg,
		topo: topo, authSvc: authSvc, hub: hub, push: hub, pipe: pipe,
		pbs: pbs, audit: audit, orch: orch, ntf: ntf, channels: channels,
		mw: mw, ai: aiSvc, wf: wfSvc, rag: ragSvc, mc: mcSvc, tickets: tickets,
		backups: backupSvc, assist: assistSvc, assistBr: assistBr,
	}
}

// SetMetrics 装配指标注册表（/metrics 出口；nil 时返回 404）。
func (s *Server) SetMetrics(m *metrics.Registry) { s.m = m }

// SetOverview 装配总览聚合服务（未装配时 /overview 返回 404）。
func (s *Server) SetOverview(ov *overview.Service) { s.ov = ov }

// Register 注册全部路由。
func (s *Server) Register(g *gin.Engine) {
	g.GET("/healthz", s.handleHealth)
	g.GET("/metrics", s.handleMetrics) // Prometheus 抓取端点（不挂在租户鉴权组下）

	// 公共端点（无租户鉴权）：WS 用短期 ticket 握手；auth/status 供前端路由守卫判定
	public := g.Group("/api/v1")
	public.GET("/ws", s.wsAuthMiddleware(), s.handleWS)
	public.GET("/auth/status", s.handleAuthStatus)

	api := g.Group("/api/v1", s.authMiddleware())
	api.GET("/ws/ticket", s.handleWSTicket) // 受保护：签发 WS 握手 ticket
	api.POST("/webhook/:provider", s.handleWebhook)
	api.GET("/alerts", s.handleListAlerts)
	api.GET("/alerts/:id", s.handleGetAlert)
	api.GET("/alerts/:id/history", s.handleAlertHistory)
	api.POST("/alerts/:id/ack", s.handleAckAlert)
	api.GET("/incidents", s.handleListIncidents)
	api.GET("/incidents/:id", s.handleGetIncident)
	api.POST("/incidents/:id/resolve", s.handleResolveIncident)
	api.PUT("/incidents/:id/resolution", s.handleUpsertResolution) // 结案数据契约（RAG Phase 0）
	api.POST("/incidents/:id/approval", s.handleApproveIncident)
	api.GET("/incidents/:id/audits", s.handleIncidentAudits)
	// RAG（RAG_PLAN §12.1）：相似事故对所有已认证租户开放；trace/status/reindex 需管理权限
	api.GET("/incidents/:id/similar", s.handleSimilarIncidents)
	api.POST("/incidents/:id/rag-feedback", s.handleRAGFeedback)
	api.GET("/incidents/:id/rag-trace", s.handleRAGTrace)
	api.GET("/incidents/:id/knowledge-status", s.handleKnowledgeStatus) // 结案→发布→索引三态（KEEPON M3）
	api.GET("/rag/status", s.handleRAGStatus)
	api.POST("/rag/reindex", s.handleRAGReindex)
	// 检索 golden set 评测（KEEPON M3；管理权限）
	api.GET("/rag/golden-queries", s.handleListGoldenQueries)
	api.POST("/rag/golden-queries", s.handleCreateGoldenQuery)
	api.PUT("/rag/golden-queries/:id", s.handleUpdateGoldenQuery)
	api.DELETE("/rag/golden-queries/:id", s.handleDeleteGoldenQuery)
	// 个人知识源（KEEPON M3；管理权限）
	api.POST("/rag/sources/preview", s.handleSourcePreview)
	api.GET("/rag/sources", s.handleListSources)
	api.POST("/rag/sources", s.handleCreateSource)
	api.GET("/rag/sources/:id", s.handleGetSource)
	api.PUT("/rag/sources/:id", s.handleUpdateSource)
	api.POST("/rag/sources/:id/publish", s.handlePublishSource)
	api.DELETE("/rag/sources/:id", s.handleDeleteSource)
	// 大模型 API 接入管理（携带密钥，独立管理开关 AIOPS_MODELCFGADMINAPIS）
	api.GET("/model-configs", s.handleListModelConfigs)
	api.PUT("/model-configs/:key", s.handleUpdateModelConfig)
	api.POST("/model-configs/:key/test", s.handleTestModelConfig)
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
	api.GET("/apikeys", s.handleListAPIKeys)
	api.DELETE("/apikeys/:id", s.handleDeleteAPIKey)
	api.POST("/apikeys/owner/regenerate", s.handleRegenerateOwner)
	// 数据备份（KEEPON 4.1；服务未装配（BackupEnabled=false）时返回 404）
	api.GET("/backups", s.handleListBackups)
	api.POST("/backups", s.handleCreateBackup)
	// 总览聚合（KEEPON 5.1；默认首页数据源）
	api.GET("/overview", s.handleOverview)
	// 基础设施指标（主机/PostgreSQL/docker 容器；监控平板数据源）
	api.GET("/infra/metrics", s.handleInfraMetrics)
	// 对话式 Agent（AGENT_CHAT_PLAN §7；功能关闭时全部 404）
	api.POST("/assistant/threads", s.handleCreateThread)
	api.GET("/assistant/threads", s.handleListThreads)
	api.GET("/assistant/threads/:id", s.handleGetThread)
	api.GET("/assistant/threads/:id/messages", s.handleListThreadMessages)
	api.DELETE("/assistant/threads/:id", s.handleDeleteThread)
	api.POST("/assistant/threads/:id/messages", s.handleCreateMessage)
	api.POST("/assistant/messages/:id/feedback", s.handleMessageFeedback)
	api.GET("/assistant/runs/:id", s.handleGetRun)
	api.GET("/assistant/runs/:id/events", s.handleRunEvents) // SSE（?lastSequence=N 续传）
	api.POST("/assistant/runs/:id/cancel", s.handleCancelRun)
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
