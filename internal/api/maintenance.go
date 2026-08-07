package api

import (
	"net/http"
	"time"

	"aiops/internal/maintenance"

	"github.com/gin-gonic/gin"
)

// maintenanceInput 创建/更新维护窗口的请求体。
type maintenanceInput struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	Service     string `json:"service"`
	Severity    string `json:"severity"`
	StartsAt    string `json:"startsAt"` // RFC3339
	EndsAt      string `json:"endsAt"`   // RFC3339
	Reason      string `json:"reason"`
	Enabled     bool   `json:"enabled"`
}

// handleListMaintenanceWindows 维护窗口列表（含当前生效中的标注由前端计算）。
func (s *Server) handleListMaintenanceWindows(c *gin.Context) {
	items, err := s.mw.List(c.Request.Context(), tenantID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// handleCreateMaintenanceWindow 创建维护窗口。
func (s *Server) handleCreateMaintenanceWindow(c *gin.Context) {
	in, starts, ends, ok := bindMaintenanceInput(c)
	if !ok {
		return
	}
	w := &maintenance.Window{
		TenantID: tenantID(c), Name: in.Name, Fingerprint: in.Fingerprint,
		Service: in.Service, Severity: in.Severity,
		StartsAt: starts, EndsAt: ends,
		Enabled: in.Enabled, Reason: in.Reason,
	}
	if err := s.mw.Create(c.Request.Context(), w); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.pipe.InvalidateMaintenance(w.TenantID)
	c.JSON(http.StatusCreated, w)
}

// handleUpdateMaintenanceWindow 更新维护窗口。
func (s *Server) handleUpdateMaintenanceWindow(c *gin.Context) {
	in, starts, ends, ok := bindMaintenanceInput(c)
	if !ok {
		return
	}
	w := &maintenance.Window{
		ID: c.Param("id"), TenantID: tenantID(c), Name: in.Name,
		Fingerprint: in.Fingerprint, Service: in.Service, Severity: in.Severity,
		StartsAt: starts, EndsAt: ends, Enabled: in.Enabled, Reason: in.Reason,
	}
	if err := s.mw.Update(c.Request.Context(), w); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "maintenance window not found"})
		return
	}
	s.pipe.InvalidateMaintenance(w.TenantID)
	c.JSON(http.StatusOK, w)
}

// handleDeleteMaintenanceWindow 删除维护窗口。
func (s *Server) handleDeleteMaintenanceWindow(c *gin.Context) {
	id := c.Param("id")
	if err := s.mw.Delete(c.Request.Context(), tenantID(c), id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "maintenance window not found"})
		return
	}
	s.pipe.InvalidateMaintenance(tenantID(c))
	c.JSON(http.StatusOK, gin.H{"id": id, "deleted": true})
}

// bindMaintenanceInput 校验并解析请求体（公共校验：时间合法 + 至少一个匹配条件），
// 返回解析后的 UTC 时间。
func bindMaintenanceInput(c *gin.Context) (maintenanceInput, time.Time, time.Time, bool) {
	var in maintenanceInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return in, time.Time{}, time.Time{}, false
	}
	if in.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 必填"})
		return in, time.Time{}, time.Time{}, false
	}
	starts, err1 := time.Parse(time.RFC3339, in.StartsAt)
	ends, err2 := time.Parse(time.RFC3339, in.EndsAt)
	if err1 != nil || err2 != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "startsAt/endsAt 需为 RFC3339 时间"})
		return in, time.Time{}, time.Time{}, false
	}
	if !ends.After(starts) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endsAt 必须晚于 startsAt"})
		return in, time.Time{}, time.Time{}, false
	}
	if in.Fingerprint == "" && in.Service == "" && in.Severity == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "至少配置一个匹配条件（fingerprint/service/severity）"})
		return in, time.Time{}, time.Time{}, false
	}
	return in, starts.UTC(), ends.UTC(), true
}
