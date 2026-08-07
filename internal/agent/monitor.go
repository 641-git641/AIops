package agent

import (
	"context"
	"encoding/json"
	"time"

	"aiops/internal/alert"
	"aiops/internal/incident"
)

// MonitorAgent 监控分析 Agent：对触发告警做诊断上下文收集与严重度复核。
// 在告警平台场景下，异常检测已由上游（Prometheus 等）完成，本 Agent 负责
// 把告警转成编排上下文（受影响服务、严重度再分级），供后续 RCA 使用。
type MonitorAgent struct{}

// NewMonitorAgent 创建监控分析 Agent。
func NewMonitorAgent() *MonitorAgent { return &MonitorAgent{} }

// Name Agent 名称。
func (m *MonitorAgent) Name() string { return "monitor" }

// Handle 分析告警并输出诊断结果。
func (m *MonitorAgent) Handle(ctx context.Context, c *Context) error {
	a := c.Alert
	state := c.State

	state.Status = incident.StateInvestigating
	service := serviceOf(a)

	result := map[string]any{
		"severity":    a.Severity,
		"service":     service,
		"provider":    a.Provider,
		"alertName":   a.Name,
		"fingerprint": a.Fingerprint,
		"analyzedAt":  time.Now().UTC().Format(time.RFC3339),
	}
	state.SetNodeResult(m.Name(), result)
	return nil
}

// serviceOf 从标签/事件中提取受影响服务名。
func serviceOf(a *alert.Alert) string {
	if a == nil {
		return ""
	}
	labels := map[string]any{}
	_ = json.Unmarshal(a.Labels, &labels)
	if v, ok := labels["service"].(string); ok && v != "" {
		return v
	}
	if v, ok := labels["service_name"].(string); ok && v != "" {
		return v
	}
	// event 兜底
	event := map[string]any{}
	_ = json.Unmarshal(a.Event, &event)
	for _, key := range []string{"service", "service_name", "target_service"} {
		if v, ok := event[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
