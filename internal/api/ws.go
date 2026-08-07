package api

import (
	"github.com/gin-gonic/gin"
)

// handleWS 实时推送 WebSocket 端点：GET /api/v1/ws（按租户路由广播）。
// 客户端可订阅 alert.new/update、incident.upsert、incident.state 等事件。
func (s *Server) handleWS(c *gin.Context) {
	s.hub.ServeWS(c.Writer, c.Request, tenantID(c))
}
