package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"aiops/internal/util"

	"gorm.io/datatypes"
)

// eval.go：检索 golden set 评测（KEEPON M3 / RAG_PLAN §17.1）。
// 个人真实查询组成 golden set，每条记录期望命中的 source（incident/runbook/playbook id）；
// RunEval 与线上完全同链路（RetrievalQuery + Retrieve），输出 Recall@K 等指标——
// 调整 TopK/权重/融合策略前后跑同一评测集即可对比（cmd/rag-eval）。

// GoldenQuery 检索评测查询。
type GoldenQuery struct {
	ID              string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID        string         `gorm:"size:64;index" json:"tenantId"`
	Query           string         `gorm:"type:text" json:"query"` // 真实查询文本（非合成）
	Service         string         `gorm:"size:64" json:"service,omitempty"`
	Severity        string         `gorm:"size:16" json:"severity,omitempty"`
	ExpectedSources datatypes.JSON `gorm:"type:jsonb" json:"expectedSources"` // source_id 数组（eval 时映射 active 文档）
	Tags            string         `gorm:"size:255" json:"tags,omitempty"`    // 分类（connection-pool/runbook/...）
	Enabled         bool           `gorm:"index" json:"enabled"`
	Useful          *bool          `json:"useful,omitempty"` // 人工 useful 反馈（null=未评估）
	Note            string         `gorm:"type:text" json:"note,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
}

// TableName 表名。
func (GoldenQuery) TableName() string { return "rag_golden_queries" }

// CreateGoldenQuery 创建评测查询（校验：query 非空、expected 至少 1 项且为真实 source）。
func (s *Service) CreateGoldenQuery(ctx context.Context, tenantID string, g *GoldenQuery) error {
	if err := validateGolden(g); err != nil {
		return err
	}
	g.ID = util.NewID()
	g.TenantID = tenantID
	g.CreatedAt, g.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	if err := s.verifySourcesExist(ctx, tenantID, g.ExpectedSources); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Create(g).Error
}

// UpdateGoldenQuery 编辑/开关评测查询。
func (s *Service) UpdateGoldenQuery(ctx context.Context, tenantID, id string, g *GoldenQuery) error {
	if err := validateGolden(g); err != nil {
		return err
	}
	if err := s.verifySourcesExist(ctx, tenantID, g.ExpectedSources); err != nil {
		return err
	}
	updates := map[string]any{
		"query": g.Query, "service": g.Service, "severity": g.Severity,
		"expected_sources": g.ExpectedSources, "tags": g.Tags, "enabled": g.Enabled,
		"useful": g.Useful, "note": g.Note, "updated_at": time.Now().UTC(),
	}
	res := s.db.WithContext(ctx).Model(&GoldenQuery{}).
		Where("tenant_id = ? AND id = ?", tenantID, id).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("golden query not found")
	}
	return nil
}

// DeleteGoldenQuery 删除评测查询。
func (s *Service) DeleteGoldenQuery(ctx context.Context, tenantID, id string) error {
	return s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&GoldenQuery{}).Error
}

// ListGoldenQueries 评测查询列表（最新在前；enabledOnly 时只返回启用项）。
func (s *Service) ListGoldenQueries(ctx context.Context, tenantID string, enabledOnly bool) ([]GoldenQuery, error) {
	q := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if enabledOnly {
		q = q.Where("enabled = true")
	}
	var items []GoldenQuery
	err := q.Order("created_at DESC").Find(&items).Error
	return items, err
}

func validateGolden(g *GoldenQuery) error {
	if g.Query == "" {
		return errors.New("golden: query 必填")
	}
	var srcs []string
	if err := json.Unmarshal(g.ExpectedSources, &srcs); err != nil || len(srcs) == 0 {
		return errors.New("golden: expectedSources 至少 1 个 source_id")
	}
	return nil
}

// verifySourcesExist expected_sources 每项必须是该租户真实存在的 source（incident/runbook/playbook）。
func (s *Service) verifySourcesExist(ctx context.Context, tenantID string, expected datatypes.JSON) error {
	var srcs []string
	if err := json.Unmarshal(expected, &srcs); err != nil {
		return err
	}
	var n int64
	if err := s.db.WithContext(ctx).Model(&KnowledgeDocument{}).
		Where("tenant_id = ? AND source_id IN ?", tenantID, srcs).Distinct("source_id").Count(&n).Error; err != nil {
		return err
	}
	if int(n) != len(srcs) {
		return fmt.Errorf("golden: expectedSources 包含不存在的 source_id（当前租户 %s 可命中 %d/%d）", tenantID, n, len(srcs))
	}
	return nil
}

// ---- 评测执行 ----

// EvalOptions 评测参数（覆盖线上默认：--topk-* 调参对比）。
type EvalOptions struct {
	Tenants          []string // 空 = 全部租户
	EnabledOnly      bool
	TopKKeyword      int
	TopKVector       int
	TopKFinal        int
	MaxContextTokens int
}

// EvalQueryResult 单条查询评测结果。
type EvalQueryResult struct {
	Query       string   `json:"query"`
	Tenant      string   `json:"tenant"`
	Expected    []string `json:"expected"`    // source_id
	HitDocs     []string `json:"hitDocs"`     // Candidates 命中 document id
	SelectedHit []string `json:"selectedHit"` // Selected 命中
	FirstHit    bool     `json:"firstHit"`    // Selected[0] ∈ expected
	Empty       bool     `json:"empty"`
	Degraded    string   `json:"degraded,omitempty"`
	LatencyMS   int64    `json:"latencyMs"`
}

// EvalMetrics 评测汇总（调参前后 -baseline diff 用）。
type EvalMetrics struct {
	Total              int               `json:"total"`
	Tenants            int               `json:"tenants"`
	AnyHitRate         float64           `json:"anyHitRate"` // Candidates 口径（主）
	MeanRecall         float64           `json:"meanRecall"`
	SelectedAnyHitRate float64           `json:"selectedAnyHitRate"` // Selected 口径（次）
	SelectedFirstHit   float64           `json:"selectedFirstHit"`   // 首条命中率（空结果计 miss）
	FirstHitNonEmpty   float64           `json:"firstHitNonEmpty"`   // 非空查询首条命中率
	EmptyRate          float64           `json:"emptyRate"`
	AvgLatencyMS       float64           `json:"avgLatencyMs"`
	P95LatencyMS       float64           `json:"p95LatencyMs"`
	UsefulCoverage     float64           `json:"usefulCoverage"` // 已人工 useful 反馈的查询占比
	Degraded           map[string]int    `json:"degraded"`
	PerQuery           []EvalQueryResult `json:"perQuery"`
	Params             EvalParams        `json:"params"`
}

// EvalParams 本次参数快照（对比留痕）。
type EvalParams struct {
	TopKKeyword      int   `json:"topKKeyword"`
	TopKVector       int   `json:"topKVector"`
	TopKFinal        int   `json:"topKFinal"`
	MaxContextTokens int   `json:"maxContextTokens"`
	RunAt            int64 `json:"runAt"`
}

// RunEval 加载 golden set → 逐条与线上同链路检索 → 汇总指标。
// 永不返回检索 error（Retrieve fail-open）；golden 为空返回空指标。
func (s *Service) RunEval(ctx context.Context, opts EvalOptions) (*EvalMetrics, error) {
	m := &EvalMetrics{Degraded: map[string]int{}}
	m.Params = EvalParams{
		TopKKeyword:      opts.TopKKeyword,
		TopKVector:       opts.TopKVector,
		TopKFinal:        opts.TopKFinal,
		MaxContextTokens: opts.MaxContextTokens,
		RunAt:            time.Now().UTC().Unix(),
	}
	queries, err := s.loadGolden(ctx, opts.Tenants, opts.EnabledOnly)
	if err != nil {
		return nil, err
	}
	tenantSet := map[string]bool{}
	var latencies []int64
	var usefulN, usefulFilled int
	hitN, selHitN, firstHitN, firstHitNonEmptyN, nonEmptyN, emptyN := 0, 0, 0, 0, 0, 0
	var recallSum float64

	for _, g := range queries {
		expected := s.expectedDocIDs(ctx, g)
		if g.Useful != nil {
			usefulFilled++
			if *g.Useful {
				usefulN++
			}
		}
		// 与线上完全同链路（IncidentID 留空：评测无"自身"概念）
		rq := RetrievalQuery{TenantID: g.TenantID, Summary: g.Query, Service: g.Service, Severity: g.Severity}
		pack := s.Retrieve(ctx, rq)

		r := EvalQueryResult{
			Query: g.Query, Tenant: g.TenantID, Expected: expected,
			Degraded: pack.DegradedReason, LatencyMS: totalLatency(pack.LatencyMS),
		}
		expSet := toSet(expected)
		r.HitDocs = docHits(pack.Candidates, expSet)
		r.SelectedHit = docHits(pack.Selected, expSet)
		r.Empty = len(pack.Selected) == 0
		if len(r.HitDocs) > 0 {
			hitN++
		}
		if len(r.SelectedHit) > 0 {
			selHitN++
		}
		if len(expected) > 0 {
			recallSum += float64(len(r.HitDocs)) / float64(len(expected))
		}
		if !r.Empty {
			nonEmptyN++
			first := docID(pack.Selected[0])
			hit := expSet[first]
			r.FirstHit = hit
			if hit {
				firstHitNonEmptyN++
			}
		} else {
			emptyN++
		}
		if r.FirstHit {
			firstHitN++
		}
		if pack.DegradedReason != "" {
			m.Degraded[pack.DegradedReason]++
		}
		latencies = append(latencies, r.LatencyMS)
		m.PerQuery = append(m.PerQuery, r)
		tenantSet[g.TenantID] = true
	}

	n := len(queries)
	m.Total = n
	m.Tenants = len(tenantSet)
	if n > 0 {
		m.AnyHitRate = float64(hitN) / float64(n)
		m.MeanRecall = recallSum / float64(n)
		m.SelectedAnyHitRate = float64(selHitN) / float64(n)
		m.SelectedFirstHit = float64(firstHitN) / float64(n)
		if nonEmptyN > 0 {
			m.FirstHitNonEmpty = float64(firstHitNonEmptyN) / float64(nonEmptyN)
		}
		m.EmptyRate = float64(emptyN) / float64(n)
		m.UsefulCoverage = float64(usefulFilled) / float64(n)
		m.AvgLatencyMS = avg(latencies)
		m.P95LatencyMS = p95(latencies)
	}
	return m, nil
}

// loadGolden 加载 golden set（租户过滤 + 可选仅启用）。
func (s *Service) loadGolden(ctx context.Context, tenants []string, enabledOnly bool) ([]GoldenQuery, error) {
	q := s.db.WithContext(ctx)
	if len(tenants) > 0 {
		q = q.Where("tenant_id IN ?", tenants)
	}
	if enabledOnly {
		q = q.Where("enabled = true")
	}
	var items []GoldenQuery
	if err := q.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// expectedDocIDs 把 expected source_id 映射为当前 active 文档 id 集合
// （源更新后评测自动命中新版本——RAG_PLAN §17.1 版本化语义）。
func (s *Service) expectedDocIDs(ctx context.Context, g GoldenQuery) []string {
	var srcs []string
	if err := json.Unmarshal(g.ExpectedSources, &srcs); err != nil || len(srcs) == 0 {
		return nil
	}
	var ids []string
	if err := s.db.WithContext(ctx).Model(&KnowledgeDocument{}).
		Where("tenant_id = ? AND status = ? AND source_id IN ?", g.TenantID, DocActive, srcs).
		Pluck("id", &ids).Error; err != nil {
		return nil
	}
	return ids
}

func totalLatency(ms map[string]int64) int64 {
	var total int64
	for _, v := range ms {
		total += v
	}
	return total
}

func docID(e Evidence) string { return e.DocumentID }

// docHits 候选证据命中期望文档集合的去重 document id（Candidates 主口径）。
func docHits(evs []Evidence, expSet map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range evs {
		if expSet[e.DocumentID] && !seen[e.DocumentID] {
			seen[e.DocumentID] = true
			out = append(out, e.DocumentID)
		}
	}
	return out
}

func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func avg(v []int64) float64 {
	if len(v) == 0 {
		return 0
	}
	var sum int64
	for _, x := range v {
		sum += x
	}
	return float64(sum) / float64(len(v))
}

func p95(v []int64) float64 {
	if len(v) == 0 {
		return 0
	}
	sorted := append([]int64(nil), v...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)) * 0.95)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return float64(sorted[idx])
}
