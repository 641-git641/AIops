package rag

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"aiops/internal/util"
)

// retriever.go：检索入口（RAG_PLAN §9）。三路并行召回（精确/关键词/向量）→ RRF 融合 →
// 业务加权 → 去重 → 上下文预算 → EvidencePack。任何一路失败只降级该路（fail-open），
// 绝不返回 error 阻塞调用方；总超时由 RetrievalTimeout 约束。

// 降级原因（指标 label 固定枚举，不掺 tenant/incident 等高基数）。
const (
	DegradedNone              = ""
	DegradedNoEvidence        = "no_evidence"
	DegradedEmbeddingTimeout  = "embedding_timeout"
	DegradedEmbeddingError    = "embedding_error"
	DegradedVectorLaneError   = "vector_lane_error"
	DegradedKeywordLaneError  = "keyword_lane_error"
	DegradedExactLaneError    = "exact_lane_error"
	DegradedAllLanesFailed    = "all_lanes_failed"
	DegradedVectorDisabled    = "vector_disabled"
	DegradedModelMismatch     = "model_mismatch"
	DegradedIndexVersionMism  = "index_version_mismatch"
	DegradedDimensionMismatch = "dimension_mismatch"
	DegradedQuerySanitize     = "query_sanitize_failed" // 查询文本触发脱敏/疑似密钥 → 跳过向量路
)

// Evidence 单条证据（带完整来源信息：document/chunk id、标题、时间、可信等级）。
type Evidence struct {
	DocumentID string    `json:"documentId"`
	ChunkID    string    `json:"chunkId"`
	SourceType string    `json:"sourceType"` // incident|playbook|...
	SourceID   string    `json:"sourceId"`
	Title      string    `json:"title"`
	Section    string    `json:"section"`
	Content    string    `json:"content"`
	Service    string    `json:"service,omitempty"`
	Severity   string    `json:"severity,omitempty"`
	TrustLevel string    `json:"trustLevel"`
	Time       time.Time `json:"time"`
	Score      float64   `json:"score"`   // 融合后分数（展示用）
	Reasons    []string  `json:"reasons"` // 命中原因：exact:service / keyword / vector:0.82 / same_service / trust:T3 / verified / recent
}

// EvidencePack 检索结果包（有类型而非拼好的 Prompt 字符串：便于测试、审计、换模型）。
type EvidencePack struct {
	Candidates     []Evidence       `json:"candidates"` // 融合前候选（trace 审计）
	Selected       []Evidence       `json:"selected"`   // 预算裁剪后的最终证据
	DegradedReason string           `json:"degradedReason,omitempty"`
	LatencyMS      map[string]int64 `json:"latencyMs"` // exact/keyword/vector/fusion
	QueryHash      string           `json:"queryHash"`
}

// laneResult 单路召回结果。
type laneResult struct {
	name string
	ev   []Evidence
	err  error
	ms   int64
}

// Retrieve 执行混合检索。永不返回 error：全部失败 → 空包 + all_lanes_failed。
func (s *Service) Retrieve(ctx context.Context, q RetrievalQuery) *EvidencePack {
	pack := &EvidencePack{QueryHash: q.Hash(), LatencyMS: map[string]int64{}}
	start := time.Now()
	defer func() {
		if s.m != nil {
			s.m.Observe("aiops_rag_retrieval_duration_seconds", time.Since(start).Seconds())
			result := "success"
			if len(pack.Selected) == 0 {
				result = "empty"
			} else if pack.DegradedReason != "" {
				result = "degraded"
			}
			s.m.Inc("aiops_rag_retrieval_total", result, pack.DegradedReason)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, s.opts.RetrievalTimeout)
	defer cancel()

	// 三路并行（向量路依赖 embedding，单独判定可用性）
	results := make(chan laneResult, 3)
	var wg sync.WaitGroup
	run := func(name string, fn func(ctx context.Context) ([]Evidence, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			ev, err := fn(ctx)
			results <- laneResult{name: name, ev: ev, err: err, ms: time.Since(start).Milliseconds()}
		}()
	}
	run("exact", func(ctx context.Context) ([]Evidence, error) { return s.recallExact(ctx, q) })
	run("keyword", func(ctx context.Context) ([]Evidence, error) { return s.recallKeyword(ctx, q) })
	run("vector", func(ctx context.Context) ([]Evidence, error) { return s.recallVector(ctx, q) })
	wg.Wait()
	close(results)

	var skipped *laneSkipped
	lanes := map[string][]Evidence{}
	for r := range results {
		pack.LatencyMS[r.name] = r.ms
		if s.m != nil {
			s.m.Observe("aiops_rag_lane_"+r.name+"_duration_seconds", float64(r.ms)/1000.0)
		}
		switch {
		case r.err == nil && len(r.ev) > 0:
			lanes[r.name] = r.ev
			slog.Debug("rag: lane ok", "lane", r.name, "hits", len(r.ev))
		case errors.Is(r.err, context.DeadlineExceeded), errors.Is(r.err, context.Canceled):
			if r.name == "vector" {
				pack.DegradedReason = DegradedEmbeddingTimeout
			} else {
				pack.DegradedReason = degradedForLane(r.name)
			}
		case errors.As(r.err, &skipped):
			pack.DegradedReason = skipped.reason // 模型/维度/版本不匹配等主动跳过
		case r.err != nil && r.name == "vector":
			pack.DegradedReason = DegradedEmbeddingError
		case r.err != nil:
			pack.DegradedReason = degradedForLane(r.name)
		}
	}

	fusionStart := time.Now()
	if len(lanes) == 0 {
		if pack.DegradedReason == "" {
			pack.DegradedReason = DegradedAllLanesFailed
		}
		pack.LatencyMS["fusion"] = 0
		return pack
	}
	all := fuse(lanes, q, s.opts.TopKFinal, 2)
	pack.Candidates = all
	pack.Selected = selectWithBudget(all, s.opts.MaxContextTokens)
	if len(pack.Selected) == 0 {
		pack.DegradedReason = DegradedNoEvidence
	}
	pack.LatencyMS["fusion"] = time.Since(fusionStart).Milliseconds()
	return pack
}

// degradedForLane 精确/关键词路失败对应的降级原因。
func degradedForLane(name string) string {
	if name == "exact" {
		return DegradedExactLaneError
	}
	return DegradedKeywordLaneError
}

// RecordTrace 落检索轨迹（审计/调试；候选与入选均存 chunk id）。
func (s *Service) RecordTrace(ctx context.Context, tenantID, incidentID string, pack *EvidencePack) (string, error) {
	candIDs, _ := json.Marshal(idsOf(pack.Candidates))
	selIDs, _ := json.Marshal(idsOf(pack.Selected))
	lat, _ := json.Marshal(pack.LatencyMS)
	t := &RAGTrace{
		ID:               util.NewID(),
		TenantID:         tenantID,
		IncidentID:       incidentID,
		QueryHash:        pack.QueryHash,
		RetrievalVersion: "rrf-v1",
		IndexVersion:     s.opts.IndexVersion,
		CandidateIDs:     candIDs,
		SelectedIDs:      selIDs,
		LatencyMS:        lat,
		DegradedReason:   pack.DegradedReason,
		CreatedAt:        time.Now().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(t).Error; err != nil {
		return "", err
	}
	return t.ID, nil
}

func idsOf(ev []Evidence) []string {
	out := make([]string, 0, len(ev))
	for _, e := range ev {
		out = append(out, e.ChunkID)
	}
	return out
}
