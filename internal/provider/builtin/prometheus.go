// Package builtin 内置 provider 实现。每个 provider 在 init() 注册到全局 Registry。
package builtin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"aiops/internal/alert"
)

// AlertmanagerWebhook 对应 Alertmanager webhook 推送格式。
type AlertmanagerWebhook struct {
	Status string `json:"status"`
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    time.Time         `json:"startsAt"`
		EndsAt      time.Time         `json:"endsAt"`
	} `json:"alerts"`
}

func init() {
	Registry.Register(&PrometheusProvider{})
}

// PrometheusProvider 对接 Prometheus Alertmanager webhook。
type PrometheusProvider struct{}

// Kind 数据源类型。
func (p *PrometheusProvider) Kind() string { return "prometheus" }

// Name 显示名。
func (p *PrometheusProvider) Name() string { return "Prometheus / Alertmanager" }

// Parse 解析 Alertmanager webhook → 归一化入站事件。
func (p *PrometheusProvider) Parse(body []byte, _ *http.Request) ([]*alert.IngestEvent, error) {
	var wh AlertmanagerWebhook
	if err := json.Unmarshal(body, &wh); err != nil {
		return nil, fmt.Errorf("prometheus: invalid webhook: %w", err)
	}
	events := make([]*alert.IngestEvent, 0, len(wh.Alerts))
	for _, a := range wh.Alerts {
		name := a.Annotations["summary"]
		if name == "" {
			name = a.Labels["alertname"]
		}
		if name == "" {
			name = "prometheus-alert"
		}
		sev := a.Labels["severity"]
		if sev == "" {
			sev = alert.SeverityHigh
		}
		labels := make(map[string]any, len(a.Labels))
		for k, v := range a.Labels {
			labels[k] = v
		}
		ev := map[string]any{
			"status": a.Status, "startsAt": a.StartsAt, "endsAt": a.EndsAt,
			"labels": labels,
		}
		if a.Annotations != nil {
			ev["annotations"] = a.Annotations
		}
		started := a.StartsAt
		if started.IsZero() {
			started = time.Now().UTC()
		}
		events = append(events, &alert.IngestEvent{
			TenantID:     "default",
			Provider:     p.Kind(),
			ProviderType: "prometheus",
			Name:         name,
			Severity:     sev,
			Status:       a.Status,
			Labels:       labels,
			Event:        ev,
			StartedAt:    &started,
		})
	}
	return events, nil
}
