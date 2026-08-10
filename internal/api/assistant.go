package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"aiops/internal/assistant"

	"github.com/gin-gonic/gin"
)

// assistant.go：对话式 Agent API（AGENT_CHAT_PLAN §7）。
// 聊天流用 run 级 SSE（全仓第一个出站 SSE）；全局 WS 继续服务告警/事故广播。
// 功能默认关闭（AIOPS_ASSISTANTENABLED=false）→ assist 为 nil，全部端点 404。

// assistantGate 服务未装配（功能关闭）时 404。
func (s *Server) assistantGate(c *gin.Context) bool {
	if s.assist == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "AI 助手未启用（AIOPS_ASSISTANTENABLED=false 或未配置模型）"})
		return false
	}
	return true
}

// handleCreateThread 新建会话。
func (s *Server) handleCreateThread(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	var in assistant.CreateThreadInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法: " + err.Error()})
		return
	}
	th, err := s.assist.CreateThread(c.Request.Context(), tenantID(c), in)
	if errors.Is(err, assistant.ErrBadContext) || errors.Is(err, assistant.ErrIncidentNotFound) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		serverErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, th)
}

// handleListThreads 会话列表。
func (s *Server) handleListThreads(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	threads, err := s.assist.ListThreads(c.Request.Context(), tenantID(c))
	if err != nil {
		serverErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": threads})
}

// handleGetThread 会话详情。
func (s *Server) handleGetThread(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	th, err := s.assist.GetThread(c.Request.Context(), tenantID(c), c.Param("id"))
	if err != nil {
		serverErr(c, err)
		return
	}
	if th == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return
	}
	c.JSON(http.StatusOK, th)
}

// handleListThreadMessages 会话消息列表（历史渲染）。
func (s *Server) handleListThreadMessages(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	msgs, err := s.assist.ListThreadMessages(c.Request.Context(), tenantID(c), c.Param("id"))
	if errors.Is(err, assistant.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return
	}
	if err != nil {
		serverErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": msgs})
}

// handleDeleteThread 软删会话。
func (s *Server) handleDeleteThread(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	err := s.assist.DeleteThread(c.Request.Context(), tenantID(c), c.Param("id"))
	if errors.Is(err, assistant.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return
	}
	if err != nil {
		serverErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleCreateMessage 发送消息（要求 Idempotency-Key 防重复；202 + runId）。
func (s *Server) handleCreateMessage(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	idemKey := c.GetHeader("Idempotency-Key")
	if idemKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 Idempotency-Key 头（防重复提交）"})
		return
	}
	var in struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法: " + err.Error()})
		return
	}
	res, err := s.assist.SendMessage(c.Request.Context(), tenantID(c), c.Param("id"), in.Content, idemKey)
	if errors.Is(err, assistant.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return
	}
	if err != nil {
		serverErr(c, err)
		return
	}
	c.JSON(http.StatusAccepted, res)
}

// handleGetRun 查询 run（含步骤）。
func (s *Server) handleGetRun(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	run, steps, err := s.assist.GetRun(c.Request.Context(), tenantID(c), c.Param("id"))
	if errors.Is(err, assistant.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "run 不存在"})
		return
	}
	if err != nil {
		serverErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"run": run, "steps": steps})
}

