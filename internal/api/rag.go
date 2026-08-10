package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"aiops/internal/rag"

	"github.com/gin-gonic/gin"
)

// rag.go：RAG 检索 API（RAG_PLAN §12.1）。
// 相似事故只读：返回带来源/可信等级/处置结果的证据，不注入任何自动动作；
// 检索全部 fail-open（RAG 故障返回 enabled 空结果，不阻塞事故详情）。

// similarItem 相似证据条目（incident 类附加结案信息，playbook 类只展示适用说明）。
type similarItem struct {
	DocumentID         string    `json:"documentId"`
	ChunkID            string    `json:"chunkId"`
	SourceType         string    `json:"sourceType"`
	SourceID           string    `json:"sourceId"`
	Title              string    `json:"title"`
	Section            string    `json:"section"`
	Content            string    `json:"content,omitempty"` // 证据正文（前端可折叠查看）
	Service            string    `json:"service,omitempty"`
	Severity           string    `json:"severity,omitempty"`
	TrustLevel         string    `json:"trustLevel"`
	Time               time.Time `json:"time"`
	Score              float64   `json:"score"`
	Reasons            []string  `json:"reasons"`
	ConfirmedRootCause string    `json:"confirmedRootCause,omitempty"`
	Resolution         string    `json:"resolution,omitempty"`
	Outcome            string    `json:"outcome,omitempty"`
	VerificationStatus string    `json:"verificationStatus,omitempty"`
}

// handleSimilarIncidents 相似历史事故（只读；RAG 关闭时返回 enabled=false）。
func (s *Server) handleSimilarIncidents(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if s.rag == nil {
		c.JSON(http.StatusOK, similarResponse{Enabled: false})
		return
	}
	inc, err := s.svc.Get(ctx, tenantID(c), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return
	}
	alerts, _ := s.svc.AlertsOf(ctx, tenantID(c), id)

	// 服务名优先取编排器 monitor 结论，其次告警白名单标签
	service := ""
	labels := map[string]string{}
	st, _ := s.svc.GetState(ctx, tenantID(c), id)
	if st != nil {
		if m := st.NodeResult("monitor"); m != nil {
			service, _ = m["service"].(string)
		}
	}
	alertNames := make([]string, 0, len(alerts))
	for i := range alerts {
		alertNames = append(alertNames, alerts[i].Name)
		var lm map[string]string
		_ = json.Unmarshal(alerts[i].Labels, &lm)
		for k, v := range lm {
			if k == "service" || k == "service_name" || k == "error_code" || k == "code" || k == "errno" {
				labels[k] = v
			}
		}
	}
	if service == "" {
		service = labels["service"]
	}

	q := rag.BuildQuery(tenantID(c), id, inc.Severity, inc.Summary, service, alertNames, labels, inc.CreatedAt)
	pack := s.rag.Retrieve(ctx, q)
	traceID, _ := s.rag.RecordTrace(ctx, tenantID(c), id, pack) // 落 trace（失败不影响响应）

	resp := similarResponse{
		Enabled:        true,
		TraceID:        traceID,
		DegradedReason: pack.DegradedReason,
		LatencyMS:      pack.LatencyMS,
		Items:          []similarItem{},
	}
	if len(pack.Selected) == 0 {
		c.JSON(http.StatusOK, resp)
		return
	}
	// 批量取结案信息（incident 类展示用；一次 SQL）
	incIDs := make([]string, 0, len(pack.Selected))
	for _, e := range pack.Selected {
		if e.SourceType == rag.SourceIncident {
			incIDs = append(incIDs, e.SourceID)
		}
	}
	resolutions := map[string]*rag.ResolutionInfo{}
	if len(incIDs) > 0 {
		if res, err := s.rag.ResolutionsOf(ctx, tenantID(c), incIDs); err == nil {
			resolutions = res
		}
	}
	for _, e := range pack.Selected {
		item := similarItem{
			DocumentID: e.DocumentID, ChunkID: e.ChunkID, SourceType: e.SourceType,
			SourceID: e.SourceID, Title: e.Title, Section: e.Section, Content: e.Content,
			Service: e.Service, Severity: e.Severity, TrustLevel: e.TrustLevel,
			Time: e.Time, Score: e.Score, Reasons: e.Reasons,
		}
		if r := resolutions[e.SourceID]; r != nil {
			item.ConfirmedRootCause = r.ConfirmedRootCause
			item.Resolution = r.Resolution
			item.Outcome = r.Outcome
			item.VerificationStatus = r.VerificationStatus
		}
		resp.Items = append(resp.Items, item)
	}
	c.JSON(http.StatusOK, resp)
}

