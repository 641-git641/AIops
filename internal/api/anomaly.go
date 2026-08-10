package api

import (
	"net/http"
	"strconv"

	"aiops/internal/anomaly"

	"github.com/gin-gonic/gin"
)

// anomaly.go：异常检测端点（KEEPON 七，P3）。只读提示，不自动动作。

// handleAlertAnomalies 告警数量异常检测（?hours=24&historyDays=7）。
func (s *Server) handleAlertAnomalies(c *gin.Context) {
	if s.anomalySvc == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "异常检测未装配"})
		return
	}
	hours, _ := strconv.Atoi(c.DefaultQuery("hours", "24"))
	history, _ := strconv.Atoi(c.DefaultQuery("historyDays", "7"))
	items, err := s.anomalySvc.AlertAnomalies(c.Request.Context(), tenantID(c), anomaly.Options{
		Hours: hours, HistoryDays: history,
	})
	if err != nil {
		serverErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// SetAnomaly 装配异常检测服务（nil 时端点 404）。
func (s *Server) SetAnomaly(svc *anomaly.Service) { s.anomalySvc = svc }
