package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aiops/internal/logutil"

	"github.com/gin-gonic/gin"
)

// captureLogger 捕获 slog 输出（写测试断言用）。
func captureLogger(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: logutil.RedactAttr})
	slog.SetDefault(slog.New(h))
	return &buf, func() { slog.SetDefault(prev) }
}

func TestRequestIDMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 无 X-Request-Id → 生成并回写
	r := gin.New()
	r.Use(RequestIDMiddleware())
	r.GET("/x", func(c *gin.Context) {
		if c.GetString(ctxRequestID) == "" {
			t.Error("requestID not injected")
		}
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if got := w.Header().Get("X-Request-Id"); got == "" {
		t.Error("X-Request-Id not written back")
	}
	// 带 X-Request-Id → 沿用（不生成新的）
	r2 := gin.New()
	r2.Use(RequestIDMiddleware())
	var echoed string
	r2.GET("/x", func(c *gin.Context) { echoed = c.GetString(ctxRequestID); c.Status(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "trace-abc")
	r2.ServeHTTP(httptest.NewRecorder(), req)
	if echoed != "trace-abc" {
		t.Errorf("echoed = %q, want trace-abc", echoed)
	}
}

func TestAccessLogMiddlewareRedacts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	buf, restore := captureLogger(t)
	defer restore()

	r := gin.New()
	r.Use(RequestIDMiddleware(), AccessLogMiddleware())
	r.GET("/ok", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/api/v1/webhook/grafana", func(c *gin.Context) { c.Status(http.StatusAccepted) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ok", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// WS 握手路径带 ticket 查询参数 → 访问日志必须脱敏
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/ws?ticket=supersecret", nil))

	out := buf.String()
	if !strings.Contains(out, `path=/ok`) {
		t.Errorf("access log missing /ok: %s", out)
	}
	if strings.Contains(out, "supersecret") {
		t.Errorf("ticket leaked in access log: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("no redaction marker: %s", out)
	}
}