type similarResponse struct {
	Enabled        bool             `json:"enabled"`
	TraceID        string           `json:"traceId,omitempty"`
	DegradedReason string           `json:"degradedReason,omitempty"`
	LatencyMS      map[string]int64 `json:"latencyMs,omitempty"`
	Items          []similarItem    `json:"items"`
}

// feedbackInput 反馈请求体。
type feedbackInput struct {
	Rating             string `json:"rating" binding:"required"` // useful|not_useful|wrong|unsafe
	DocumentID         string `json:"documentId" binding:"required"`
	TraceID            string `json:"traceId,omitempty"`
	CorrectedRootCause string `json:"correctedRootCause,omitempty"`
	Actor              string `json:"actor,omitempty"`
}

// handleRAGFeedback 提交检索反馈（RAG_PLAN §12.1；unsafe 触发 quarantine）。
func (s *Server) handleRAGFeedback(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	if s.rag == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "RAG 未启用"})
		return
	}
	if _, err := s.svc.Get(ctx, tenantID(c), id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return
	}
	var in feedbackInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// 反馈必须绑定本次检索：documentId 需属于该事故最近 trace 的候选/入选集合
	// （防同租户拿任意文档 ID 误 quarantine；跨租户由 SQL 过滤保证）
	if !s.rag.TraceHasDocument(ctx, tenantID(c), id, in.DocumentID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "documentId 不属于该事故的检索结果（反馈必须针对本次检索的证据）"})
		return
	}
	fb := &rag.RAGFeedback{
		TraceID:            in.TraceID,
		DocumentID:         in.DocumentID,
		Rating:             in.Rating,
		CorrectedRootCause: in.CorrectedRootCause,
		Actor:              in.Actor,
	}
	quarantined, err := s.rag.AddFeedback(ctx, tenantID(c), id, fb)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": fb.ID, "rating": fb.Rating, "quarantined": quarantined})
}

// handleRAGTrace 本次事故的检索轨迹（管理权限；RAG_PLAN §12.1）。
func (s *Server) handleRAGTrace(c *gin.Context) {
	if !s.ragAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_RAGADMINAPIS）"})
		return
	}
	ctx := c.Request.Context()
	items, err := s.rag.TracesOf(ctx, tenantID(c), c.Param("id"), 20)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// handleKnowledgeStatus 某事故的结案→发布→索引三态（KEEPON M3；无 admin gate——
// 事故自身信息，tenant 隔离；RAG 关闭时 enabled:false）。
func (s *Server) handleKnowledgeStatus(c *gin.Context) {
	if s.rag == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}
	incidentID := c.Param("id")
	// 校验事故存在（该租户下）
	if _, err := s.svc.Get(c.Request.Context(), tenantID(c), incidentID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return
	}
	c.JSON(http.StatusOK, s.rag.KnowledgeStatus(c.Request.Context(), tenantID(c), incidentID))
}

// handleRAGStatus 索引/检索状态（管理权限；运维手册第一入口）。
func (s *Server) handleRAGStatus(c *gin.Context) {
	if !s.ragAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_RAGADMINAPIS）"})
		return
	}
	if s.rag == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}
	c.JSON(http.StatusOK, s.rag.Status(c.Request.Context()))
}

// handleListGoldenQueries 评测查询列表（管理权限；?enabled=1 仅启用项）。
func (s *Server) handleListGoldenQueries(c *gin.Context) {
	if !s.ragAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_RAGADMINAPIS）"})
		return
	}
	if s.rag == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "RAG 未启用"})
		return
	}
	items, err := s.rag.ListGoldenQueries(c.Request.Context(), tenantID(c), c.Query("enabled") == "1")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// handleCreateGoldenQuery 创建评测查询。
func (s *Server) handleCreateGoldenQuery(c *gin.Context) {
	if !s.ragAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_RAGADMINAPIS）"})
		return
	}
	if s.rag == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "RAG 未启用"})
		return
	}
	var in rag.GoldenQuery
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.rag.CreateGoldenQuery(c.Request.Context(), tenantID(c), &in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, in)
}

