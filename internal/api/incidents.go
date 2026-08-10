package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"aiops/internal/alert"
	"aiops/internal/incident"
	"aiops/internal/orchestrator"
	"aiops/internal/pusher"

	"github.com/gin-gonic/gin"
)

// handleListIncidents 事故列表：?status=&limit=&offset=
func (s *Server) handleListIncidents(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	items, total, err := s.svc.List(c.Request.Context(), tenantID(c), c.Query("status"), limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total, "limit": limit, "offset": offset})
}

// incidentDetail 事故详情聚合（含关联告警 + 编排器状态 + 各 Agent 结果 + 结案数据）。
type incidentDetail struct {
	incident.Incident
	Alerts      []alert.DTO          `json:"alerts"`
	State       *incident.State      `json:"state,omitempty"`
	NodeResults map[string]any       `json:"nodeResults,omitempty"`
	Resolution  *incident.Resolution `json:"resolution,omitempty"` // 结案数据契约（RAG Phase 0）
}

// handleGetIncident 事故详情。
func (s *Server) handleGetIncident(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()
	inc, err := s.svc.Get(ctx, tenantID(c), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return
	}
	alerts, _ := s.svc.AlertsOf(ctx, tenantID(c), id)

	detail := incidentDetail{Incident: *inc}
	detail.Alerts = make([]alert.DTO, 0, len(alerts))
	for i := range alerts {
		detail.Alerts = append(detail.Alerts, alerts[i].ToDTO())
	}
	detail.Resolution, _ = s.svc.GetResolution(ctx, tenantID(c), id)

	st, err := s.svc.GetState(ctx, tenantID(c), id)
	if err == nil && st != nil {
		detail.State = st
		m := map[string]any{}
		if err := json.Unmarshal(st.NodeResults, &m); err == nil {
			detail.NodeResults = m
		}
	}
	c.JSON(http.StatusOK, detail)
}

// approvalInput 审批决策请求体。
type approvalInput struct {
	Decision string `json:"decision" binding:"required"` // approve|reject
	Actor    string `json:"actor"`                       // oncall-engineer|tech-lead|user（默认 user）
	Reason   string `json:"reason"`
}

// handleApproveIncident 分级审批：审批自愈动作（安全护栏链最后一道）。
func (s *Server) handleApproveIncident(c *gin.Context) {
	var in approvalInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Actor == "" {
		in.Actor = "user"
	}
	id := c.Param("id")
	ctx := c.Request.Context()
	if err := s.orch.ApproveAction(ctx, tenantID(c), id, in.Decision, in.Actor, in.Reason); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// incident 状态：approve 闭环 resolved；reject 保持 open（人工跟进）
	status := incident.IncidentResolved
	if in.Decision == orchestrator.DecisionReject {
		status = incident.IncidentOpen
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "decision": in.Decision, "status": status})
}

// handleIncidentAudits 事故的审计轨迹（who/when/what/why）。
func (s *Server) handleIncidentAudits(c *gin.Context) {
	id := c.Param("id")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	items, total, err := s.audit.List(c.Request.Context(), tenantID(c), id, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total})
}

// handleResolveIncident 解决事故（闭环）。
func (s *Server) handleResolveIncident(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()
	if err := s.svc.Resolve(ctx, tenantID(c), id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return
	}
	inc, _ := s.svc.Get(ctx, tenantID(c), id)
	if inc != nil {
		s.push.Broadcast(pusher.Event{Type: "incident.upsert", TenantID: inc.TenantID, Data: inc})
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "status": incident.IncidentResolved})
}

// resolutionInput 结案数据请求体（RAG Phase 0 数据契约）。
type resolutionInput struct {
	ConfirmedRootCause string `json:"confirmedRootCause"`
	Resolution         string `json:"resolution"`
	Outcome            string `json:"outcome"`
	Verification       string `json:"verification"`
	VerificationStatus string `json:"verificationStatus"`
	PostmortemURL      string `json:"postmortemUrl"`
	ResolvedBy         string `json:"resolvedBy"`
}

// handleUpsertResolution 写入/更新结案数据（仅已解决事故；一对一幂等，version 递增）。
func (s *Server) handleUpsertResolution(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()
	inc, err := s.svc.Get(ctx, tenantID(c), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident not found"})
		return
	}
	if inc.Status != incident.IncidentResolved {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅已解决事故可填写结案数据（先 POST /incidents/:id/resolve）"})
		return
	}
	var in resolutionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	r := &incident.Resolution{
		ConfirmedRootCause: in.ConfirmedRootCause,
		Resolution:         in.Resolution,
		Outcome:            in.Outcome,
		Verification:       in.Verification,
		VerificationStatus: in.VerificationStatus,
		PostmortemURL:      in.PostmortemURL,
		ResolvedBy:         in.ResolvedBy,
	}
	if err := s.svc.UpsertResolution(ctx, tenantID(c), id, r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "version": r.Version, "verificationStatus": r.VerificationStatus})
}
