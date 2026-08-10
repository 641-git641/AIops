package api

import (
	"log/slog"
	"time"

	"aiops/internal/logutil"
	"aiops/internal/util"

	"github.com/gin-gonic/gin"
)

// requestlog.go：请求 ID 注入 + HTTP 访问日志（KEEPON 4.2 日志可追溯）。
// 访问日志的 path 经 logutil.RedactURLQuery 脱敏（webhook/WS 查询参数不泄漏）。

// ctxRequestID 请求 ID 的 gin context key。
const ctxRequestID = "requestID"

// RequestIDMiddleware 注入请求 ID：优先沿用调用方 X-Request-Id，否则生成并回写响应头。
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader("X-Request-Id")
		if rid == "" {
			rid = util.CorrelationID()
		}
		c.Set(ctxRequestID, rid)
		c.Writer.Header().Set("X-Request-Id", rid)
		c.Next()
	}
}

// AccessLogMiddleware 请求完成（或 abort）后记录访问日志：method / path（脱敏）/ status /
// duration / tenant / requestID。请求量不大，单行日志即可满足排查。
func AccessLogMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("http",
			"method", c.Request.Method,
			"path", logutil.RedactURLQuery(c.Request.URL.RequestURI()),
			"status", c.Writer.Status(),
			"durationMs", time.Since(start).Milliseconds(),
			"tenant", tenantID(c),
			"requestID", c.GetString(ctxRequestID),
		)
	}
}
