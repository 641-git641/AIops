package rag

import (
	"context"
	"log/slog"
	"time"
)

// postgres.go：PostgreSQL 混合召回实现（RAG_PLAN §9.3）。
// 强制过滤在 SQL 层完成：tenant_id、status=active、有效期内、verification_status<>rejected、
// 排除当前 incident。禁止先跨租户搜索再在 Go 内存过滤。

// recallRow 三路共用的结果行映射。
type recallRow struct {
	DocumentID string
	ChunkID    string
	Ordinal    int
	Section    string
	Content    string
	SourceType string
	SourceID   string
	Title      string
	Service    string
	Severity   string
	TrustLevel string
	ValidFrom  time.Time
	Score      float64
}

// baseFilter 强制过滤条件（拼进各召回路；参数顺序：tenantID, incidentID）。
const baseFilter = `
	AND d.status = 'active'
	AND d.verification_status <> 'rejected'
	AND (d.valid_until IS NULL OR d.valid_until > now())
	AND NOT (d.source_type = 'incident' AND d.source_id = ?)`

// recallExact 精确召回：同服务 / 告警名包含 / 错误码包含（metadata @> jsonb 数组包含）。
func (s *Service) recallExact(ctx context.Context, q RetrievalQuery) ([]Evidence, error) {
	if q.Service == "" && len(q.ErrorCodes) == 0 && len(q.AlertNames) == 0 {
		return nil, nil // 无精确条件，跳过该路
	}
	var rows []recallRow
	err := s.db.WithContext(ctx).Raw(`
		SELECT d.id AS document_id, c.id AS chunk_id, c.ordinal, c.section, c.content,
			d.source_type, d.source_id, d.title, COALESCE(d.service, '') AS service,
			COALESCE(d.severity, '') AS severity, d.trust_level, d.valid_from, 0.0 AS score
		FROM knowledge_chunks c
		JOIN knowledge_documents d ON d.id = c.document_id
		WHERE d.tenant_id = ?`+baseFilter+`
		  AND (d.service = ? OR d.metadata @> ?::jsonb)
		ORDER BY d.trust_level DESC, d.valid_from DESC
		LIMIT 10`,
		q.TenantID, q.IncidentID, q.Service, string(q.exactMetaJSON())).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return toEvidence(rows, "exact"), nil
}

// recallKeyword 关键词召回：FTS（simple 配置，主要覆盖英文标识符）+ pg_trgm 相似度
// （中文模糊匹配的主力；中文分词弱是已知限制，多语言向量路补充）。
func (s *Service) recallKeyword(ctx context.Context, q RetrievalQuery) ([]Evidence, error) {
	text := q.QueryText()
	if text == "" {
		return nil, nil
	}
	var rows []recallRow
	err := s.db.WithContext(ctx).Raw(`
		SELECT d.id AS document_id, c.id AS chunk_id, c.ordinal, c.section, c.content,
			d.source_type, d.source_id, d.title, COALESCE(d.service, '') AS service,
			COALESCE(d.severity, '') AS severity, d.trust_level, d.valid_from,
			ts_rank(c.content_tsv, websearch_to_tsquery('simple', ?))
				+ similarity(c.content, ?) AS score
		FROM knowledge_chunks c
		JOIN knowledge_documents d ON d.id = c.document_id
		WHERE d.tenant_id = ?`+baseFilter+`
		  AND (c.content_tsv @@ websearch_to_tsquery('simple', ?) OR similarity(c.content, ?) > 0.2)
		ORDER BY score DESC
		LIMIT ?`,
		text, text, q.TenantID, q.IncidentID, text, text, s.opts.TopKKeyword).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return toEvidence(rows, "keyword"), nil
}

