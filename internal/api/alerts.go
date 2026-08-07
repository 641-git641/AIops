package api

import (
	"net/http"
	"strconv"
	"time"

	"aiops/internal/alert"
	"aiops/internal/pusher"

	"github.com/gin-gonic/gin"
)

// handleListAlerts 告警列表：?status=&severity=&provider=&limit=&offset=
func (s *Server) handleListAlerts(c *gin.Context) {
	q := alert.Query{TenantID: tenantID(c)}
	q.Status = c.Query("status")
	q.Severity = c.Query("severity")
	q.Provider = c.Query("provider")
	if v := c.Query("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.From = &t
		}
	}
	if v := c.Query("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			q.To = &t
		}
	}
	q.Limit, _ = strconv.Atoi(c.DefaultQuery("limit", "50"))
	q.Offset, _ = strconv.Atoi(c.DefaultQuery("offset", "0"))

	items, total, err := s.alerts.List(c.Request.Context(), q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	dtos := make([]alert.DTO, 0, len(items))
	for i := range items {
		dtos = append(dtos, items[i].ToDTO())
	}
	c.JSON(http.StatusOK, gin.H{"items": dtos, "total": total, "limit": q.Limit, "offset": q.Offset})
}

// handleGetAlert 告警详情。
func (s *Server) handleGetAlert(c *gin.Context) {
	a, err := s.alerts.Get(c.Request.Context(), tenantID(c), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "alert not found"})
		return
	}
	c.JSON(http.StatusOK, a.ToDTO())
}

// handleAlertHistory 告警去重演进历史（同 fingerprint）。
func (s *Server) handleAlertHistory(c *gin.Context) {
	a, err := s.alerts.Get(c.Request.Context(), tenantID(c), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "alert not found"})
		return
	}
	items, err := s.alerts.History(c.Request.Context(), tenantID(c), a.Fingerprint, 50)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	dtos := make([]alert.DTO, 0, len(items))
	for i := range items {
		dtos = append(dtos, items[i].ToDTO())
	}
	c.JSON(http.StatusOK, gin.H{"items": dtos})
}

// handleAckAlert 确认告警（认领）：状态 → acknowledged。
func (s *Server) handleAckAlert(c *gin.Context) {
	a, err := s.alerts.UpdateStatus(c.Request.Context(), tenantID(c), c.Param("id"), alert.StatusAcknowledged)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "alert not found"})
		return
	}
	s.push.Broadcast(pusher.Event{Type: "alert.update", TenantID: a.TenantID, Data: a.ToDTO()})
	c.JSON(http.StatusOK, a.ToDTO())
}
