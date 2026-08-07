package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
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
}

// NewRuleEngine 创建规则引擎。
func NewRuleEngine(db *gorm.DB, e *cel.Engine, svc *Service) *RuleEngine {
	return &RuleEngine{db: db, cel: e, svc: svc}
}

// Evaluate 对单个告警执行全部启用的规则，返回可能创建/更新的 incident。
// 返回 nil 表示未命中或阈值未达。
func (re *RuleEngine) Evaluate(ctx context.Context, a *alert.Alert) (*Incident, error) {
	var rules []Rule
	if err := re.db.WithContext(ctx).Where("tenant_id = ? AND enabled = ?", a.TenantID, true).Find(&rules).Error; err != nil {
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
