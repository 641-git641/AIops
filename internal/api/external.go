package api

import (
	"net/http"

	"aiops/internal/external"

	"github.com/gin-gonic/gin"
)

// external.go：外部可观测系统入口 API（KEEPON 6.2）。
// 无敏感字段（不含密钥），管理不设独立开关；全部按租户隔离。

// handleListExternalSystems 列表（侧边栏与设置页共用）。
func (s *Server) handleListExternalSystems(c *gin.Context) {
	if s.external == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "外部系统未装配"})
		return
	}
	items, err := s.external.List(c.Request.Context(), tenantID(c))
	if err != nil {
		serverErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// handleCreateExternalSystem 新建。
func (s *Server) handleCreateExternalSystem(c *gin.Context) {
	if s.external == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "外部系统未装配"})
		return
	}
	var in external.System
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法: " + err.Error()})
		return
	}
	sys, err := s.external.Create(c.Request.Context(), tenantID(c), in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, sys)
}

// handleUpdateExternalSystem 更新。
func (s *Server) handleUpdateExternalSystem(c *gin.Context) {
	if s.external == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "外部系统未装配"})
		return
	}
	var in external.System
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法: " + err.Error()})
		return
	}
	sys, err := s.external.Update(c.Request.Context(), tenantID(c), c.Param("id"), in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if sys == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "外部系统不存在"})
		return
	}
	c.JSON(http.StatusOK, sys)
}

// handleDeleteExternalSystem 删除。
func (s *Server) handleDeleteExternalSystem(c *gin.Context) {
	if s.external == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "外部系统未装配"})
		return
	}
	if err := s.external.Delete(c.Request.Context(), tenantID(c), c.Param("id")); err != nil {
		serverErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// SetExternal 装配外部系统服务（nil 时端点 404）。
func (s *Server) SetExternal(svc *external.Service) { s.external = svc }
