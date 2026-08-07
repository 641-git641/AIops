package api

import (
	"net/http"

	"aiops/internal/playbook"

	"github.com/gin-gonic/gin"
)

// playbookInput 创建/更新剧本的请求体。
type playbookInput struct {
	Name              string  `json:"name"`
	Level             string  `json:"level"`
	CommandTemplate   string  `json:"commandTemplate"`
	BlastRadiusFactor float64 `json:"blastRadiusFactor"`
	RollbackCommand   string  `json:"rollbackCommand"`
	DryRunFirst       bool    `json:"dryRunFirst"`
	Enabled           bool    `json:"enabled"`
}

// handleListPlaybooks 剧本列表（未配置时先补种默认剧本，保证演示可用）。
func (s *Server) handleListPlaybooks(c *gin.Context) {
	tid := tenantID(c)
	_ = s.pbs.EnsureDefaults(c.Request.Context(), tid)
	items, err := s.pbs.List(c.Request.Context(), tid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// handleCreatePlaybook 创建剧本。
func (s *Server) handleCreatePlaybook(c *gin.Context) {
	var in playbookInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 必填"})
		return
	}
	if in.Level == "" {
		in.Level = playbook.LevelL1
	}
	if in.CommandTemplate == "" {
		in.CommandTemplate = "true"
	}
	pb := &playbook.Playbook{
		TenantID: tenantID(c), Name: in.Name, Level: in.Level,
		CommandTemplate: in.CommandTemplate, BlastRadiusFactor: in.BlastRadiusFactor,
		RollbackCommand: in.RollbackCommand, DryRunFirst: in.DryRunFirst, Enabled: in.Enabled,
	}
	if err := s.pbs.Create(c.Request.Context(), pb); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, pb)
}

// handleUpdatePlaybook 更新剧本。
func (s *Server) handleUpdatePlaybook(c *gin.Context) {
	id := c.Param("id")
	var in playbookInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pb := &playbook.Playbook{
		ID: id, TenantID: tenantID(c), Name: in.Name, Level: in.Level,
		CommandTemplate: in.CommandTemplate, BlastRadiusFactor: in.BlastRadiusFactor,
		RollbackCommand: in.RollbackCommand, DryRunFirst: in.DryRunFirst, Enabled: in.Enabled,
	}
	if err := s.pbs.Update(c.Request.Context(), pb); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "playbook not found"})
		return
	}
	c.JSON(http.StatusOK, pb)
}

// handleDeletePlaybook 删除剧本。
func (s *Server) handleDeletePlaybook(c *gin.Context) {
	id := c.Param("id")
	if err := s.pbs.Delete(c.Request.Context(), tenantID(c), id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "playbook not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "deleted": true})
}
