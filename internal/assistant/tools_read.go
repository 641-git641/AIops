package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"aiops/internal/alert"
	"aiops/internal/incident"
)

// tools_read.go：只读领域工具（AGENT_CHAT_PLAN §5.3 首期 6 个）。
// 全部：参数二次校验（schema 白名单）、limit/window/depth 钳制、结果裁剪 ≤8KB、
// 结果视为 untrusted content（防提示注入）；工具结果进 steps 只存裁剪摘要。

const (
	defaultLimit = 10
	maxListLimit = 50
)

// registerReadTools 注册只读工具。
func registerReadTools(r *Registry, deps ToolDeps) {
	registerEvidenceTools(r, deps) // 数据源工具（deps 为 nil 时不注册）
	r.tools["get_platform_overview"] = Tool{
		Name: "get_platform_overview", Description: "查询平台总览：活跃告警/事故计数、24h 趋势、系统健康与队列状态。",
		JSONSchema: schema(map[string]any{
			"window_hours": intProp("统计窗口小时数（1-168，默认 24）", 1, 168),
		}),
		ReadOnly: true, Timeout: 3 * time.Second,
		Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
			w := intArg(in.Args, "window_hours", 24)
			ov, err := deps.Overview.Get(ctx, in.TenantID, time.Duration(w)*time.Hour)
			if err != nil {
				return nil, err
			}
			return trimToolResult(sprintJSON(map[string]any{
				"window":  ov.Window,
				"counts":  ov.Counts,
				"system":  ov.System,
				"recentEvents": trimSlice(ov.RecentEvents, 5),
			})), nil
		},
	}

	r.tools["list_alerts"] = Tool{
		Name: "list_alerts", Description: "查询告警列表（按时间倒序）。可按 status/severity/provider 过滤。",
		JSONSchema: schema(map[string]any{
			"status":   enumProp("告警状态", "firing", "resolved", "acknowledged", "suppressed"),
			"severity": enumProp("严重度", "critical", "high", "medium", "low", "info"),
			"provider": strProp("数据源"),
			"limit":    intProp("返回条数（1-50，默认 10）", 1, maxListLimit),
		}),
		ReadOnly: true, Timeout: 3 * time.Second,
		Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
			alerts, _, err := deps.Alerts.List(ctx, alert.Query{
				TenantID: in.TenantID,
				Status:   stringArg(in.Args, "status"),
				Severity: stringArg(in.Args, "severity"),
				Provider: stringArg(in.Args, "provider"),
				Limit:    clampInt(intArg(in.Args, "limit", defaultLimit), 1, maxListLimit),
			})
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(alerts))
			for _, a := range alerts {
				d := a.ToDTO()
				out = append(out, map[string]any{
					"id": d.ID, "name": d.Name, "severity": d.Severity, "status": d.Status,
					"provider": d.Provider, "service": pickString(d.Labels, "service"),
					"startedAt": d.StartedAt.Format(time.RFC3339),
				})
			}
			return trimToolResult(sprintJSON(map[string]any{"count": len(out), "alerts": out})), nil
		},
	}

	r.tools["get_alert"] = Tool{
		Name: "get_alert", Description: "查询单条告警详情（含标签与事件快照摘要）。",
		JSONSchema: schema(map[string]any{
			"alert_id": strProp("告警 ID（必填）"),
		}, "alert_id"),
		ReadOnly: true, Timeout: 3 * time.Second,
		Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
			a, err := deps.Alerts.Get(ctx, in.TenantID, stringArg(in.Args, "alert_id"))
			if err != nil {
				return nil, err
			}
			if a == nil {
				return map[string]any{"error": "告警不存在"}, nil
			}
			d := a.ToDTO()
			return trimToolResult(sprintJSON(map[string]any{
				"id": d.ID, "name": d.Name, "severity": d.Severity, "status": d.Status,
				"provider": d.Provider, "fingerprint": d.Fingerprint,
				"labels":    trimMap(d.Labels, 10),
				"eventKeys": sortedKeys(d.Event),
				"startedAt": d.StartedAt.Format(time.RFC3339), "lastSeenAt": d.LastSeenAt.Format(time.RFC3339),
			})), nil
		},
	}

	r.tools["list_incidents"] = Tool{
		Name: "list_incidents", Description: "查询事故列表（按时间倒序）。可按 status 过滤；severity 为辅助过滤（Service 无 SQL 过滤，工具层内存过滤）。",
		JSONSchema: schema(map[string]any{
			"status":   enumProp("事故状态", "open", "resolved", "merged", "pending_approval"),
			"severity": enumProp("严重度", "critical", "high", "medium", "low", "info"),
			"limit":    intProp("返回条数（1-50，默认 10）", 1, maxListLimit),
		}),
		ReadOnly: true, Timeout: 3 * time.Second,
		Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
			incidents, _, err := deps.Incidents.List(ctx, in.TenantID,
				stringArg(in.Args, "status"), clampInt(intArg(in.Args, "limit", defaultLimit)*3, 1, 200), 0)
			if err != nil {
				return nil, err
			}
			sev := stringArg(in.Args, "severity")
			out := make([]map[string]any, 0, len(incidents))
			for _, inc := range incidents {
				if sev != "" && inc.Severity != sev {
					continue // 内存二次过滤（领域 List 无 severity，Phase 2 可扩展 ListWithFilters）
				}
				out = append(out, map[string]any{
					"id": inc.ID, "name": firstNonEmpty(inc.AIGeneratedName, inc.Name),
					"status": inc.Status, "severity": inc.Severity, "summary": inc.Summary,
					"source": inc.Source, "createdAt": inc.CreatedAt.Format(time.RFC3339),
				})
				if len(out) >= maxListLimit {
					break
				}
			}
			return trimToolResult(sprintJSON(map[string]any{"count": len(out), "incidents": out})), nil
		},
	}

	r.tools["get_incident"] = Tool{
		Name: "get_incident", Description: "查询事故详情：状态/严重度/摘要 + 编排器节点结果摘要（monitor/rca/heal/change）+ 关联告警。",
		JSONSchema: schema(map[string]any{
			"incident_id": strProp("事故 ID（必填）"),
		}, "incident_id"),
		ReadOnly: true, Timeout: 3 * time.Second,
		Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
			inc, err := deps.Incidents.Get(ctx, in.TenantID, stringArg(in.Args, "incident_id"))
			if err != nil {
				return nil, err
			}
			if inc == nil {
				return map[string]any{"error": "事故不存在"}, nil
			}
			st, err := deps.Incidents.GetState(ctx, in.TenantID, inc.ID)
			if err != nil || st == nil {
				st = nil
			}
			alerts, err := deps.Incidents.AlertsOf(ctx, in.TenantID, inc.ID)
			if err != nil {
				alerts = nil
			}
			alertNames := make([]string, 0, len(alerts))
			for _, a := range alerts {
				alertNames = append(alertNames, a.Name)
			}
			return trimToolResult(sprintJSON(map[string]any{
				"id": inc.ID, "name": firstNonEmpty(inc.AIGeneratedName, inc.Name),
				"status": inc.Status, "severity": inc.Severity, "summary": inc.Summary,
				"source": inc.Source, "createdAt": inc.CreatedAt.Format(time.RFC3339),
				"nodeResults": summarizeNodeResults(st),
				"alertCount":  len(alertNames), "alertNames": trimSlice(alertNames, 10),
			})), nil
		},
	}

	r.tools["get_topology_impact"] = Tool{
		Name: "get_topology_impact", Description: "查询服务拓扑影响面：哪些下游依赖该服务（深度受限的依赖链）。",
		JSONSchema: schema(map[string]any{
			"service": strProp("服务名（必填）"),
			"depth":   intProp("影响深度（1-5，默认 5）", 1, 5),
		}, "service"),
		ReadOnly: true, Timeout: 3 * time.Second,
		Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
			nodes, err := deps.Topology.Impact(ctx, in.TenantID,
				stringArg(in.Args, "service"), clampInt(intArg(in.Args, "depth", 5), 1, 5))
			if err != nil {
				return nil, err
			}
			if len(nodes) == 0 {
				return map[string]any{"service": stringArg(in.Args, "service"), "impacted": []any{}}, nil
			}
			out := make([]map[string]any, 0, len(nodes))
			for _, n := range nodes {
				out = append(out, map[string]any{
					"service": n.Service, "depth": n.Depth, "path": n.Path,
				})
			}
			return trimToolResult(sprintJSON(map[string]any{
				"service": stringArg(in.Args, "service"), "impacted": out,
			})), nil
		},
	}
}

