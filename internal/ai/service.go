package ai

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"aiops/internal/alert"
	"aiops/internal/incident"
	"aiops/internal/util"

	"gorm.io/gorm"
)

// RunResult 一次聚类的执行结果。
type RunResult struct {
	Scanned  int                  `json:"scanned"`  // 扫描的未归属 firing 告警数
	Clusters int                  `json:"clusters"` // 达到阈值的确认簇数
	Created  []*incident.Incident `json:"created"`  // 本次新建的 incident
	Merged   int                  `json:"merged"`   // 合并进既有 incident 的簇数
	Skipped  int                  `json:"skipped"`  // 低于阈值未确认的簇数
}

// Service AI 聚类服务：扫描近期未归属告警 → 聚类 → 达到阈值确认生成 Incident。
type Service struct {
	db         *gorm.DB
	inc        *incident.Service
	threshold  int
	window     time.Duration // 聚类时间窗口（同时作为扫描回溯窗口与组内断开间隔）
	clusterLLM ClusterLLM    // 可选：配置 LLM 后聚类摘要用 LLM 生成（非确定性模板），nil 时确定性 Summarize
	mu         sync.Mutex    // 串行化 RunOnce：定时 worker 与手动 POST /ai/cluster 并发时避免重复建事故
}

// NewService 创建聚类服务。
func NewService(db *gorm.DB, inc *incident.Service, threshold int, window time.Duration) *Service {
	if threshold < 1 {
		threshold = 3
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	return &Service{db: db, inc: inc, threshold: threshold, window: window}
}

// SetClusterLLM 注入聚类摘要增强器（可选）：配置 LLM 后聚类事故摘要改为 LLM 生成（失败保持确定性）。
func (s *Service) SetClusterLLM(llm ClusterLLM) { s.clusterLLM = llm }

// RunOnce 对某租户执行一次聚类：返回新建/合并结果。
func (s *Service) RunOnce(ctx context.Context, tenantID string) (*RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &RunResult{}
	items, err := s.recentFiringUnattached(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	res.Scanned = len(items)
	if len(items) == 0 {
		return res, nil
	}

	alerts := make([]alert.Alert, len(items))
	for i := range items {
		alerts[i] = items[i]
	}
	clusters := ClusterAlerts(alerts, s.window)
	for i := range clusters {
		c := &clusters[i]
		if c.Count < s.threshold {
			res.Skipped++
			continue
		}
		if err := s.confirm(ctx, tenantID, c, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// RunAll 扫描全部租户并执行一次聚类（定时 worker 用）。
func (s *Service) RunAll(ctx context.Context) []RunResult {
	var tenants []string
	if err := s.db.WithContext(ctx).Model(&alert.Alert{}).Distinct().Pluck("tenant_id", &tenants).Error; err != nil {
		slog.Warn("ai: list tenants", "err", err)
		return nil
	}
	var out []RunResult
	for _, t := range tenants {
		r, err := s.RunOnce(ctx, t)
		if err != nil {
			slog.Warn("ai: cluster tenant", "tenant", t, "err", err)
			continue
		}
		if r.Scanned > 0 {
			out = append(out, *r)
		}
	}
	return out
}

// confirm 确认一个达到阈值的簇：复用既有 open incident（同 cluster_key）或新建。
func (s *Service) confirm(ctx context.Context, tenantID string, c *Cluster, res *RunResult) error {
	var existing incident.Incident
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND source = ? AND cluster_key = ? AND status = ?",
			tenantID, "ai", c.Key, incident.IncidentOpen).
		First(&existing).Error
	switch {
	case err == nil:
		// 既有事故：仅追加新告警，并刷新名称/摘要（成员数已变化）
		for _, id := range c.MemberIDs {
			if aerr := s.inc.AttachAlert(ctx, &existing, &alert.Alert{ID: id, TenantID: tenantID}); aerr != nil {
				return aerr
			}
		}
		if rerr := s.refreshMerged(ctx, tenantID, &existing); rerr != nil {
			return rerr
		}
		res.Merged++
		return nil
	case err != gorm.ErrRecordNotFound:
		return err
	}

	name, summary := Summarize(c)
	inc := &incident.Incident{
		ID:         util.NewID(),
		TenantID:   tenantID,
		ClusterKey: c.Key,
		Name:       name,
		Status:     incident.IncidentOpen,
		Severity:   c.Severity,
		Summary:    summary,
		Source:     "ai",
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.inc.Create(ctx, inc); err != nil {
		return err
	}
	for _, id := range c.MemberIDs {
		if aerr := s.inc.AttachAlert(ctx, inc, &alert.Alert{ID: id, TenantID: tenantID}); aerr != nil {
			return aerr
		}
	}
	res.Clusters++
	res.Created = append(res.Created, inc)
	slog.Info("ai: cluster confirmed incident", "tenant", tenantID, "key", c.Key, "service", c.Service, "count", c.Count)
	// 聚类摘要接 LLM：非确定性模板。建事故 + 关联告警完成后才增强（best-effort，失败仅 warn，
	// 保留确定性 summary，不阻塞也不影响已落库的事故；顺序放最后避免 LLM 卡住导致孤儿事故）。
	if s.clusterLLM != nil {
		if out, err := s.clusterLLM.EnhanceCluster(ctx, clusterInput(c)); err != nil {
			slog.Warn("ai: cluster llm enhance skipped (keep deterministic)", "service", c.Service, "err", err)
		} else if out != nil && out.Summary != "" {
			if uerr := s.inc.UpdateLLMSummary(ctx, tenantID, inc.ID, out.Summary); uerr != nil {
				slog.Warn("ai: cluster llm summary update", "incident", inc.ID, "err", uerr)
			}
		}
	}
	return nil
}

// refreshMerged 合并后按累计成员重建名称/摘要（合并前的名称停留在创建时的告警数，会失真）。
func (s *Service) refreshMerged(ctx context.Context, tenantID string, inc *incident.Incident) error {
	alerts, err := s.inc.AlertsOf(ctx, tenantID, inc.ID)
	if err != nil {
		return err
	}
	if len(alerts) == 0 {
		return nil
	}
	memberIDs := make([]string, len(alerts))
	for i := range alerts {
		memberIDs[i] = alerts[i].ID
	}
	c := &Cluster{
		Service:    labelService(&alerts[0]),
		Names:      distinctNames(memberIDs, alerts),
		Severity:   inc.Severity,
		Count:      len(alerts),
		StartedAt:  inc.CreatedAt,
		LastSeenAt: time.Now().UTC(),
	}
	for i := range alerts { // 严重度取成员最高（合并的新成员可能更高）
		if alert.SeverityRank(alerts[i].Severity) > alert.SeverityRank(c.Severity) {
			c.Severity = alerts[i].Severity
		}
	}
	name, summary := Summarize(c)
	if err := s.inc.UpdateNameSummary(ctx, tenantID, inc.ID, name, summary); err != nil {
		return err
	}
	if s.clusterLLM != nil {
		// 合并后成员变化 → 刷新 LLM 摘要（best-effort，失败保持原有摘要，fail-open 不返回 error）
		if out, err := s.clusterLLM.EnhanceCluster(ctx, clusterInput(c)); err != nil {
			slog.Warn("ai: cluster llm re-enhance skipped", "incident", inc.ID, "err", err)
		} else if out != nil && out.Summary != "" {
			if uerr := s.inc.UpdateLLMSummary(ctx, tenantID, inc.ID, out.Summary); uerr != nil {
				slog.Warn("ai: cluster llm re-enhance summary update", "incident", inc.ID, "err", uerr)
			}
		}
	}
	return nil
}

// recentFiringUnattached 返回租户在窗口内、未被任何 incident 承接的 firing 告警。
func (s *Service) recentFiringUnattached(ctx context.Context, tenantID string) ([]alert.Alert, error) {
	since := time.Now().UTC().Add(-s.window)
	var items []alert.Alert
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND status = ? AND created_at >= ? AND id NOT IN (?)",
			tenantID, alert.StatusFiring, since,
			s.db.WithContext(ctx).Model(&incident.AlertToIncident{}).Select("alert_id")).
		Order("created_at ASC").Limit(500).Find(&items).Error
	return items, err
}
