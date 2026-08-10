package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"aiops/internal/ai"
	"aiops/internal/alert"
	"aiops/internal/evidence"
	"aiops/internal/incident"
	"aiops/internal/topology"
)

// RCALLM 可选 RCA 增强器（internal/ai 实现）：输入告警 + 确定性证据，返回自然语言根因/建议。
// 未配置/调用失败时返回 error，RCAAgent 降级为纯确定性结论（fail-open，不阻塞编排）。
type RCALLM interface {
	EnhanceRCA(ctx context.Context, in ai.RCAInput) (*ai.RCAOutput, error)
}

// RCAAgent 根因分析 Agent：基于服务拓扑 + 近期变更做确定性贝叶斯式推断。
// 知识图反向 BFS 找候选 + 证据规则打分（recent_change 0.9 /
// metric_anomaly 0.8）+ 置信度门槛。确定性证据链之外，可注入 RCALLM 补充自然语言根因
// 与建议动作（写入结果 llm* 字段）；未配置 LLM 或调用失败时保持结构化结论（fail-open）。
// 告警平台场景下根因事实来源有限，采用启发式证据规则，失败也返回结构化结论。
type RCAAgent struct {
	topo *topology.Service
	llm  RCALLM                      // 可选：nil 表示纯确定性
	ev   *evidence.EvidenceCollector // 可选：nil 表示未配置数据源证据（行为完全不变）
}

// NewRCAAgent 创建 RCA Agent。llm 可为 nil（未配置 LLM 时不增强）；ev 可为 nil（未配置数据源）。
func NewRCAAgent(topo *topology.Service, llm RCALLM, ev *evidence.EvidenceCollector) *RCAAgent {
	return &RCAAgent{topo: topo, llm: llm, ev: ev}
}

// Name Agent 名称。
func (r *RCAAgent) Name() string { return "rca" }

// Handle 输出根因候选与置信度，写入 state.NodeResults["rca"]。
func (r *RCAAgent) Handle(ctx context.Context, c *Context) error {
	a := c.Alert
	svc := serviceOf(a)

	result := map[string]any{
		"service":          svc,
		"confidence":       0.0,
		"rootCause":        "unknown",
		"evidence":         []string{},
		"paths":            []string{},
		"changes":          []string{},
		"suggestedActions": []string{},
		"analyzedAt":       time.Now().UTC().Format(time.RFC3339),
	}

	switch {
	case svc == "":
		// 无法定位服务：输出最保守结论，等待人工介入
		result["rootCause"] = "service_not_found"
		result["evidence"] = []string{"告警标签中未携带 service 信息，无法定位受影响服务"}
		result["suggestedActions"] = []string{"检查 provider 配置，确保告警携带 service 标签"}
	case r.topo == nil:
		result["rootCause"] = "no_topology"
		result["evidence"] = []string{"拓扑未配置，无法做依赖链分析"}
		result["suggestedActions"] = []string{"导入服务拓扑，提升根因判定能力"}
	default:
		r.collect(ctx, c, svc, result)
	}

	c.State.SetNodeResult(r.Name(), result)
	return nil
}

// collect 执行证据收集与根因判定。
func (r *RCAAgent) collect(ctx context.Context, c *Context, svc string, result map[string]any) {
	a := c.Alert

	// 证据 1：依赖链（反向 BFS 路径，用于展示扩散范围）
	if paths, err := r.topo.DependencyPaths(ctx, a.TenantID, svc, c.Cfg.MaxDepth); err == nil {
		result["paths"] = paths
	}

	// 证据 2：近期变更（变更是最常见根因信号）
	changes, err := r.topo.RecentChanges(ctx, a.TenantID, svc)
	if err != nil {
		result["evidence"] = []string{fmt.Sprintf("查询近期变更失败：%v", err)}
		result["rootCause"] = "analysis_error"
		result["suggestedActions"] = []string{"人工复核服务拓扑与变更记录"}
		return
	}
	result["changes"] = changes

	evidence, _ := result["evidence"].([]string)
	switch {
	case len(changes) > 0:
		result["rootCause"] = "recent_change"
		result["confidence"] = 0.54
		evidence = append(evidence,
			fmt.Sprintf("受影响服务存在近期变更：%s", strings.Join(changes, "、")))
		result["evidence"] = evidence
		result["suggestedActions"] = []string{"rollback", "profiling", "verify_dependency"}
	default:
		// 无变更证据：倾向资源/负载类问题（可叠加指标异常证据，Phase 2）
		result["rootCause"] = "resource_exhaustion"
		result["confidence"] = 0.35
		evidence = append(evidence,
			"未发现近期变更，判定为资源/负载类问题（需结合监控指标确认）")
		result["evidence"] = evidence
		result["suggestedActions"] = []string{"scale_up", "rate_limit"}
	}

	// 告警严重度提升置信度：critical 作为强先验叠加
	if a.Severity == "critical" {
		result["confidence"] = result["confidence"].(float64) + 0.15
	}

	// 证据 3：数据源证据（Prometheus 指标 / Loki 日志，可选；失败仅追加降级说明句子）
	if r.ev != nil {
		evidence, _ := result["evidence"].([]string)
		evidence = append(evidence, r.ev.Collect(ctx, svc, a.StartedAt)...)
		result["evidence"] = evidence
	}

	// LLM 增强：确定性证据链之外补充自然语言根因/建议（可选，失败不阻塞）
	if r.llm != nil {
		r.enhance(ctx, c, svc, result)
	}

	c.State.Status = incident.StateInvestigating
}

// enhance 用 LLM 补充自然语言根因分析：输入告警 + 确定性证据，结论写入节点结果 llm* 字段。
// 未配置 apiKey / 网络失败 / 输出无法解析时仅记日志，保留确定性结论（fail-open）。
func (r *RCAAgent) enhance(ctx context.Context, c *Context, svc string, result map[string]any) {
	a := c.Alert
	in := ai.RCAInput{
		Service:   svc,
		AlertName: a.Name,
		Severity:  a.Severity,
		Summary:   summaryOf(a),
		Labels:    labelsOf(a),
	}
	in.RootCause, _ = result["rootCause"].(string)
	if f, ok := result["confidence"].(float64); ok {
		in.Confidence = f
	}
	in.Evidence, _ = result["evidence"].([]string)
	in.Changes, _ = result["changes"].([]string)
	in.Paths, _ = result["paths"].([]string)

	out, err := r.llm.EnhanceRCA(ctx, in)
	if err != nil {
		slog.Warn("rca: llm enhance skipped (keep deterministic)", "err", err)
		return
	}
	if out == nil || out.RootCause == "" {
		return
	}
	result["llmRootCause"] = out.RootCause
	result["llmRationale"] = out.Rationale
	result["llmSuggestedActions"] = out.SuggestedActions
	result["llmAnalyzedAt"] = time.Now().UTC().Format(time.RFC3339)
}

// summaryOf 从 Event.annotations.summary 提取人类可读摘要（缺失返回空）。
func summaryOf(a *alert.Alert) string {
	if a == nil {
		return ""
	}
	var ev map[string]any
	if err := json.Unmarshal(a.Event, &ev); err != nil {
		return ""
	}
	ann, _ := ev["annotations"].(map[string]any)
	if s, ok := ann["summary"].(string); ok && s != "" {
		return s
	}
	return ""
}

// labelsOf 提取告警标签为 string map（供 LLM 输入）。
func labelsOf(a *alert.Alert) map[string]string {
	if a == nil {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(a.Labels, &raw); err != nil {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}
