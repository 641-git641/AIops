package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// handleAICluster 对当前租户立即执行一次告警聚类（验收②演示入口）。
// 达到阈值的簇确认生成 Incident（source=ai），已存在同 key 的 open incident 则合并。
func (s *Server) handleAICluster(c *gin.Context) {
	res, err := s.ai.RunOnce(c.Request.Context(), tenantID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}
