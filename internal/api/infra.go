package api

import (
	"net/http"

	"aiops/internal/infra"

	"github.com/gin-gonic/gin"
)

// infra.go：基础设施指标端点（监控平板/总览数据源——主机 CPU/内存、PG 状态、docker 容器占用）。

// handleInfraMetrics 基础设施实时快照 + 近期历史（所有已认证租户可见——平台自身状态）。
func (s *Server) handleInfraMetrics(c *gin.Context) {
	if s.infra == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "基础设施指标未装配"})
		return
	}
	c.JSON(http.StatusOK, s.infra.Get(c.Request.Context()))
}

// SetInfra 装配基础设施指标服务（未装配时 404）。
func (s *Server) SetInfra(svc *infra.Service) { s.infra = svc }
