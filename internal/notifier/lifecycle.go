package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"aiops/internal/eventbus"
	"aiops/internal/incident"

	"gorm.io/datatypes"
)

// IncidentLifecycleMessage 构建事故生命周期通知（新建/解决）。纯函数，便于单测。
// action 取值与 Message.Action 对齐：created / resolved。
func IncidentLifecycleMessage(inc *incident.Incident, action string) Message {
	title := "【新事故】" + inc.Name
	lines := []string{
		fmt.Sprintf("**来源**：`%s`", inc.Source),
		fmt.Sprintf("**严重度**：`%s`", inc.Severity),
	}
	if inc.Summary != "" {
		lines = append(lines, inc.Summary)
	}
	switch action {
	case "resolved":
		title = "【事故已解决】" + inc.Name
		lines = append(lines, "**状态**：✅ 事故已解决")
	default:
		lines = append(lines, "**状态**：🆕 新事故已创建，请关注")
	}
	return Message{
		Title:      title,
		Content:    strings.Join(lines, "\n"),
		Severity:   inc.Severity,
		IncidentID: inc.ID,
		Action:     action,
	}
}

// LifecycleNotifier 事故生命周期通知（created/resolved）发送 + 审计。
// 由装配层注入 incident.Service.SetNotifyFunc；fail-open：无渠道/失败仅记审计不抛错。
type LifecycleNotifier struct {
	ntf   *Service
	audit *eventbus.AuditService // 可为 nil（不落审计）
}

// NewLifecycleNotifier 创建生命周期通知器。
func NewLifecycleNotifier(ntf *Service, audit *eventbus.AuditService) *LifecycleNotifier {
	return &LifecycleNotifier{ntf: ntf, audit: audit}
}

// Notify 发送事故生命周期通知并落审计（签名匹配 incident.IncidentNotify）。
func (n *LifecycleNotifier) Notify(ctx context.Context, inc *incident.Incident, action string) {
	if n.ntf == nil {
		return
	}
	msg := IncidentLifecycleMessage(inc, action)
	msg.IncidentID = inc.ID
	target := "incident." + action // incident.created / incident.resolved

	results := n.ntf.Notify(ctx, msg)
	if len(results) == 0 {
		n.auditRecord(ctx, inc, "notify.skipped", target, "未配置通知渠道（AIOPS_NOTIFIERCHANNELS 为空）",
			map[string]any{"channels": n.ntf.Enabled()})
		return
	}
	auditAction := "notify.sent"
	if !n.ntf.AllOK(results) {
		auditAction = "notify.failed"
	}
	n.auditRecord(ctx, inc, auditAction, target,
		fmt.Sprintf("通知 %s 结果：%v", action, results), map[string]any{"channels": n.ntf.Enabled(), "results": results})
}

// auditRecord 落审计（audit 为 nil 时跳过）。
func (n *LifecycleNotifier) auditRecord(ctx context.Context, inc *incident.Incident, action, target, reason string, meta map[string]any) {
	if n.audit == nil {
		return
	}
	b, _ := json.Marshal(meta)
	_, _ = n.audit.Record(ctx, eventbus.AuditLog{
		TenantID: inc.TenantID, IncidentID: inc.ID,
		Actor: "auto-system", Action: action, Target: target, Reason: reason,
		Metadata: datatypes.JSON(b),
	})
}
