// Package util 提供通用工具函数：ID 生成、哈希、时间。
package util

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/google/uuid"
)

// NewID 生成 UUIDv7（时间有序，对 SQLite/Postgres 索引友好）。
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil { // 兜底：库版本不支持 v7 时回退 v4
		return uuid.NewString()
	}
	return id.String()
}

// CorrelationID 生成一次故障处理链路的追踪 ID。
func CorrelationID() string {
	return uuid.NewString()
}

// SHA256 返回字符串的 sha256 十六进制摘要（用于 API key 哈希、fingerprint 计算）。
func SHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// HashEvent 对事件关键字段做稳定哈希，用于 alert_hash（物理去重键）。
func HashEvent(parts ...any) string {
	h := sha256.New()
	for _, p := range parts {
		var b []byte
		switch v := p.(type) {
		case string:
			b = []byte(v)
		case []byte:
			b = v
		default:
			b, _ = json.Marshal(v)
		}
		h.Write(b)
		h.Write([]byte{0xff})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NowMillis 返回当前毫秒时间戳（REST 响应常用）。
func NowMillis() int64 {
	return timeNow().UnixMilli()
}
