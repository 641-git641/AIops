package assistant

import "aiops/internal/metrics"

// metrics.go：对话式 Agent 指标（AGENT_CHAT_PLAN §12 清单，本期注册 7 个）。
// 全部埋点判空（SetMetrics 可 nil——测试/未装配路径）。

// 指标名（main.go 装配段注册，与命名约定 aiops_<域>_<名词>_total 对齐）。
const (
	metricRunsTotal         = "aiops_assistant_runs_total"
	metricRunDuration       = "aiops_assistant_run_duration_seconds"
	metricModelDuration     = "aiops_assistant_model_duration_seconds"
	metricToolCallsTotal    = "aiops_assistant_tool_calls_total"
	metricToolDuration      = "aiops_assistant_tool_duration_seconds"
	metricActiveRuns        = "aiops_assistant_active_runs"
	metricTokensTotal       = "aiops_assistant_tokens_total"
	metricCitationsTotal    = "aiops_assistant_citations_total"
	metricFeedbackTotal     = "aiops_assistant_feedback_total"
	metricActionRequestsTotal = "aiops_assistant_action_requests_total"
)

// RegisterMetrics 注册 assistant 指标（main.go 调用）。
func RegisterMetrics(reg *metrics.Registry, active func() float64) {
	if reg == nil {
		return
	}
	reg.Counter(metricRunsTotal, "对话 Agent 运行结算（status=completed|failed|cancelled）", "status", "model")
	reg.Histogram(metricRunDuration, "单 run 总耗时（秒）", metrics.SecondsBuckets)
	reg.Histogram(metricModelDuration, "单次模型调用耗时（秒）", metrics.SecondsBuckets)
	reg.Counter(metricToolCallsTotal, "工具调用结算（outcome=ok|rejected|failed|unknown）", "tool", "outcome")
	reg.Histogram(metricToolDuration, "单工具执行耗时（秒）", metrics.SecondsBuckets)
	reg.Gauge(metricActiveRuns, "当前运行中的 run 数（gauge 采集时求值）", active)
	reg.Counter(metricTokensTotal, "模型 token 用量", "direction", "model")
	reg.Counter(metricCitationsTotal, "回答引用（source_type=incident|playbook|runbook|knowledge）", "source_type")
	reg.Counter(metricFeedbackTotal, "消息反馈（rating=useful|not_useful）", "rating")
	reg.Counter(metricActionRequestsTotal, "动作请求结算（decision=proposed|confirm|reject，outcome=状态）", "tool", "decision", "outcome")
}

// incAction 动作提出侧指标（runner 调用；确认侧在 service）。
func (r *Runner) incAction(tool, decision, outcome string) {
	if r.m != nil {
		r.m.Inc(metricActionRequestsTotal, tool, decision, outcome)
	}
}

func (r *Runner) incRuns(status string) {
	if r.m != nil {
		r.m.Inc(metricRunsTotal, status, r.modelID())
	}
}

func (r *Runner) observeRun(sec float64) {
	if r.m != nil {
		r.m.Observe(metricRunDuration, sec)
	}
}

func (r *Runner) incModel(outcome string, sec float64) {
	if r.m != nil {
		r.m.Observe(metricModelDuration, sec)
		r.m.Inc(metricTokensTotal, "completion", r.modelID())
	}
}

func (r *Runner) incTool(tool, outcome string, sec float64) {
	if r.m != nil {
		r.m.Inc(metricToolCallsTotal, tool, outcome)
		r.m.Observe(metricToolDuration, sec)
	}
}

func (r *Runner) incCitations(sourceType string) {
	if r.m != nil {
		r.m.Inc(metricCitationsTotal, sourceType)
	}
}
