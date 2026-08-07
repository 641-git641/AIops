package api

import (
	"errors"
	"net/http"
	"strconv"

	"aiops/internal/workflow"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// workflowInput 工作流创建/更新入参。
type workflowInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	YAML        string `json:"yaml"`
	Enabled     *bool  `json:"enabled"` // 指针：更新时区分「未传」与「false」
}

// handleListWorkflows 列出租户全部工作流。
func (s *Server) handleListWorkflows(c *gin.Context) {
	items, err := s.wf.List(c.Request.Context(), tenantID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// handleCreateWorkflow 创建工作流（YAML 先解析校验）。
func (s *Server) handleCreateWorkflow(c *gin.Context) {
	var in workflowInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.YAML == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "yaml 必填"})
		return
	}
	wf := &workflow.Workflow{
		TenantID:    tenantID(c),
		Name:        in.Name,
		Description: in.Description,
		YAML:        in.YAML,
		Enabled:     in.Enabled == nil || *in.Enabled,
	}
	if err := s.wf.Create(c.Request.Context(), wf); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()}) // 含 YAML 解析错误
		return
	}
	c.JSON(http.StatusCreated, wf)
}

// handleUpdateWorkflow 更新工作流（yaml/enabled；部分更新：未传字段保留现值）。
// 先读现有记录再合并，避免 in.Enabled 为 nil 时零值 false 误禁用工作流。
func (s *Server) handleUpdateWorkflow(c *gin.Context) {
	id := c.Param("id")
	var in workflowInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	tenant := tenantID(c)
	existing, err := s.wf.Get(c.Request.Context(), tenant, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if in.YAML != "" {
		existing.YAML = in.YAML
	}
	if in.Enabled != nil {
		existing.Enabled = *in.Enabled
	}
	if err := s.wf.Update(c.Request.Context(), existing); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()}) // 含 YAML 解析错误
		return
	}
	got, err := s.wf.Get(c.Request.Context(), tenant, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, got)
}

// handleDeleteWorkflow 删除工作流。
func (s *Server) handleDeleteWorkflow(c *gin.Context) {
	if err := s.wf.Delete(c.Request.Context(), tenantID(c), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "deleted": true})
}

// workflowRunInput 手动触发工作流入参。
type workflowRunInput struct {
	AlertID string `json:"alert_id"` // 可选：携带告警执行（按工作流 filters 匹配）
}

// handleRunWorkflow 手动触发工作流执行（interval/manual 触发演示入口；不做并发防重，便于反复演练）。
func (s *Server) handleRunWorkflow(c *gin.Context) {
	var in workflowRunInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	run, err := s.wf.RunManual(c.Request.Context(), tenantID(c), c.Param("id"), in.AlertID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 工作流或携带的告警不存在：把真实原因（含 alert_id）返回，避免误报为「工作流不存在」
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"runId": run.ID, "workflowId": run.WorkflowID, "trigger": run.Trigger, "status": run.Status})
}

// handleListWorkflowRuns 分页列出运行记录（验收④：核对并发防重后的实际执行次数）。
func (s *Server) handleListWorkflowRuns(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	items, total, err := s.wf.ListRuns(c.Request.Context(), tenantID(c), limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total, "limit": limit, "offset": offset})
}
