package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// overview.go：总览聚合端点（KEEPON 5.1）——一次回答"现在有没有事、哪里有事"。

// handleOverview 总览聚合：GET /api/v1/overview?window=24h（默认 24h，上限 7d）。
func (s *Server) handleOverview(c *gin.Context) {
	if s.ov == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "总览服务未装配"})
		return
	}
	window := 24 * time.Hour
	if w := c.Query("window"); w != "" {
		d, err := time.ParseDuration(w)
		if err != nil || d <= 0 || d > 7*24*time.Hour {
			c.JSON(http.StatusBadRequest, gin.H{"error": "window 非法（如 24h/6h，上限 168h）"})
			return
		}
		window = d
	}
	ov, err := s.ov.Get(c.Request.Context(), tenantID(c), window)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ov)
}
