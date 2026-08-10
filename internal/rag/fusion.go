package rag

import (
	"sort"
	"time"
)

// fusion.go：多路召回融合（RAG_PLAN §9.4）。
// RRF（Reciprocal Rank Fusion, k=60）合并关键词与向量排名 → 业务加权（同服务/T3/验证成功/
// 近期）→ 同文档相邻 chunk 去重 → 来源多样性 → topK。权重与参数记录在 retrieval_version
// 常量中，任何调整都应同步修改 RAGTrace.RetrievalVersion。

const (
	rrfK            = 60.0
	maxChunksPerDoc = 2 // 同一文档最多入选 chunk 数（避免上下文被单一来源占满）
	recentWindow    = 90 * 24 * time.Hour
)

// fuse 融合多路候选：RRF 分数 + 业务加权 + 去重 + 多样性 → topK。
func fuse(lanes map[string][]Evidence, q RetrievalQuery, topK, maxPerDoc int) []Evidence {
	if maxPerDoc <= 0 {
		maxPerDoc = maxChunksPerDoc
	}
	// 1) RRF：Σ 1/(k+rank)，rank 从 1 起
	score := map[string]float64{} // key: chunkID
	reasonOf := map[string][]string{}
	for _, lane := range lanes {
		for i, e := range lane {
			key := e.ChunkID
			score[key] += 1.0 / (rrfK + float64(i+1))
			reasonOf[key] = append(reasonOf[key], e.Reasons...)
		}
	}
	// 2) 业务加权（记录理由，权重变更需同步 retrieval_version）
	byID := map[string]Evidence{}
	for _, lane := range lanes {
		for _, e := range lane {
			byID[e.ChunkID] = e
		}
	}
	for id, e := range byID {
		if e.Service != "" && q.Service != "" && e.Service == q.Service {
			score[id] += 0.15
			reasonOf[id] = append(reasonOf[id], "same_service")
		}
		switch e.TrustLevel {
		case TrustT3:
			score[id] += 0.10
			reasonOf[id] = append(reasonOf[id], "trust:T3")
		case TrustT2:
			score[id] += 0.05
			reasonOf[id] = append(reasonOf[id], "trust:T2")
		}
		if e.SourceType == SourceIncident && time.Since(e.Time) <= recentWindow {
			score[id] += 0.05
			reasonOf[id] = append(reasonOf[id], "recent")
		}
	}
	// 3) 按融合分排序
	order := make([]string, 0, len(score))
	for id := range score {
		order = append(order, id)
	}
	sort.SliceStable(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })
	// 4) 同文档去重（maxPerDoc）+ 多样性（至少保留一类规范来源，如剧本）
	perDoc := map[string]int{}
	var selected []Evidence
	var playbookIdx = -1
	for _, id := range order {
		e := byID[id]
		if perDoc[e.DocumentID] >= maxPerDoc {
			continue
		}
		perDoc[e.DocumentID]++
		e.Score = score[id]
		e.Reasons = dedupStrings(reasonOf[id])
		selected = append(selected, e)
		if e.SourceType == SourcePlaybook {
			playbookIdx = len(selected) - 1
		}
	}
	// 多样性：全部是事故且候选里存在剧本时，用最低分剧本替换最低分事故
	if playbookIdx == -1 && len(selected) > 1 {
		var playbookCands []Evidence
		for _, lane := range lanes {
			for _, e := range lane {
				if e.SourceType == SourcePlaybook {
					playbookCands = append(playbookCands, e)
				}
			}
		}
		if len(playbookCands) > 0 {
			// 取候选剧本中融合分最高的一条（重算其 RRF）
			best, bestScore := playbookCands[0], -1.0
			for _, p := range playbookCands {
				if sc := score[p.ChunkID]; sc > bestScore {
					best, bestScore = p, sc
				}
			}
			best.Score = bestScore
			best.Reasons = dedupStrings(reasonOf[best.ChunkID])
			selected[len(selected)-1] = best // 替换最低分事故
		}
	}
	if topK > 0 && len(selected) > topK {
		selected = selected[:topK]
	}
	return selected
}

// selectWithBudget 上下文预算（RAG_PLAN §9.5）：总 token 上限 + 单文档 40% 上限；
// 超预算优先保留高可信、同服务、带确认结果的证据。
func selectWithBudget(ev []Evidence, maxTokens int) []Evidence {
	if maxTokens <= 0 || len(ev) == 0 {
		return ev
	}
	perDocLimit := maxTokens * 40 / 100
	used := map[string]int{}
	docTokens := map[string]int{}
	total := 0
	var out []Evidence
	for _, e := range ev {
		if used[e.DocumentID] > 0 && used[e.DocumentID] >= maxChunksPerDoc {
			continue
		}
		toks := EstimateTokens(e.Content)
		if total+toks > maxTokens {
			continue
		}
		if docTokens[e.DocumentID]+toks > perDocLimit {
			continue
		}
		used[e.DocumentID]++
		docTokens[e.DocumentID] += toks
		total += toks
		out = append(out, e)
	}
	return out
}
