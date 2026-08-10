package assistant

import (
	"context"
	"errors"
	"math"
	"time"

	"aiops/internal/rag"

	"gorm.io/gorm"
)

// tools_knowledge.go：知识/剧本类只读工具（AGENT_CHAT_PLAN §5.3 Phase 2）。
// - search_knowledge / get_similar_incidents 依赖 rag.Service（RAG 关闭时不注册，
//   工具列表里根本不出现——模型无法调用，聊天仍可用领域只读工具回答）；
// - list_playbooks 依赖 playbook.Service（未装配时不注册）。
// 知识内容一律视为不可信数据（防提示注入，AGENT_CHAT_PLAN §9.4）；
// 命中证据同时产出 Citation（citation.added 事件 + 最终消息引用卡）。

// registerKnowledgeTools 注册知识类工具（依赖缺失的工具不注册）。
func registerKnowledgeTools(r *Registry, deps ToolDeps) {
	if deps.RAG != nil {
		registerSearchKnowledge(r, deps)
		registerSimilarIncidents(r, deps)
	}
	if deps.Playbooks != nil {
		registerListPlaybooks(r, deps)
	}
}

// registerSearchKnowledge 通用知识检索（AGENT_CHAT_PLAN §8.2：用户查询先脱敏再混合检索）。
func registerSearchKnowledge(r *Registry, deps ToolDeps) {
	r.tools["search_knowledge"] = Tool{
		Name: "search_knowledge", Description: "检索知识库（已解决事故、剧本、runbook 知识源）。按相关度返回带来源与可信等级的证据，回答事实性结论必须引用。",
		JSONSchema: schema(map[string]any{
			"query":   strProp("检索问题（必填）"),
			"service": strProp("按服务过滤（可选）"),
			"limit":   intProp("返回条数（1-10，默认 5）", 1, 10),
		}, "query"),
		ReadOnly: true, Timeout: 3 * time.Second,
		Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
			query := stringArg(in.Args, "query")
			pack := deps.RAG.SearchKnowledge(ctx, in.TenantID, query, rag.KnowledgeSearchFilters{
				Service: stringArg(in.Args, "service"),
				Limit:   clampInt(intArg(in.Args, "limit", 5), 1, 10),
			})
			return evidenceResult(in, query, pack), nil
		},
	}
}

// registerSimilarIncidents 相似事故检索（事故驱动查询：服务/告警名/错误码/摘要三路召回，排除自身）。
func registerSimilarIncidents(r *Registry, deps ToolDeps) {
	r.tools["get_similar_incidents"] = Tool{
		Name: "get_similar_incidents", Description: "检索与该事故相似的历史已解决事故与知识。基于事故的服务、告警名、错误码与摘要混合召回，返回带可信等级的证据。",
		JSONSchema: schema(map[string]any{
			"incident_id": strProp("事故 ID（必填）"),
			"limit":       intProp("返回条数（1-10，默认 5）", 1, 10),
		}, "incident_id"),
		ReadOnly: true, Timeout: 3 * time.Second,
		Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
			incID := stringArg(in.Args, "incident_id")
			inc, err := deps.Incidents.Get(ctx, in.TenantID, incID)
			// 注意：incident.Service.Get 对不存在返回 gorm.ErrRecordNotFound（与 alert repo 的
			// nil,nil 语义不同）——按「事故不存在」降级结果处理，不把工具标记失败。
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return map[string]any{"error": "事故不存在"}, nil
			}
			if err != nil {
				return nil, err
			}
			if inc == nil {
				return map[string]any{"error": "事故不存在"}, nil
			}
			alerts, err := deps.Incidents.AlertsOf(ctx, in.TenantID, incID)
			if err != nil {
				alerts = nil
			}
			names := make([]string, 0, len(alerts))
			labels := map[string]string{}
			for _, a := range alerts {
				names = append(names, a.Name)
				if len(labels) == 0 {
					for k, v := range a.ToDTO().Labels {
						if s, ok := v.(string); ok {
							labels[k] = s
						}
					}
				}
			}
			q := rag.BuildQuery(in.TenantID, incID, inc.Severity, inc.Summary, "", names, labels, inc.CreatedAt)
			pack := deps.RAG.Retrieve(ctx, q)
			return evidenceResult(in, "similar-to:"+incID, pack), nil
		},
	}
}

// registerListPlaybooks 剧本元数据（只读：不触发执行，不展示命令细节以外的敏感模板）。
func registerListPlaybooks(r *Registry, deps ToolDeps) {
	r.tools["list_playbooks"] = Tool{
		Name: "list_playbooks", Description: "查询可用的自愈剧本（名称/级别/命令模板/爆炸半径/是否强制 dry-run）。只返回元数据，不执行任何动作。",
		JSONSchema: schema(map[string]any{
			"limit": intProp("返回条数（1-20，默认 10）", 1, 20),
		}),
		ReadOnly: true, Timeout: 3 * time.Second,
		Call: func(ctx context.Context, in ToolCallCtx) (any, error) {
			pbs, err := deps.Playbooks.List(ctx, in.TenantID)
			if err != nil {
				return nil, err
			}
			limit := clampInt(intArg(in.Args, "limit", 10), 1, 20)
			out := make([]map[string]any, 0, len(pbs))
			for _, pb := range pbs {
				if !pb.Enabled {
					continue
				}
				out = append(out, map[string]any{
					"name": pb.Name, "level": pb.Level, "commandTemplate": pb.CommandTemplate,
					"blastRadiusFactor": pb.BlastRadiusFactor, "dryRunFirst": pb.DryRunFirst,
					"rollback": pb.RollbackCommand,
				})
				if len(out) >= limit {
					break
				}
			}
			return trimToolResult(sprintJSON(map[string]any{"count": len(out), "playbooks": out})), nil
		},
	}
}

// evidenceResult 证据包 → 工具结果（模型可读）+ Citation 收集（前端引用卡）。
// 降级原因（degradedReason）透出：模型回答可提示降级，UI 引用区展示。
func evidenceResult(in ToolCallCtx, label string, pack *rag.EvidencePack) string {
	out := make([]map[string]any, 0, len(pack.Selected))
	for _, e := range pack.Selected {
		out = append(out, map[string]any{
			"documentId": e.DocumentID, "sourceType": e.SourceType, "sourceId": e.SourceID,
			"title": e.Title, "section": e.Section, "trustLevel": e.TrustLevel,
			"time": e.Time.Format(time.RFC3339), "score": round3(e.Score), "reasons": e.Reasons,
			"content": truncateStr(e.Content, 200),
		})
		in.addCitations(citationOf(e))
	}
	return trimToolResult(sprintJSON(map[string]any{
		"query": label, "count": len(out), "degradedReason": pack.DegradedReason, "results": out,
	}))
}

// citationOf 证据 → 引用（仅可定位来源生成跳转 URL；其余靠前端按 sourceType 兜底路由）。
func citationOf(e rag.Evidence) Citation {
	c := Citation{
		SourceType: e.SourceType, SourceID: e.SourceID, Title: e.Title,
		TrustLevel: e.TrustLevel, Time: e.Time.Format(time.RFC3339),
	}
	switch e.SourceType {
	case rag.SourceIncident:
		c.URL = "/incidents/" + e.SourceID
	case rag.SourcePlaybook:
		c.URL = "/playbooks"
	}
	return c
}

// round3 保留 3 位小数（证据分数展示）。
func round3(f float64) float64 {
	return math.Round(f*1000) / 1000
}
