package notifythrottle

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"aiops/internal/alert"
	"aiops/internal/eventbus"
	"aiops/internal/notifier"
	"aiops/internal/queue"

	"gorm.io/datatypes"
)

// handle.go：notify.send 队列任务处理——节流决策 → 发送 → 审计（可解释"为何发送/被节流"）。
// 通知 fail-open：任何失败只落审计，不阻塞主流程。

// notifyTaskPayload pipeline 投递的告警通知任务。
type notifyTaskPayload struct {
	AlertID     string `json:"alertId"`
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	Severity    string `json:"severity"`
	Status      string `json:"status"`
	Provider    string `json:"provider"`
}

// MarshalNotifyTask 序列化 notify.send 任务（pipeline 钩子使用，热路径仅 marshal + Enqueue）。
func MarshalNotifyTask(a *alert.Alert) ([]byte, error) {
	return json.Marshal(notifyTaskPayload{
		AlertID: a.ID, Fingerprint: a.Fingerprint, Name: a.Name,
		Severity: a.Severity, Status: a.Status, Provider: a.Provider,
	})
}

// HandleNotifyTask queue handler：节流决策 → 发送/抑制 → 审计。
func HandleNotifyTask(ctx context.Context, t queue.Task, th *Throttler, ntf *notifier.Service, audit *eventbus.AuditService, baseURL string) error {
	var p notifyTaskPayload
	if err := json.Unmarshal(t.Payload, &p); err != nil {
		slog.Warn("notify: bad task payload", "err", err)
		return nil // 坏任务丢弃，不重试
	}
	if p.Fingerprint == "" || p.Name == "" {
		return nil
	}

	d := th.Decide(t.TenantID, p.Fingerprint, p.Name, p.Severity, p.Status)
	switch {
	case d.Reason == ReasonCooldown:
		if d.FirstThrottled {
			auditRecord(ctx, audit, t.TenantID, "", "notify.throttled", "alert.firing",
				"进入冷却期，后续重复合并通知（冷却 "+formatDuration(th.Cooldown)+"）",
				map[string]any{"fingerprint": p.Fingerprint, "alertId": p.AlertID, "severity": p.Severity})
		}
		return nil
	case d.Reason == ReasonNoActive:
		return nil
	}

	// 发送决策（first/upgrade/resolved/cooldown-expired）
	resolved := p.Status == alert.StatusResolved
	action := "alert.firing"
	if resolved {
		action = "alert.resolved"
	}
	msg := notifier.Message{
		Title:         "【告警】" + p.Name,
		Content:       notifierContent(p, d, resolved),
		Severity:      p.Severity,
		CorrelationID: p.AlertID,
		URL:           baseURL + "/alerts/" + p.AlertID,
		Action:        action,
	}
	results := ntf.Notify(ctx, msg)
	reason := decisionReasonText(d)
	if len(results) == 0 {
		auditRecord(ctx, audit, t.TenantID, "", "notify.skipped", action,
			reason+"（未配置启用渠道，跳过）", metaOf(p, d))
		return nil
	}
	if ntf.AllOK(results) {
		auditRecord(ctx, audit, t.TenantID, "", "notify.sent", action, reason, metaOf(p, d))
	} else {
		auditRecord(ctx, audit, t.TenantID, "", "notify.failed", action, reason, metaOf(p, d))
	}
	return nil
}

// HandleSummaryTask 汇总任务发送（worker 调用；Count>0 才有任务）。
func HandleSummaryTask(ctx context.Context, task SummaryTask, ntf *notifier.Service, audit *eventbus.AuditService, baseURL string, interval time.Duration) {
	msg := notifier.Message{
		Title:         "【告警汇总】" + task.Name,
		Content:       SummaryText(task, interval),
		Severity:      task.Severity,
		CorrelationID: task.Key.Fingerprint,
		URL:           baseURL,
		Action:        "alert.summary",
	}
	results := ntf.Notify(ctx, msg)
	meta := map[string]any{
		"fingerprint": task.Key.Fingerprint, "count": task.Count,
		"firstAt": task.FirstAt, "lastAt": task.LastAt, "severity": task.Severity,
	}
	if len(results) == 0 {
		auditRecord(ctx, audit, task.Key.TenantID, "", "notify.skipped", "alert.summary",
			"周期汇总（未配置启用渠道，跳过）", meta)
		return
	}
	if ntf.AllOK(results) {
		auditRecord(ctx, audit, task.Key.TenantID, "", "notify.summary", "alert.summary", "周期汇总", meta)
	} else {
		auditRecord(ctx, audit, task.Key.TenantID, "", "notify.failed", "alert.summary", "周期汇总", meta)
	}
}

func notifierContent(p notifyTaskPayload, d Decision, resolved bool) string {
	base := "严重度 " + p.Severity + " · 来源 " + p.Provider
	if resolved {
		return base + " · 已恢复"
	}
	if d.Count > 0 {
		return base + " · 冷却期内已合并 " + strconv.Itoa(d.Count) + " 次重复"
	}
	return base
}

func decisionReasonText(d Decision) string {
	switch d.Reason {
	case ReasonFirst:
		return "首条通知(first)"
	case ReasonUpgrade:
		return "严重度升级，突破冷却(upgrade)"
	case ReasonResolved:
		return "恢复通知(resolved)"
	case ReasonCooldownExpired:
		return "冷却到期后再次触发(cooldown-expired)"
	}
	return d.Reason
}

func metaOf(p notifyTaskPayload, d Decision) map[string]any {
	return map[string]any{
		"fingerprint": p.Fingerprint, "alertId": p.AlertID,
		"severity": p.Severity, "status": p.Status, "count": d.Count,
	}
}

// auditRecord 通知审计（与 notifier/lifecycle.go 既有模式一致；audit 为 nil 时跳过）。
func auditRecord(ctx context.Context, audit *eventbus.AuditService, tenantID, incidentID, action, target, reason string, meta map[string]any) {
	if audit == nil {
		return
	}
	b, _ := json.Marshal(meta)
	_, err := audit.Record(ctx, eventbus.AuditLog{
		TenantID: tenantID, IncidentID: incidentID, CorrelationID: "",
		Actor: "auto-system", Action: action, Target: target, Reason: reason,
		Metadata: datatypes.JSON(b),
	})
	if err != nil {
		slog.Warn("notify: audit record", "err", err)
	}
}
