// Package embedding 提供向量化 Provider（RAG 索引/检索用）。
// 与 Chat LLM 客户端（internal/ai/llm.go）分开配置、分开超时、分开限流：
// Embedding 与 Chat 生命周期不同，接口隔离便于测试与替换（RAG_PLAN §11.2）。
package embedding

import "context"

// Provider 向量化接口。维度与模型 ID 是索引契约：换模型必须新建索引版本重算，
// 禁止不同模型向量混入同一 ANN 索引（RAG_PLAN §6.3）。
type Provider interface {
	// EmbedDocuments 批量向量化（调用方按 batch 分片传入；返回与输入一一对应）。
	EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error)
	// EmbedQuery 单条查询向量化（RCA 查询只发单条，不缓存）。
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	// ModelID 模型标识（写入 knowledge_embeddings.model_id 与检索一致性检查）。
	ModelID() string
	// Dimension 返回向量维度；0 表示未知（openai-compat 首次响应后学习）。
	Dimension() int
}
