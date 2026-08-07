package api

import (
	"net/http"

	"aiops/internal/alert"
	"aiops/internal/notifier"

	"github.com/gin-gonic/gin"
)

// notifyTestInput 测试通知请求体（验证渠道配置，不落 incident）。
type notifyTestInput struct {
	Title    string `json:"title"`
	Content  string `json:"content"`
	Severity string `json:"severity"`
}

// handleNotifyTest 手动测试通知渠道（调试用：快速验证钉钉/飞书等 webhook 是否可达）。
func (s *Server) handleNotifyTest(c *gin.Context) {
	var in notifyTestInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if s.ntf == nil || s.ntf.Len() == 0 {
		c.JSON(http.StatusOK, gin.H{
			"message":  "未配置通知渠道，请设置 AIOPS_NOTIFIERCHANNELS 与对应 webhook",
			"channels": []string{},
			"results":  []notifier.Result{},
		})
		return
	}
	if in.Title == "" {
		in.Title = "AIops 通知测试"
	}
	if in.Content == "" {
		in.Content = "这是一条来自 AIops 的测试通知，用于验证渠道配置。"
	}
	if in.Severity == "" {
		in.Severity = alert.SeverityInfo
	}
	results := s.ntf.Notify(c.Request.Context(), notifier.Message{
		Title: in.Title, Content: in.Content, Severity: in.Severity, Action: "test",
	})
	c.JSON(http.StatusOK, gin.H{
		"channels": s.ntf.Enabled(),
		"results":  results,
	})
}
