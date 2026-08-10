// Package logutil 提供日志脱敏纯函数：slog ReplaceAttr 全局脱敏 + URL 查询参数遮蔽。
// 脱敏是安全底线（KEEPON 4.2）：API Key、模型密钥、Authorization、webhook 查询参数
// 等敏感字段永不进入日志（无论输出到 stdout 还是文件）。
package logutil

import (
	"net/url"
	"strings"

	"log/slog"
)

// sensitiveKeys 敏感属性 key 黑名单（匹配前先去掉 -_ 并小写，命中即值替换 [REDACTED]）。
// "key" 单独作词条过于宽泛，这里用具体组合词；URL 查询参数另有独立清单。
var sensitiveKeys = []string{
	"apikey", "secret", "password", "passwd", "pwd", "token", "authorization",
	"signature", "sig", "privatekey", "accesskey", "ticket",
}

// SensitiveKey 判断日志属性 key 是否敏感（规范化后与词表精确匹配或以其结尾）。
func SensitiveKey(s string) bool {
	norm := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "-", ""), "_", ""))
	for _, k := range sensitiveKeys {
		if norm == k || strings.HasSuffix(norm, k) {
			return true
		}
	}
	return false
}

// sensitiveQueryParams URL 查询参数脱敏清单（ticket 在内：WS ticket 不允许入日志）。
var sensitiveQueryParams = []string{
	"token", "access_token", "api_key", "apikey", "key", "secret", "signature",
	"sig", "password", "passwd", "pwd", "code", "auth", "authorization", "ticket",
}

// RedactURLQuery 遮蔽 URL 查询参数中的敏感值；解析失败（非法 URL）时整段查询打码。
// 输入可以是完整 URL（https://...?a=1）或相对路径（/api/v1/ws?ticket=xxx）。
func RedactURLQuery(raw string) string {
	idx := strings.Index(raw, "?")
	if idx < 0 {
		return raw // 无查询串，无需处理
	}
	u, err := url.Parse(raw)
	if err != nil {
		// 解析失败：整段查询打码，宁可误伤不可泄漏
		return raw[:idx] + "?[REDACTED]"
	}
	if u.RawQuery == "" {
		return raw
	}
	// 手动按 & 拆分替换（保持原顺序、敏感值用字面 [REDACTED]，不经 Encode 避免 %5B 转义）
	parts := strings.Split(u.RawQuery, "&")
	changed := false
	for i, p := range parts {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		name, err := url.QueryUnescape(kv[0])
		if err != nil {
			continue
		}
		for _, s := range sensitiveQueryParams {
			if strings.ToLower(name) == s {
				parts[i] = kv[0] + "=[REDACTED]"
				changed = true
				break
			}
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = strings.Join(parts, "&")
	return u.String()
}

// RedactValue 按属性 key 脱敏值：key 敏感 → 直接替换；值形如带查询串的 URL → 遮蔽查询参数。
func RedactValue(key, v string) string {
	if SensitiveKey(key) {
		return "[REDACTED]"
	}
	if strings.Contains(v, "?") {
		return RedactURLQuery(v)
	}
	return v
}

// RedactAttr slog HandlerOptions.ReplaceAttr 适配器：递归处理嵌套 group。
func RedactAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		children := a.Value.Group()
		args := make([]any, len(children))
		for i := range children {
			children[i] = RedactAttr(nil, children[i])
			args[i] = children[i]
		}
		return slog.Group(a.Key, args...)
	}
	if a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, RedactValue(a.Key, a.Value.String()))
	}
	return a
}