// handleUpdateGoldenQuery 编辑/开关评测查询。
func (s *Server) handleUpdateGoldenQuery(c *gin.Context) {
	if !s.ragAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_RAGADMINAPIS）"})
		return
	}
	if s.rag == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "RAG 未启用"})
		return
	}
	var in rag.GoldenQuery
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.rag.UpdateGoldenQuery(c.Request.Context(), tenantID(c), c.Param("id"), &in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, in)
}

// handleDeleteGoldenQuery 删除评测查询。
func (s *Server) handleDeleteGoldenQuery(c *gin.Context) {
	if !s.ragAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_RAGADMINAPIS）"})
		return
	}
	if s.rag == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "RAG 未启用"})
		return
	}
	if err := s.rag.DeleteGoldenQuery(c.Request.Context(), tenantID(c), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": c.Param("id")})
}

// ragSourceGate sources/golden 公共管理门（RAG 关闭 → 404 带指引）。
func (s *Server) ragSourceGate(c *gin.Context) bool {
	if !s.ragAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_RAGADMINAPIS）"})
		return false
	}
	if s.rag == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "RAG 未启用（AIOPS_RAGENABLED=true AIOPS_RAGMODE=readonly）"})
		return false
	}
	return true
}

// handleSourcePreview 知识源上传预览（无副作用：front matter 解析 + 脱敏检查）。
func (s *Server) handleSourcePreview(c *gin.Context) {
	if !s.ragSourceGate(c) {
		return
	}
	var in rag.SourceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p, err := s.rag.PreviewSource(in.Name, in.Content)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, p)
}

// handleCreateSource 创建知识源草稿。
func (s *Server) handleCreateSource(c *gin.Context) {
	if !s.ragSourceGate(c) {
		return
	}
	var in rag.SourceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	src, err := s.rag.CreateSource(c.Request.Context(), tenantID(c), in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, src)
}

// handleListSources 知识源列表（含派生索引状态）。
func (s *Server) handleListSources(c *gin.Context) {
	if !s.ragSourceGate(c) {
		return
	}
	rows, err := s.rag.ListSources(c.Request.Context(), tenantID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": rows, "total": len(rows)})
}

// handleGetSource 知识源详情（含原文，编辑回显）。
func (s *Server) handleGetSource(c *gin.Context) {
	if !s.ragSourceGate(c) {
		return
	}
	src, err := s.rag.GetSource(c.Request.Context(), tenantID(c), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, src)
}

// handleUpdateSource 更新知识源内容（published 且内容变化 → 新版本并自动发布）。
func (s *Server) handleUpdateSource(c *gin.Context) {
	if !s.ragSourceGate(c) {
		return
	}
	var in rag.SourceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	src, err := s.rag.UpdateSource(c.Request.Context(), tenantID(c), c.Param("id"), in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, src)
}

// handlePublishSource 发布知识源（Sanitize 预检 → 版本+1 → 入队索引）。
func (s *Server) handlePublishSource(c *gin.Context) {
	if !s.ragSourceGate(c) {
		return
	}
	src, err := s.rag.PublishSource(c.Request.Context(), tenantID(c), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, src)
}

// handleDeleteSource 删除知识源（软删 + 入队撤销文档）。
func (s *Server) handleDeleteSource(c *gin.Context) {
	if !s.ragSourceGate(c) {
		return
	}
	if err := s.rag.DeleteSource(c.Request.Context(), tenantID(c), c.Param("id")); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": c.Param("id")})
}

// handleRAGReindex 全量重建索引（异步：入队 rebuild 后返回，轮询 rag/status 观察积压）。
func (s *Server) handleRAGReindex(c *gin.Context) {
	if !s.ragAdminAllowed(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "管理权限不足（AIOPS_RAGADMINAPIS）"})
		return
	}
	if s.rag == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "RAG 未启用"})
		return
	}
	n, err := s.rag.ReindexAll(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"enqueued": n, "note": "后台重建中，进度见 GET /rag/status"})
}

// ragAdminAllowed 管理端点鉴权：AIOPS_RAGADMINAPIS=false 一律拒绝；
// apikey 模式下仅 AIOPS_RAGADMINTENANTS 白名单租户可访问；noauth（本地/开发）直接放行。
func (s *Server) ragAdminAllowed(c *gin.Context) bool {
	if !s.cfg.RAGAdminAPIs {
		return false
	}
	if s.cfg.AuthType == "apikey" {
		cur := tenantID(c)
		for _, t := range strings.Split(s.cfg.RAGAdminTenants, ",") {
			if strings.TrimSpace(t) == cur {
				return true
			}
		}
		return false
	}
	return true
}
