package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// channelUpdateInput 渠道配置更新请求体。
type channelUpdateInput struct {
	Enabled bool   `json:"enabled"` // 启用开关（false = 停用该渠道）
	Webhook string `json:"webhook"` // webhook 地址（可空 = 无地址不发送）
}

// handleListNotifyChannels 渠道配置列表（含未启用，UI 管理页展示）。
func (s *Server) handleListNotifyChannels(c *gin.Context) {
	items, err := s.channels.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": len(items)})
}

// handleUpdateNotifyChannel 保存渠道配置并热重载（新建或更新）。
func (s *Server) handleUpdateNotifyChannel(c *gin.Context) {
	name := c.Param("name")
	var in channelUpdateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.channels.Save(c.Request.Context(), name, in.Enabled, in.Webhook); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfg, err := s.channels.Get(c.Request.Context(), name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// handleTestNotifyChannel 测试指定渠道（用 DB 配置，无需启用，便于验证新 webhook）。
func (s *Server) handleTestNotifyChannel(c *gin.Context) {
	name := c.Param("name")
	r, err := s.channels.Test(c.Request.Context(), name)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, r)
}
