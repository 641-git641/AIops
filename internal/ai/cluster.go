// Package ai 提供告警聚类与摘要（确定性实现，不依赖 LLM）。
// 用于对未被规则/Agent 承接的散落告警做事件关联：同一服务在时间窗口内
// 的告警聚为簇，达到阈值后确认生成 Incident（source=ai）。
package ai

import (
	"encoding/json"
	"sort"
	"time"

	"aiops/internal/alert"
	"aiops/internal/util"
)

// Cluster 告警簇（同一故障的多条关联告警）。
type Cluster struct {
	Key        string    `json:"key"` // 稳定分组键：SHA256(service|names|severity)
	Service    string    `json:"service"`
	Names      []string  `json:"names"` // 去重后排序的告警名
	MemberIDs  []string  `json:"memberIds"`
	Severity   string    `json:"severity"` // 簇内最高严重度
	Count      int       `json:"count"`
	StartedAt  time.Time `json:"startedAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	Name       string    `json:"name"`    // 生成的名称（Summarize 填充）
	Summary    string    `json:"summary"` // 生成的摘要（Summarize 填充）
}

// ClusterAlerts 将告警聚类：
//  1. 按 labels.service 分组（缺失归 "unknown"）；
//  2. 组内按时间升序，间隔超过 window 的断开为新簇；
//  3. Key = SHA256(service + 去重排序 names + 最高严重度)（跨运行稳定，用于 Incident 去重）。
//
// 纯函数，便于单测。
func ClusterAlerts(alerts []alert.Alert, window time.Duration) []Cluster {
	bySvc := make(map[string][]alert.Alert)
	for _, a := range alerts {
		bySvc[labelService(&a)] = append(bySvc[labelService(&a)], a)
	}

	var out []Cluster
	for svc, list := range bySvc {
		sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
		var cur *Cluster
		for i := range list {
			a := &list[i]
			if cur != nil && a.CreatedAt.Sub(cur.LastSeenAt) > window {
				out = append(out, *cur)
				cur = nil
			}
			if cur == nil {
				cur = &Cluster{Service: svc, StartedAt: a.CreatedAt, LastSeenAt: a.CreatedAt}
			}
			cur.MemberIDs = append(cur.MemberIDs, a.ID)
			cur.LastSeenAt = a.CreatedAt
			if alert.SeverityRank(a.Severity) > alert.SeverityRank(cur.Severity) {
				cur.Severity = a.Severity
			}
		}
		if cur != nil {
			out = append(out, *cur)
		}
	}

	for i := range out {
		c := &out[i]
		c.Names = distinctNames(c.MemberIDs, alerts)
		c.Key = clusterKey(c.Service, c.Names, c.Severity)
		c.Count = len(c.MemberIDs)
	}
	return out
}

// clusterKey 稳定分组键：跨运行相同（与时间无关），用于 incident 去重。
func clusterKey(service string, names []string, severity string) string {
	return util.SHA256(service + "|" + joinNames(names) + "|" + severity)
}

// clusterInput 把告警簇事实组装成 LLM 摘要增强的输入。
func clusterInput(c *Cluster) ClusterInput {
	return ClusterInput{
		Service:  c.Service,
		Severity: c.Severity,
		Names:    c.Names,
		Count:    c.Count,
	}
}

// distinctNames 收集簇成员去重排序后的告警名。
func distinctNames(memberIDs []string, alerts []alert.Alert) []string {
	byID := make(map[string]string, len(alerts))
	for i := range alerts {
		byID[alerts[i].ID] = alerts[i].Name
	}
	seen := map[string]struct{}{}
	var names []string
	for _, id := range memberIDs {
		n, ok := byID[id]
		if !ok {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func joinNames(names []string) string {
	out := ""
	for _, n := range names {
		out += n + ","
	}
	return out
}

// labelService 从告警 labels 提取 service（缺失归 "unknown"）。
func labelService(a *alert.Alert) string {
	var labels map[string]any
	if err := json.Unmarshal(a.Labels, &labels); err != nil {
		return "unknown"
	}
	if v, ok := labels["service"].(string); ok && v != "" {
		return v
	}
	return "unknown"
}
