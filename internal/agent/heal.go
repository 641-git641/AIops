package agent

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"text/template"
	"time"

	"aiops/internal/circuit"
	"aiops/internal/incident"
	"aiops/internal/playbook"
	"aiops/internal/topology"
)

// playbookFinder 剧本匹配（由 *playbook.Service 实现，接口化便于测试注入）。
type playbookFinder interface {
	Resolve(ctx context.Context, tenantID string, suggested []string) (*playbook.Playbook, error)
}

// topoGetter 拓扑节点查询（由 *topology.Service 实现）。
type topoGetter interface {
	Get(ctx context.Context, tenantID, name string) (*topology.TopologyService, error)
}

// HealAgent 自愈 Agent：依据 RCA 根因建议动作 → 匹配 Playbook → 计算爆炸半径 → 分级审批。
// 参考 multi-agent-aiops 安全护栏链：dry-run → 爆炸半径（超限强制降级 L2）→ 熔断器 → L0/L1/L2 分级。
// 默认 dry_run=true，故结果通常为 status=dry_run + 需要审批（演示审批闭环）。
type HealAgent struct {
	pb   playbookFinder
	topo topoGetter
	br   *circuit.Breaker
}

// NewHealAgent 创建自愈 Agent。topo/br 可为 nil（弱化护栏，测试或纯规则场景）。
func NewHealAgent(pb playbookFinder, topo topoGetter, br *circuit.Breaker) *HealAgent {
	return &HealAgent{pb: pb, topo: topo, br: br}
}

// Name Agent 名称。
func (h *HealAgent) Name() string { return "heal" }

// Handle 制定并门控自愈动作，结果写入 state.NodeResults["heal"]。
func (h *HealAgent) Handle(ctx context.Context, c *Context) error {
	st := c.State
	st.Status = incident.StateHealing

	// 1. 取 RCA 建议动作
	actions := suggestedActions(st.NodeResult("rca"))
	if len(actions) == 0 {
		st.SetNodeResult(h.Name(), map[string]any{
			"status": "no_suggested_action", "action": "", "level": "",
			"reason":     "RCA 未给出可执行建议，跳过自愈",
			"proposedAt": time.Now().UTC().Format(time.RFC3339),
		})
		return nil
	}

	// 2. 匹配剧本（未配置时自动补种默认剧本，零配置可演示）
	pb, err := h.pb.Resolve(ctx, st.TenantID, actions)
	if err != nil {
		st.SetNodeResult(h.Name(), map[string]any{
			"status": "no_playbook", "action": actions[0], "level": "",
			"reason":     fmt.Sprintf("无可用剧本，请先在剧本管理中配置：%v", err),
			"proposedAt": time.Now().UTC().Format(time.RFC3339),
		})
		return nil
	}
	if !pb.Enabled {
		st.SetNodeResult(h.Name(), map[string]any{
			"status": "disabled", "action": pb.Name, "level": pb.Level,
			"reason": "剧本已禁用", "proposedAt": time.Now().UTC().Format(time.RFC3339),
		})
		return nil
	}

	// 3. 爆炸半径：剧本固有 + 拓扑影响分数放大（高依赖节点波及面更大）
	blast := pb.BlastRadiusFactor
	if h.topo != nil {
		if t, err := h.topo.Get(ctx, st.TenantID, serviceOf(c.Alert)); err == nil && t != nil && t.ImpactScore > 0 {
			blast = math.Min(1.0, blast+t.ImpactScore*0.2)
		}
	}

	// 4. 级别判定：剧本级别；爆炸半径超限强制降级 L2（fail-safe）
	level := pb.Level
	downgraded := blast > c.Cfg.BlastRadiusLimit && level != playbook.LevelL2
	if downgraded {
		level = playbook.LevelL2
	}

	// 5. 审批门控：dry-run 或 高于自动级别 → 需审批（进入 pending_approval，等待人工决策）
	approvalRequired := c.Cfg.DryRun || levelRank(level) > levelRank(c.Cfg.AutoHealLevel)
	if approvalRequired {
		st.Status = incident.StatePendingApproval
	}

	status := "proposed"
	executedAt := ""
	if c.Cfg.DryRun {
		status = "dry_run" // 仅演练，不执行
	} else if !approvalRequired {
		// 自动执行路径：熔断器放行才执行，否则阻断
		if h.br != nil && !h.br.Allow() {
			st.SetNodeResult(h.Name(), map[string]any{
				"status": "blocked", "action": pb.Name, "level": level,
				"blastRadius": round2(blast), "downgraded": downgraded,
				"approvalRequired": false,
				"reason":           fmt.Sprintf("熔断器开启（%s），动作被阻断", h.br.State()),
				"command":          renderCommand(pb.CommandTemplate, serviceOf(c.Alert)),
				"proposedAt":       time.Now().UTC().Format(time.RFC3339),
			})
			return nil
		}
		status = "executed"
		executedAt = time.Now().UTC().Format(time.RFC3339)
		if h.br != nil {
			h.br.Success()
		}
	} else {
		status = "awaiting_approval"
	}

	svc := serviceOf(c.Alert)
	st.SetNodeResult(h.Name(), map[string]any{
		"status": status, "action": pb.Name, "level": level,
		"blastRadius": round2(blast), "downgraded": downgraded,
		"approvalRequired": approvalRequired, "dryRun": c.Cfg.DryRun,
		"playbookId": pb.ID, "command": renderCommand(pb.CommandTemplate, svc),
		"rollbackCommand": renderCommand(pb.RollbackCommand, svc),
		"reason":          reasonOf(actions),
		"executedAt":      executedAt,
		"proposedAt":      time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

// suggestedActions 从 RCA 结果提取建议动作列表。
// LLM 建议动作回填：确定性 suggestedActions 优先，随后并入 llmSuggestedActions（去重，保留顺序）。
// 让 Heal 能基于 LLM 补充的自然语言建议做剧本匹配（回填 Heal），同时不改变确定性主选。
func suggestedActions(rca map[string]any) []string {
	if rca == nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, key := range []string{"suggestedActions", "llmSuggestedActions"} {
		raw, ok := rca[key].([]any)
		if !ok {
			continue
		}
		for _, v := range raw {
			s, ok := v.(string)
			if !ok || s == "" {
				continue
			}
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// reasonOf 动作选择说明（首个动作为主选）。
func reasonOf(actions []string) string {
	if len(actions) == 0 {
		return ""
	}
	return "依据 RCA 建议动作优先匹配：" + strings.Join(actions, "、")
}

// levelRank 自愈级别排序（L0 最安全）。
func levelRank(level string) int {
	switch level {
	case playbook.LevelL0:
		return 0
	case playbook.LevelL1:
		return 1
	case playbook.LevelL2:
		return 2
	}
	return 2
}

// renderCommand 渲染命令模板（{{.service}} 等）。
func renderCommand(tmpl, service string) string {
	if tmpl == "" {
		return ""
	}
	t, err := template.New("cmd").Parse(tmpl)
	if err != nil {
		return tmpl
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, map[string]any{"service": service}); err != nil {
		return tmpl
	}
	return buf.String()
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
