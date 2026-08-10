package logutil

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestSensitiveKey(t *testing.T) {
	hit := []string{"apiKey", "api_key", "ApiKey", "secret", "Authorization", "access_token", "password", "ticket", "privateKey", "x-api-key", "signature", "sig"}
	for _, k := range hit {
		if !SensitiveKey(k) {
			t.Errorf("SensitiveKey(%q) = false, want true", k)
		}
	}
	miss := []string{"apiKeySet", "name", "tenantId", "duration", "status", "error", "level", "time", "msg", "keyspace", "monkey"}
	for _, k := range miss {
		if SensitiveKey(k) {
			t.Errorf("SensitiveKey(%q) = true, want false", k)
		}
	}
}

func TestRedactURLQuery(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://example.com/webhook?access_token=abc123&host=x", "https://example.com/webhook?access_token=[REDACTED]&host=x"},
		{"/api/v1/ws?ticket=deadbeef", "/api/v1/ws?ticket=[REDACTED]"},
		{"/api/v1/alerts?page=1&size=20", "/api/v1/alerts?page=1&size=20"}, // 无敏感参数不动
		{"/api/v1/alerts", "/api/v1/alerts"},                               // 无查询串不动
		{"https://a.b/c?key=zz", "https://a.b/c?key=[REDACTED]"},
		{"http://exa mple.com/x?a=1", "http://exa mple.com/x?[REDACTED]"}, // 解析失败整段打码
	}
	for _, c := range cases {
		if got := RedactURLQuery(c.in); got != c.want {
			t.Errorf("RedactURLQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactValue(t *testing.T) {
	if got := RedactValue("apiKey", "sk-abc"); got != "[REDACTED]" {
		t.Errorf("sensitive key: got %q", got)
	}
	if got := RedactValue("url", "http://x/hook?token=zz"); !strings.Contains(got, "[REDACTED]") {
		t.Errorf("url value should redact query: %q", got)
	}
	if got := RedactValue("host", "node-1"); got != "node-1" {
		t.Errorf("plain value must pass through: %q", got)
	}
}

// TestReplaceAttr 抓包断言：slog 输出中敏感字段被替换、普通字段保留、嵌套 group 递归脱敏。
func TestReplaceAttr(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: RedactAttr})
	logger := slog.New(h)
	logger.Info("test",
		"apiKey", "sk-secret-123",
		"tenantId", "default",
		slog.Group("client",
			"access_token", "tok-999",
			"name", "my-script",
		),
	)
	out := buf.String()
	for _, forbid := range []string{"sk-secret-123", "tok-999"} {
		if strings.Contains(out, forbid) {
			t.Errorf("log leaked %q: %s", forbid, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("no redaction marker: %s", out)
	}
	if !strings.Contains(out, "default") || !strings.Contains(out, "my-script") {
		t.Errorf("non-sensitive fields lost: %s", out)
	}
}
