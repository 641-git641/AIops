package rag

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// sha256Hex 内容校验和（幂等与 reconciliation 依据）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// labelLineRe 匹配「标签：」独立行（语义字段边界，如「现象：」「确认根因：」）。
// 标签限制 1~16 个非冒号字符，避免正文行误判。
var labelLineRe = regexp.MustCompile(`^([^：\n]{1,16})：$`)
