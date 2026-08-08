package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"aiops/internal/alert"
	"aiops/internal/cel"
	"aiops/internal/util"

	"gorm.io/gorm"
)

// RuleEngine 关联规则引擎：对告警求值 CEL 规则，命中后按分组指纹聚合 Incident。
// 参考 keep 的 RulesEngine + multi-agent-aiops 的编排条件路由。
type RuleEngine struct {
	db  *gorm.DB
	cel *cel.Engine
	svc *Service

	// 规则缓存（规模化延伸）：热路径按租户缓存「启用规则」（TTL 内不重复查库——
	// 原实现每事件全量读 rules 表，是逐事件 ~50ms 串行成本里 1 次 DB 往返）。
	// 规则 CRUD 后调用 Invalidate 立即失效；模式对齐 WindowFilter（锁外查库 + double-check）。
	ruleTTL   time.Duration
	loadRules func(ctx context.Context, tenantID string) ([]Rule, error) // 可注入（单测用假 loader）
	now       func() time.Time                                           // 时钟注入（单测用）

	ruleMu    sync.RWMutex
	ruleCache map[string]ruleCacheEntry // tenantID → 缓存
}

type ruleCacheEntry struct {
	rules   []Rule
	fetched time.Time
}

// NewRuleEngine 创建规则引擎。ruleTTL 为启用规则缓存有效期（<=0 默认 5s，与 WindowFilter 一致）。
func NewRuleEngine(db *gorm.DB, e *cel.Engine, svc *Service, ruleTTL time.Duration) *RuleEngine {
	if ruleTTL <= 0 {
		ruleTTL = 5 * time.Second
	}
	re := &RuleEngine{
		db: db, cel: e, svc: svc,
		ruleTTL:   ruleTTL,
		now:       time.Now,
		ruleCache: make(map[string]ruleCacheEntry),
	}
	re.loadRules = re.queryEnabledRules
	return re
}

// Invalidate 使某租户的规则缓存立即失效（规则 CRUD 后调用；下次 Evaluate 重新查库）。
func (re *RuleEngine) Invalidate(tenantID string) {
	re.ruleMu.Lock()
	delete(re.ruleCache, tenantID)
	re.ruleMu.Unlock()
}

// Evaluate 对单个告警执行全部启用的规则，返回可能创建/更新的 incident。
// 返回 nil 表示未命中或阈值未达。
func (re *RuleEngine) Evaluate(ctx context.Context, a *alert.Alert) (*Incident, error) {
	rules, err := re.enabledRules(ctx, a.TenantID)
	if err != nil {
		return nil, err
	}
	vars := alertVars(a)
	var hitIncident *Incident
	for i := range rules {
		r := &rules[i]
		ok, err := re.cel.EvalBool(r.CELExpression, vars)
		if err != nil {
			slog.Warn("rule eval failed", "rule", r.ID, "err", err)
			continue
		}
		if !ok {
			continue
		}
		inc, err := re.upsertIncidentForRule(ctx, r, a)
		if err != nil {
			slog.Warn("rule incident failed", "rule", r.ID, "err", err)
			continue
		}
		if inc != nil {
			hitIncident = inc
		}
	}
	return hitIncident, nil
}

