package api

import (
	"context"
	"io"
	"net/http"

	"aiops/internal/alert"

	"github.com/gin-gonic/gin"
)

// handleWebhook 接收 provider 的 push webhook → 归一化 → 事件管线。
// URL: POST /api/v1/webhook/:provider（provider 为注册表 kind，如 prometheus）。
func (s *Server) handleWebhook(c *gin.Context) {
	kind := c.Param("provider")
	w, err := s.reg.MustWebhook(kind)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
		return
	}
	events, err := w.Parse(body, c.Request)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 以当前租户覆盖 provider 默认租户（多租户贯穿）
	tenant := tenantID(c)
	ctx := context.Background()
	accepted, processed := 0, 0
	for _, e := range events {
		e.TenantID = tenant
		accepted++
	}
	results := s.pipe.ProcessEvents(ctx, events)
	for _, r := range results {
		if r != nil {
			processed++
		}
	}
	_ = accepted
	c.JSON(http.StatusOK, gin.H{
		"accepted":  accepted,
		"processed": processed,
		"provider":  kind,
		"events":    eventsToDTO(events),
	})
}

// eventsToDTO 回显归一化后的入站事件（便于排查 provider 解析）。
func eventsToDTO(events []*alert.IngestEvent) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"name": e.Name, "severity": e.Severity, "status": e.Status,
			"labels": e.Labels,
		})
	}
	return out
}