// handleMessageFeedback 消息反馈（useful/not_useful + 可选备注；仅最终回答可评）。
func (s *Server) handleMessageFeedback(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	var in struct {
		Rating string `json:"rating"`
		Note   string `json:"note"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法: " + err.Error()})
		return
	}
	err := s.assist.AddFeedback(c.Request.Context(), tenantID(c), c.Param("id"), in.Rating, in.Note)
	if errors.Is(err, assistant.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "消息不存在"})
		return
	}
	if errors.Is(err, assistant.ErrBadFeedback) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		serverErr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// handleListThreadActions 线程动作请求列表（页面恢复确认卡）。
func (s *Server) handleListThreadActions(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	items, err := s.assist.ListThreadActions(c.Request.Context(), tenantID(c), c.Param("id"), 50)
	if err != nil {
		serverErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// actionDecisionInput 确认/拒绝请求体（reason 可选）。
type actionDecisionInput struct {
	Reason string `json:"reason"`
}

// requireOwnerKey 高风险动作确认的保护：apikey 模式仅 owner key 可确认/拒绝
// （noauth 本地模式直通）。低成本实现，不引入完整 scopes（AGENT_CHAT_PLAN §9.1 后续）。
func (s *Server) requireOwnerKey(c *gin.Context) bool {
	if s.cfg.AuthType != "apikey" {
		return true
	}
	_, ok, err := s.authSvc.VerifyOwner(c.Request.Context(), c.GetHeader("X-API-Key"))
	if err != nil || !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅 owner key 可确认/拒绝动作（普通 key 只读）"})
		return false
	}
	return true
}

// handleConfirmAction 确认动作请求并执行（幂等：重复确认返回已处理错误）。
func (s *Server) handleConfirmAction(c *gin.Context) {
	if !s.assistantGate(c) || !s.requireOwnerKey(c) {
		return
	}
	var in actionDecisionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法: " + err.Error()})
		return
	}
	req, err := s.assist.ConfirmAction(c.Request.Context(), tenantID(c), c.Param("id"), "local-noauth", in.Reason)
	if errors.Is(err, assistant.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "动作请求不存在"})
		return
	}
	if errors.Is(err, assistant.ErrActionExpired) || errors.Is(err, assistant.ErrActionNotPending) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if errors.Is(err, assistant.ErrActionExecutorMissing) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		serverErr(c, err)
		return
	}
	c.JSON(http.StatusOK, req) // 执行失败也返回请求（status=failed + 原因），前端展示
}

// handleRejectAction 拒绝动作请求（不执行）。
func (s *Server) handleRejectAction(c *gin.Context) {
	if !s.assistantGate(c) || !s.requireOwnerKey(c) {
		return
	}
	var in actionDecisionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体非法: " + err.Error()})
		return
	}
	req, err := s.assist.RejectAction(c.Request.Context(), tenantID(c), c.Param("id"), "local-noauth", in.Reason)
	if errors.Is(err, assistant.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "动作请求不存在"})
		return
	}
	if errors.Is(err, assistant.ErrActionExpired) || errors.Is(err, assistant.ErrActionNotPending) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		serverErr(c, err)
		return
	}
	c.JSON(http.StatusOK, req)
}

// handleCancelRun 取消 run。
func (s *Server) handleCancelRun(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	err := s.assist.CancelRun(c.Request.Context(), tenantID(c), c.Param("id"))
	if errors.Is(err, assistant.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "run 不存在"})
		return
	}
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusAccepted)
}

// handleRunEvents run 事件 SSE 流（全仓第一个出站 SSE）：
// 先清底层 deadline（全局 WriteTimeout=30s 会杀长连接，绝不放宽全局超时）；
// ?lastSequence=N 时先发 DB 快照（续传），再实时订阅；15s 心跳；run 终态后关闭。
func (s *Server) handleRunEvents(c *gin.Context) {
	if !s.assistantGate(c) {
		return
	}
	ctx := c.Request.Context()
	tenant := tenantID(c)
	runID := c.Param("id")

	// 校验 run 属主（不存在/跨租户 → 404，不泄漏存在性）
	run, _, err := s.assist.GetRun(ctx, tenant, runID)
	if err != nil || run == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run 不存在"})
		return
	}

	lastSeq, _ := strconv.ParseInt(c.Query("lastSequence"), 10, 64)
	broker := s.assistBr
	if broker == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "AI 助手未启用"})
		return
	}

	// 清 deadline（对齐 hub.ServeWS 先例）——SSE 长连接不受全局 WriteTimeout 影响
	rc := http.NewResponseController(c.Writer)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	flush := func() { _ = rc.Flush() }
	flush()

	// 快照：lastSequence 之后的事件（续传不重复）；含 run 终态事件时直接关闭
	snapshot, err := broker.EventsSince(ctx, tenant, runID, lastSeq)
	if err != nil {
		slog.Warn("assistant: sse snapshot", "run", runID, "err", err)
	} else {
		for _, ev := range snapshot {
			if err := writeSSEEvent(c, ev); err != nil {
				return
			}
			lastSeq = ev.Sequence
			if ev.Type == assistant.EvRunCompleted || ev.Type == assistant.EvRunFailed ||
				ev.Type == assistant.EvRunCancelled {
				flush()
				return
			}
		}
	}
	flush()

	// 实时订阅
	ch, unsub := broker.Subscribe(runID)
	defer unsub()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case ev := <-ch:
			if ev.Sequence <= lastSeq {
				continue // 乱序/重复（快照与实时重叠）跳过
			}
			if err := writeSSEEvent(c, ev); err != nil {
				return
			}
			lastSeq = ev.Sequence
			// run 终态事件发出后关闭连接
			if ev.Type == assistant.EvRunCompleted || ev.Type == assistant.EvRunFailed ||
				ev.Type == assistant.EvRunCancelled {
				flush()
				return
			}
			flush()
		case <-heartbeat.C:
			// 心跳保活（前端可依赖其判定连接存活）
			if err := writeSSEEvent(c, assistant.Event{
				RunID: runID, Sequence: lastSeq, Type: assistant.EvHeartbeat,
				Timestamp: time.Now().UTC(),
			}); err != nil {
				return
			}
			flush()
		case <-ctx.Done():
			return // 客户端断开/取消
		}
	}
}

// writeSSEEvent 输出一条 SSE 事件（data: <json>\n\n）。
func writeSSEEvent(c *gin.Context, ev assistant.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = c.Writer.WriteString("data: " + string(b) + "\n\n")
	return err
}

// serverErr 通用 500。
func serverErr(c *gin.Context, err error) {
	slog.Warn("assistant: api", "err", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("internal: %v", err)})
}