// enabledRules 返回某租户的启用规则（缓存优先）。模式对齐 WindowFilter：
// 快路径只读锁判缓存命中（不碰 DB）；未命中时锁外查库（并发请求各自触发，
// 但每租户至多一次，double-check 回填更新鲜结果）——避免锁内查库在缓存过期
// 瞬间所有并发请求在锁前排大队。
func (re *RuleEngine) enabledRules(ctx context.Context, tenantID string) ([]Rule, error) {
	re.ruleMu.RLock()
	if e, ok := re.ruleCache[tenantID]; ok && re.now().Sub(e.fetched) < re.ruleTTL {
		re.ruleMu.RUnlock()
		return e.rules, nil
	}
	re.ruleMu.RUnlock()

	rules, err := re.loadRules(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	re.ruleMu.Lock()
	defer re.ruleMu.Unlock()
	if e, ok := re.ruleCache[tenantID]; ok && re.now().Sub(e.fetched) < re.ruleTTL {
		return e.rules, nil // 已被其他请求回填更新鲜，直接返回
	}
	re.ruleCache[tenantID] = ruleCacheEntry{rules: rules, fetched: re.now()}
	return rules, nil
}

// queryEnabledRules 全量查询某租户的启用规则（默认 loader）。
func (re *RuleEngine) queryEnabledRules(ctx context.Context, tenantID string) ([]Rule, error) {
	var rules []Rule
	err := re.db.WithContext(ctx).Where("tenant_id = ? AND enabled = ?", tenantID, true).Find(&rules).Error
	return rules, err
}

// upsertIncidentForRule 按分组指纹查找/创建 incident 并关联告警。
func (re *RuleEngine) upsertIncidentForRule(ctx context.Context, r *Rule, a *alert.Alert) (*Incident, error) {
	groupKey := groupingKey(r, a)
	ruleFP := util.SHA256(r.ID + ":" + groupKey)

	// 查找该规则指纹下活跃 incident
	var inc *Incident
	var existing Incident
	err := re.db.WithContext(ctx).
		Where("tenant_id = ? AND rule_fingerprint = ? AND status IN ?", a.TenantID, ruleFP, []string{IncidentOpen}).
		Order("created_at ASC").First(&existing).Error
	switch {
	case err == nil:
		inc = &existing
	case err == gorm.ErrRecordNotFound:
		// 阈值判定：先数分组内已命中次数
		cnt, cerr := re.groupHitCount(ctx, r, a, groupKey)
		if cerr != nil {
			return nil, cerr
		}
		if int(cnt)+1 < r.Threshold {
			return nil, nil // 阈值未达
		}
		inc = &Incident{
			ID:              util.NewID(),
			TenantID:        a.TenantID,
			RuleFingerprint: ruleFP,
			Name:            r.Name,
			Status:          IncidentOpen,
			Severity:        a.Severity,
			Source:          "rule",
			CreatedAt:       time.Now().UTC(),
		}
		if err := re.svc.Create(ctx, inc); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}

	// 关联告警（幂等）
	if err := re.svc.AttachAlert(ctx, inc, a); err != nil {
		return nil, err
	}
	return inc, nil
}

// groupHitCount 统计分组键下已有告警数量（用于 threshold）。
func (re *RuleEngine) groupHitCount(ctx context.Context, r *Rule, a *alert.Alert, groupKey string) (int64, error) {
	var cnt int64
	// 简化：按 rule + 最近 1 小时同 severity 估算；MVP 用 incident 关联表统计
	err := re.db.WithContext(ctx).Model(&AlertToIncident{}).
		Where("tenant_id = ? AND alert_id IN (?)", a.TenantID,
			re.db.WithContext(ctx).Table("alerts").Select("id").
				Where("tenant_id = ? AND severity = ? AND created_at > ?", a.TenantID, a.Severity, time.Now().Add(-time.Hour))).
		Count(&cnt).Error
	return cnt, err
}

// groupingKey 按分组字段计算分组键。
func groupingKey(r *Rule, a *alert.Alert) string {
	var fields []string
	_ = json.Unmarshal(r.GroupingCriteria, &fields)
	if len(fields) == 0 {
		return "all" // 无分组字段时所有命中归一组
	}
	labels := map[string]any{}
	_ = json.Unmarshal(a.Labels, &labels)
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		switch f {
		case "provider":
			parts = append(parts, a.Provider)
		case "severity":
			parts = append(parts, a.Severity)
		case "name":
			parts = append(parts, a.Name)
		default:
			if v, ok := labels[trimLabelKey(f)]; ok {
				parts = append(parts, fmt.Sprintf("%v", v))
			} else {
				parts = append(parts, "")
			}
		}
	}
	sort.Strings(parts)
	// 稳定分组键：按标签值拼接后哈希（与字段顺序无关）
	args := make([]any, len(parts))
	for i, p := range parts {
		args[i] = p
	}
	return util.HashEvent(args...)
}

// trimLabelKey 去掉 "labels." 前缀得到标签键。
func trimLabelKey(s string) string {
	const prefix = "labels."
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

// alertVars 构建 CEL 求值上下文。
func alertVars(a *alert.Alert) map[string]any {
	labels := map[string]any{}
	_ = json.Unmarshal(a.Labels, &labels)
	event := map[string]any{}
	_ = json.Unmarshal(a.Event, &event)
	alertMap := map[string]any{
		"id": a.ID, "provider": a.Provider, "name": a.Name,
		"severity": a.Severity, "status": a.Status, "labels": labels,
	}
	return map[string]any{
		"alert":  alertMap,
		"labels": labels,
		"event":  event,
	}
}
