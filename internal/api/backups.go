package api

import (
	"errors"
	"net/http"
	"strconv"

	"aiops/internal/backup"

	"github.com/gin-gonic/gin"
)

// backups.go：备份执行记录列表 + 手动触发（KEEPON 4.1）。
// 恢复是高风险操作，不做网页按钮——仅通过 scripts/restore.sh（交互二次确认）。

// backupsUnavailable 备份服务未装配（AIOPS_BACKUPENABLED=false）→ 404 带启用指引。
func (s *Server) backupsUnavailable(c *gin.Context) bool {
	if s.backups == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "备份未启用（设置 AIOPS_BACKUPENABLED=true 并重启后端）"})
		return true
	}
	return false
}

// handleListBackups 最近 20 条备份记录（新在前；含状态/校验/镜像字段供前端渲染）。
func (s *Server) handleListBackups(c *gin.Context) {
	if s.backupsUnavailable(c) {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if offset < 1 {
		offset = 1
	}
	runs, total, err := s.backups.List(c.Request.Context(), limit, (offset-1)*limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": runs, "total": total})
}

// handleCreateBackup 手动触发备份（运行中 → 409）。
func (s *Server) handleCreateBackup(c *gin.Context) {
	if s.backupsUnavailable(c) {
		return
	}
	run, err := s.backups.RunManual(c.Request.Context())
	if err != nil {
		if errors.Is(err, backup.ErrRunning) {
			c.JSON(http.StatusConflict, gin.H{"error": "备份正在执行中，请稍后再试"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if run.Status != "success" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": run.ErrorDetail, "run": run})
		return
	}
	c.JSON(http.StatusCreated, run)
}
