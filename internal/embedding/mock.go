package embedding

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
)

// Mock 确定性 mock Provider（仅演示/CI：同文本同向量，检索质量不可用）。
// 向量 = 归一化后的文本 n-gram 哈希投影，用于端到端演练与单元测试。
type Mock struct {
	dim int
}

// NewMock 创建确定性 mock（dim 为向量维度）。
func NewMock(dim int) *Mock { return &Mock{dim: dim} }

// EmbedDocuments 批量确定性向量。
func (m *Mock) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, t := range texts {
		out = append(out, m.vector(t))
	}
	return out, nil
}

// EmbedQuery 单条确定性向量。
func (m *Mock) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return m.vector(text), nil
}

// ModelID 固定标识（索引契约的一部分）。
func (m *Mock) ModelID() string { return "mock-local" }

// Dimension 返回维度。
func (m *Mock) Dimension() int { return m.dim }

// vector 文本 → 归一化向量：对每个 4-gram 取 fnv 哈希投影累加。
func (m *Mock) vector(text string) []float32 {
	v := make([]float32, m.dim)
	runes := []rune(strings.ToLower(text))
	for i := 0; i+3 < len(runes); i++ {
		h := fnv.New32a()
		_, _ = h.Write([]byte(string(runes[i : i+4])))
		idx := int(h.Sum32() % uint32(m.dim))
		v[idx] += 1.0
	}
	norm := 0.0
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	if norm > 0 {
		inv := float32(1.0 / math.Sqrt(norm))
		for i := range v {
			v[i] *= inv
		}
	}
	return v
}
