package rag

import (
	"context"
	"strings"
	"time"
)

// search.go：通用知识检索入口（AGENT_CHAT_PLAN §8.2，对话 Agent search_knowledge 工具的数据源）。
// 用户自由文本查询 → 构造 RetrievalQuery → 三路召回（向量路的查询脱敏由 recallVector 内
// Sanitize fail-closed 检查负责：疑似密钥 → 跳过向量路降级关键词）。
// 与事故驱动检索（BuildQuery→Retrieve）共用同一套召回/融合/预算，不另建检索链路。

// KnowledgeSearchFilters 通用检索过滤条件。
type KnowledgeSearchFilters struct {
	Service string // 按服务过滤（空 = 不限）
	Limit   int    // 返回条数（<=0 表示不裁剪，沿用 MaxContextTokens 预算）
}

// SearchKnowledge 通用知识检索。永不返回 error（对齐 Retrieve fail-open 语义）：
// 空查询返回空包（no_evidence），RAG 关闭时本入口不可用（调用方负责判空服务）。
func (s *Service) SearchKnowledge(ctx context.Context, tenantID, query string, filters KnowledgeSearchFilters) *EvidencePack {
	pack := &EvidencePack{QueryHash: "", LatencyMS: map[string]int64{}}
	if strings.TrimSpace(query) == "" {
		pack.DegradedReason = DegradedNoEvidence
		return pack
	}
	q := RetrievalQuery{
		TenantID:   tenantID,
		Summary:    query,
		Service:    filters.Service,
		OccurredAt: time.Now(),
	}
	pack = s.Retrieve(ctx, q)
	if filters.Limit > 0 && len(pack.Selected) > filters.Limit {
		pack.Selected = pack.Selected[:filters.Limit]
	}
	return pack
}