// summarizeNodeResults 编排器节点结果摘要（monitor/rca/heal/change 四节点形状）。
// 节点缺失或非法跳过；只取模型需要的事实字段（不返回原始大 JSON）。
// 接收具体类型 *incident.State（typed-nil 安全）。
func summarizeNodeResults(st *incident.State) map[string]any {
	if st == nil {
		return nil
	}
	out := map[string]any{}
	if m := st.NodeResult("monitor"); m != nil {
		out["monitor"] = pickMap(m, "severity", "service", "provider", "alertName", "fingerprint")
	}
	if m := st.NodeResult("rca"); m != nil {
		rca := pickMap(m, "service", "confidence", "rootCause", "evidence", "paths", "suggestedActions")
		if llmRC, ok := m["llmRootCause"]; ok {
			rca["llmRootCause"] = llmRC
		}
		out["rca"] = rca
	}
	if m := st.NodeResult("heal"); m != nil {
		out["heal"] = pickMap(m, "status", "action", "level", "blastRadius", "downgraded",
			"approvalRequired", "dryRun", "playbookId", "reason")
	}
	if m := st.NodeResult("change"); m != nil {
		change := pickMap(m, "action", "riskScore", "level", "decision", "approvalRequired")
		if bd, ok := m["breakdown"]; ok {
			change["breakdown"] = bd
		}
		out["change"] = change
	}
	return out
}

// pickMap 从 map 取指定键（保持顺序；不存在的键跳过）。
func pickMap(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// pickString 从标签 map 取值。
func pickString(labels map[string]any, key string) string {
	if v, ok := labels[key].(string); ok {
		return v
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// sprintJSON 序列化为单行 JSON 字符串（工具结果统一形态；失败回退 %v）。
func sprintJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// trimSlice 取前 n 项。
func trimSlice[T any](s []T, n int) []T {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// trimMap 取前 n 键（map 顺序不稳定但可接受——结果仅供模型阅读）。
func trimMap(m map[string]any, n int) map[string]any {
	if len(m) <= n {
		return m
	}
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
		if len(out) >= n {
			break
		}
	}
	return out
}

// sortedKeys 事件快照的键列表（不返回原始事件内容——不可信数据防注入）。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 简单排序保持确定性
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

