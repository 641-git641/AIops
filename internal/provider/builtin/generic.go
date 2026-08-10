package builtin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aiops/internal/alert"
)

// generic.go：通用告警入口（KEEPON 5.2）——个人脚本一条 curl 接入。
// 契约 {title, severity, status, source, dedupKey, labels, description}；
// severity 别名归一化与状态校验在此完成（与 BuildAlert 共用同一映射表）；
// description 只进 Event 快照、绝不进 Labels（含变化内容，进 Labels 会破坏指纹稳定性）。

// GenericWebhook generic webhook payload。
type GenericWebhook struct {
	Title       string         `json:"title"`
	Severity    string         `json:"severity"`
	Status      string         `json:"status"`
	Source      string         `json:"source"`
	DedupKey    string         `json:"dedupKey"`
	Labels      map[string]any `json:"labels"`
	Description string         `json:"description"`
}

// genericSeverityAllowed severity 白名单（含别名；归一化后写入）。
var genericSeverityAllowed = map[string]bool{
	"p0": true, "severe": true, "critical": true, "error": true, "high": true,
	"warning": true, "warn": true, "medium": true, "low": true, "notice": true, "info": true,
}

type genericProvider struct{}

func init() {
	Registry.Register(genericProvider{})
}

// Kind 注册名。
func (genericProvider) Kind() string { return "generic" }

// Name 展示名。
func (genericProvider) Name() string { return "通用告警入口（一条 curl 接入）" }

// Parse 校验并归一化 generic webhook payload（必填字段缺失/非法值返回 error → 400）。
func (genericProvider) Parse(body []byte, _ *http.Request) ([]*alert.IngestEvent, error) {
	var in GenericWebhook
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("generic: 非法 JSON: %w", err)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, fmt.Errorf("generic: title 必填")
	}
	if in.Severity == "" {
		return nil, fmt.Errorf("generic: severity 必填（p0/critical/high/medium/low/info/error/warning/notice）")
	}
	if !genericSeverityAllowed[strings.ToLower(strings.TrimSpace(in.Severity))] {
		return nil, fmt.Errorf("generic: severity %q 不支持（p0/critical/high/medium/low/info/error/warning/notice）", in.Severity)
	}
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = alert.StatusFiring
	}
	if status != alert.StatusFiring && status != alert.StatusResolved {
		return nil, fmt.Errorf("generic: status %q 不支持（firing|resolved）", status)
	}

	labels := in.Labels
	if labels == nil {
		labels = map[string]any{}
	}
	labels["dedupKey"] = in.DedupKey // 空值也写入：参与指纹，同 dedupKey 稳定去重
	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = "generic"
	}
	now := time.Now().UTC()
	event := map[string]any{
		"title":       title,
		"severity":    in.Severity,
		"status":      status,
		"source":      source,
		"dedupKey":    in.DedupKey,
		"description": in.Description, // 仅快照，不参与指纹
	}
	if in.Labels != nil {
		event["labels"] = in.Labels
	}

	return []*alert.IngestEvent{{
		Provider:     "generic",
		ProviderType: source,
		Name:         title,
		Severity:     in.Severity, // 别名由 BuildAlert 归一化（与全部 provider 一致）
		Status:       status,
		Labels:       labels,
		Event:        event,
		StartedAt:    &now,
	}}, nil
}