// recallVector 向量召回：cosine 距离（<=>）。embedding 未配置或模型/维度/版本与在线
// 索引不一致时跳过该路并给出降级原因（允许关键词降级，RAG_PLAN §6.3/§13）。
func (s *Service) recallVector(ctx context.Context, q RetrievalQuery) ([]Evidence, error) {
	if s.opts.Emb == nil {
		return nil, &laneSkipped{reason: DegradedVectorDisabled}
	}
	meta := s.indexMeta()
	if meta.ModelID != "" && meta.ModelID != s.opts.Emb.ModelID() {
		slog.Warn("rag: vector lane disabled", "reason", "model mismatch", "online", meta.ModelID, "config", s.opts.Emb.ModelID())
		return nil, &laneSkipped{reason: DegradedModelMismatch}
	}
	if meta.IndexVersion != "" && meta.IndexVersion != s.opts.IndexVersion {
		slog.Warn("rag: vector lane disabled", "reason", "index version mismatch", "online", meta.IndexVersion, "config", s.opts.IndexVersion)
		return nil, &laneSkipped{reason: DegradedIndexVersionMism}
	}
	if meta.Dimension > 0 && s.opts.Emb.Dimension() > 0 && meta.Dimension != s.opts.Emb.Dimension() {
		slog.Warn("rag: vector lane disabled", "reason", "dimension mismatch", "online", meta.Dimension, "config", s.opts.Emb.Dimension())
		return nil, &laneSkipped{reason: DegradedDimensionMismatch}
	}
	// 查询向量化前同样脱敏（索引路径已脱敏，查询摘要可能含告警原文粘贴的敏感信息）；
	// 命中疑似未分类密钥（Sanitize 报错）或触发脱敏 → 跳过向量路降级关键词（fail-closed on query）
	sres, err := Sanitize(q.QueryText())
	if err != nil || len(sres.Redacted) > 0 {
		return nil, &laneSkipped{reason: DegradedQuerySanitize}
	}
	vec, err := s.opts.Emb.EmbedQuery(ctx, sres.Text)
	if err != nil {
		return nil, err // 超时/错误 → 关键词降级
	}
	var rows []recallRow
	err = s.db.WithContext(ctx).Raw(`
		SELECT d.id AS document_id, c.id AS chunk_id, c.ordinal, c.section, c.content,
			d.source_type, d.source_id, d.title, COALESCE(d.service, '') AS service,
			COALESCE(d.severity, '') AS severity, d.trust_level, d.valid_from,
			1 - (e.embedding <=> ?::vector) AS score
		FROM knowledge_embeddings e
		JOIN knowledge_chunks c ON c.id = e.chunk_id
		JOIN knowledge_documents d ON d.id = c.document_id
		WHERE e.tenant_id = ? AND e.model_id = ? AND e.index_version = ?
		  AND d.tenant_id = ?`+baseFilter+`
		ORDER BY score DESC
		LIMIT ?`,
		Vec(vec).String(), q.TenantID, s.opts.Emb.ModelID(), s.opts.IndexVersion,
		q.TenantID, q.IncidentID, s.opts.TopKVector).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return toEvidence(rows, "vector"), nil
}

// laneSkipped 主动跳过的向量路（非错误，不产生 embedding_error 语义）。
type laneSkipped struct{ reason string }

func (e *laneSkipped) Error() string { return "vector lane skipped: " + e.reason }

// toEvidence 行 → 证据（score 后续由融合重算）。
func toEvidence(rows []recallRow, lane string) []Evidence {
	out := make([]Evidence, 0, len(rows))
	for _, r := range rows {
		out = append(out, Evidence{
			DocumentID: r.DocumentID,
			ChunkID:    r.ChunkID,
			SourceType: r.SourceType,
			SourceID:   r.SourceID,
			Title:      r.Title,
			Section:    r.Section,
			Content:    r.Content,
			Service:    r.Service,
			Severity:   r.Severity,
			TrustLevel: r.TrustLevel,
			Time:       r.ValidFrom,
			Score:      r.Score,
			Reasons:    []string{lane},
		})
	}
	return out
}
