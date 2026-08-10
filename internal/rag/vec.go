package rag

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Vec 向量（pgvector 的 Go 表示）。数据库列类型为 vector(D)（EnsureSchema 裸 SQL 添加），
// 读写均经 ::vector / ::text 显式转换；本类型提供 Scan 供只读 SQL 直接映射。
type Vec []float32

// String 渲染为 pgvector 文本格式 "[1,2,3]"（Valuer 返回值，配合 SQL 中 ?::vector 使用）。
func (v Vec) String() string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(float64(x), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// Scan 实现 sql.Scanner：接受 "[1,2,3]" 文本（pgx 读 vector 列返回 []byte）。
func (v *Vec) Scan(src any) error {
	switch s := src.(type) {
	case nil:
		*v = nil
		return nil
	case string:
		return v.parse(s)
	case []byte:
		return v.parse(string(s))
	default:
		return fmt.Errorf("rag: cannot scan %T into Vec", src)
	}
}

func (v *Vec) parse(s string) error {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return errors.New("rag: invalid vector literal")
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		*v = Vec{}
		return nil
	}
	parts := strings.Split(inner, ",")
	out := make(Vec, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return fmt.Errorf("rag: invalid vector element %q: %w", p, err)
		}
		out = append(out, float32(f))
	}
	*v = out
	return nil
}

// Value 实现 driver.Valuer：返回 pgvector 文本（调用方 SQL 需拼 ::vector 转换）。
func (v Vec) Value() (driver.Value, error) { return v.String(), nil }

// Dim 返回维度。
func (v Vec) Dim() int { return len(v) }
