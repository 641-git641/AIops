package agent

import (
	"context"
	"math"
	"time"

	"aiops/internal/incident"
	"aiops/internal/playbook"
)

// ChangeAgent 变更风险 Agent：对自愈动作做 5 维加权风险评分并给出分级审批建议。
// 5 维权重：爆炸半径 0.30 / 操作风险 0.25 / 时间风险 0.20 / 历史 0.15 / 关键度 0.10，
// 评分分级 L0 自动 ≤0.3 / L1 oncall ≤0.5 / L2 tech-lead。审计四元组由审批环节落库。
type ChangeAgent struct{}

// NewChangeAgent 创建变更风险 Agent。
func NewChangeAgent() *ChangeAgent { return &ChangeAgent{} }

// Name Agent 名称。
func (c *ChangeAgent) Name() string { return "change" }

// weights 5 维风险权重。
var weights = map[string]float64{
	"blastRadius": 0.30,
	"operation":   0.25,
	"time":        0.20,
	"history":     0.15,
	"criticality": 0.10,
}

// Handle 对 heal 提议的自愈动作评分，写入 state.NodeResults["change"]。
// 依赖 heal 先行（编排表顺序保证）。
func (c *ChangeAgent) Handle(ctx context.Context, ctx2 *Context) error {
	st := ctx2.State
	heal := st.NodeResult("heal")
	action := ""
	if heal != nil {
		action, _ = heal["action"].(string)
	}
	if action == "" {
		action = "manual_followup" // heal 未产出动作时按人工跟进处理
	}

	severe := severityRank(ctx2.Alert.Severity)

	breakdown := map[string]float64{
		"blastRadius": blastRisk(heal),
		"operation":   operationRisk(action),
		"time":        timeRisk(),
		"history":     historyRisk(st.NodeResult("rca")),
		"criticality": criticalityRisk(severe),
	}
	score := 0.0
	for k, w := range weights {
		score += breakdown[k] * w
	}
	score = math.Min(1, math.Max(0, score))

	level, decision := classify(score)
	if level != playbook.LevelL0 {
		st.Status = incident.StatePendingApproval
	}

	st.SetNodeResult(c.Name(), map[string]any{
		"action": action, "riskScore": round2(score), "level": level,
		"decision": decision, "approvalRequired": level != playbook.LevelL0,
		"breakdown": breakdown,
		"weights":   weights,
		"service":   serviceOf(ctx2.Alert),
		"scoredAt":  time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

// classify 评分分级：L0 自动 ≤0.3 / L1 oncall ≤0.5 / L2 tech-lead。
func classify(score float64) (string, string) {
	switch {
	case score <= 0.3:
		return playbook.LevelL0, "auto_execute"
	case score <= 0.5:
		return playbook.LevelL1, "await_approval"
	default:
		return playbook.LevelL2, "await_approval"
	}
}

// blastRisk 从 heal 结果的爆炸半径映射风险（≤0.1 → 0.1；0.1~0.2 → 0.4；0.2~0.4 → 0.7；>0.4 → 1.0）。
func blastRisk(heal map[string]any) float64 {
	br, _ := heal["blastRadius"].(float64)
	switch {
	case br <= 0.1:
		return 0.1
	case br <= 0.2:
		return 0.4
	case br <= 0.4:
		return 0.7
	default:
		return 1.0
	}
}

// operationRisk 动作固有操作风险。
func operationRisk(action string) float64 {
	switch action {
	case "verify_dependency":
		return 0.1
	case "scale_up", "scale_down":
		return 0.2
	case "rollback":
		return 0.3
	case "restart_pod":
		return 0.4
	case "rate_limit":
		return 0.5
	case "manual_followup":
		return 0.6
	default:
		return 0.4
	}
}

// timeRisk 时间风险：MVP 固定低值（可在 Phase 3 按时间窗/变更窗口动态化）。
func timeRisk() float64 { return 0.2 }

// historyRisk 历史风险：RCA 输出的近期变更证据条数越多，越倾向"变更引发故障"。
func historyRisk(rca map[string]any) float64 {
	if rca == nil {
		return 0.1
	}
	changes, _ := rca["changes"].([]any)
	switch n := len(changes); {
	case n == 0:
		return 0.1
	case n <= 1:
		return 0.4
	default:
		return 0.8
	}
}

// severityRank 告警严重度排序（change 与 pipeline 各自持有，避免跨包依赖）。
func severityRank(s string) int {
	switch s {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

// criticalityRisk 告警关键度映射。
func criticalityRisk(rank int) float64 {
	switch rank {
	case 4: // critical
		return 1.0
	case 3: // high
		return 0.7
	case 2: // medium
		return 0.5
	default:
		return 0.3
	}
}
