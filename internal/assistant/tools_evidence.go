package assistant

import (
	"context"
	"fmt"
	"time"

	"aiops/internal/evidence"
)

// tools_evidence.go：数据源证据工具（Prometheus 指标 / Loki 日志）。
// 查询串由模型生成（不可信）——过注入检测（命中拒绝）；日志行是外部数据——逐行脱敏。
// 数据源不可达 → 返回结构化说明（让模型知道「证据缺失」而非「工具坏了」），不失败 run。

const maxWindowMinutes = 360 // 查询时间窗上限（防拖垮数据源）

// registerEvidenceTools 注册数据源工具（deps 为 nil 时不注册——未配置数据源保持原工具集）。
func registerEvidenceTools(r *Registry, deps ToolDeps) {
	if deps.Prometheus != nil {
		r.tools["query_metrics"] = Tool{
			Name: "query_metrics", Description: "查询 Prometheus 指标趋势（PromQL，只读）。返回每序列的标签与首/末/最大/最小摘要值，用于确认资源/负载异常。",
			JSONSchema: schema(map[string]any{
				"query":          strProp("PromQL 表达式（必填），如 rate(http_requests_total{service=\"pay\"}[5m])"),
				"window_minutes": intProp("查询时间窗分钟（1-360，默认 30）", 1, maxWindowMinutes),
				"step_seconds":   intProp("采样步长秒（≥15，默认 60）", 15, 3600),
			}, "query"),
			ReadOnly: true, Timeout: 5 * time.Second,
			Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
				query := stringArg(in.Args, "query")
				if evidence.CheckQueryInjection(query) {
					return map[string]any{"error": "查询疑似提示注入，已拒绝执行"}, nil
				}
				win := clampInt(intArg(in.Args, "window_minutes", 30), 1, maxWindowMinutes)
				step := time.Duration(clampInt(intArg(in.Args, "step_seconds", 60), 15, 3600)) * time.Second
				now := time.Now().UTC()
				res, err := deps.Prometheus.QueryRange(ctx, query, now.Add(-time.Duration(win)*time.Minute), now, step)
				if err != nil {
					return map[string]any{"error": "prometheus 数据源不可达", "detail": truncateStr(err.Error(), 200)}, nil
				}
				summary := make([]map[string]any, 0, len(res.Series))
				for _, s := range res.Series {
					if len(s.Points) == 0 {
						continue
					}
					summary = append(summary, map[string]any{
						"labels":   s.Labels,
						"first":    s.Points[0][1],
						"last":     s.Points[len(s.Points)-1][1],
						"max":      maxOf(s.Points),
						"min":      minOf(s.Points),
						"points":   len(s.Points),
						"trending": trend(s.Points),
					})
				}
				return trimToolResult(sprintJSON(map[string]any{
					"query": query, "seriesCount": len(summary), "series": summary,
				})), nil
			},
		}
	}
	if deps.Loki != nil {
		r.tools["search_logs"] = Tool{
			Name: "search_logs", Description: "查询 Loki 容器日志（LogQL，只读）。返回脱敏后的日志行（时间戳 + 内容），用于排查错误与异常上下文。",
			JSONSchema: schema(map[string]any{
				"query":          strProp("LogQL 选择器/过滤器，如 {service=\"pay\"} |= \"error\"（可选——不填时按 service 前缀）"),
				"service":        strProp("服务名（可选，拼 {service=\"x\"} 前缀）"),
				"window_minutes": intProp("查询时间窗分钟（1-360，默认 30）", 1, maxWindowMinutes),
				"limit":          intProp("返回日志条数（1-100，默认 50）", 1, 100),
			}),
			ReadOnly: true, Timeout: 5 * time.Second,
			Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
				query := stringArg(in.Args, "query")
				service := stringArg(in.Args, "service")
				// 构造 LogQL：service 参数拼 {service="x"} 前缀，query 作为过滤器
				logql := query
				if service != "" {
					if logql == "" {
						logql = fmt.Sprintf(`{service=%q}`, service)
					} else if !startsWithSelector(logql) {
						logql = fmt.Sprintf(`{service=%q} %s`, service, logql)
					}
				}
				if logql == "" {
					return map[string]any{"error": "需要 query 或 service 参数"}, nil
				}
				if evidence.CheckQueryInjection(logql) {
					return map[string]any{"error": "查询疑似提示注入，已拒绝执行"}, nil
				}
				win := clampInt(intArg(in.Args, "window_minutes", 30), 1, maxWindowMinutes)
				limit := clampInt(intArg(in.Args, "limit", 50), 1, 100)
				now := time.Now().UTC()
				res, err := deps.Loki.QueryRange(ctx, logql, now.Add(-time.Duration(win)*time.Minute), now, limit)
				if err != nil {
					return map[string]any{"error": "loki 数据源不可达", "detail": truncateStr(err.Error(), 200)}, nil
				}
				entries := make([]map[string]any, 0, len(res.Entries))
				for _, e := range res.Entries {
					entries = append(entries, map[string]any{
						"timestamp": e.Timestamp.Format(time.RFC3339),
						"line":      truncateStr(evidence.RedactLogLine(e.Line), 200),
					})
				}
				return trimToolResult(sprintJSON(map[string]any{
					"query": logql, "count": len(entries), "entries": entries,
				})), nil
			},
		}
	}
}

// startsWithSelector 判断 LogQL 是否已是选择器开头（{...}）。
func startsWithSelector(q string) bool {
	return len(q) > 0 && q[0] == '{'
}

func maxOf(pts [][2]float64) float64 {
	m := pts[0][1]
	for _, p := range pts[1:] {
		if p[1] > m {
			m = p[1]
		}
	}
	return m
}

func minOf(pts [][2]float64) float64 {
	m := pts[0][1]
	for _, p := range pts[1:] {
		if p[1] < m {
			m = p[1]
		}
	}
	return m
}

// trend 趋势判定：末值相对首值变化（up/down/flat）。
func trend(pts [][2]float64) string {
	if len(pts) < 2 {
		return "flat"
	}
	first, last := pts[0][1], pts[len(pts)-1][1]
	if last > first*1.5 {
		return "up"
	}
	if last < first*0.5 {
		return "down"
	}
	return "flat"
}
