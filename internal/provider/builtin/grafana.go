// Package builtin 内置 provider 实现。每个 provider 在 init() 注册到全局 Registry。
package builtin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"aiops/internal/alert"
)

// GrafanaWebhook 对应 Grafana Unified Alerting webhook 通知负载
// （Grafana Alerting → 通知渠道 webhook；与 Alertmanager 负载结构相似但含
// generatorURL/fingerprint/values 等 Grafana 特有字段）。
type GrafanaWebhook struct {
	Receiver string `json:"receiver"`
	Status   string `json:"status"` // firing | resolved（顶层兜底）
	Alerts   []struct {
		Status       string             `json:"status"`
		Labels       map[string]string  `json:"labels"` // 含 alertname / grafana_folder / severity 等
		Annotations  map[string]string  `json:"annotations"`
		StartsAt     time.Time          `json:"startsAt"`
		EndsAt       time.Time          `json:"endsAt"`
		GeneratorURL string             `json:"generatorURL"`
		Fingerprint  string             `json:"fingerprint"`
		Values       map[string]float64 `json:"values"`
	} `json:"alerts"`
}

func init() {
	Registry.Register(&GrafanaProvider{})
}

// GrafanaProvider 对接 Grafana Alerting webhook（Unified Alerting 通知格式）。
type GrafanaProvider struct{}

// Kind 数据源类型。
func (p *GrafanaProvider) Kind() string { return "grafana" }

// Name 显示名。
func (p *GrafanaProvider) Name() string { return "Grafana Alerting" }

// Parse 解析 Grafana webhook → 归一化入站事件。
// 字段映射与 prometheus.go 对齐：annotations 只进 Event 不进 Labels；
// per-alert status 优先，顶层 status 兜底。
func (p *GrafanaProvider) Parse(body []byte, _ *http.Request) ([]*alert.IngestEvent, error) {
	var wh GrafanaWebhook
	if err := json.Unmarshal(body, &wh); err != nil {
		return nil, fmt.Errorf("grafana: invalid webhook: %w", err)
	}
	events := make([]*alert.IngestEvent, 0, len(wh.Alerts))
	for _, a := range wh.Alerts {
		name := a.Annotations["summary"]
		if name == "" {
			name = a.Labels["alertname"]
		}
		if name == "" {
			name = "grafana-alert"
		}
		sev := a.Labels["severity"]
		if sev == "" {
			sev = alert.SeverityHigh
		}
		status := a.Status
		if status == "" {
			status = wh.Status // 顶层兜底（旧版 Grafana 只有顶层 state）
		}
		labels := make(map[string]any, len(a.Labels))
		for k, v := range a.Labels {
			labels[k] = v
		}
		event := map[string]any{
			"status":   status,
			"startsAt": a.StartsAt,
			"endsAt":   a.EndsAt,
			"labels":   labels,
		}
		if len(a.Annotations) > 0 {
			event["annotations"] = a.Annotations // 只进 Event，不进 Labels（对齐 prometheus.go）
		}
		if a.GeneratorURL != "" {
			event["generatorURL"] = a.GeneratorURL
		}
		if a.Fingerprint != "" {
			event["fingerprint"] = a.Fingerprint
		}
		if len(a.Values) > 0 {
			event["values"] = a.Values
		}
		started := a.StartsAt
		if started.IsZero() {
			started = time.Now().UTC()
		}
		events = append(events, &alert.IngestEvent{
			Provider:     "grafana",
			ProviderType: "grafana",
			Name:         name,
			Severity:     sev,
			Status:       status,
			Labels:       labels,
			Event:        event,
			StartedAt:    &started,
		})
	}
	return events, nil
}
