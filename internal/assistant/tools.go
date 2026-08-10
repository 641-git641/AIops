package assistant

import (
	"context"
	"encoding/json"
	"time"

	"aiops/internal/alert"
	"aiops/internal/evidence"
	"aiops/internal/incident"
	"aiops/internal/llm"
	"aiops/internal/overview"
	"aiops/internal/playbook"
	"aiops/internal/rag"
	"aiops/internal/topology"
)

// tools.go：工具注册表（AGENT_CHAT_PLAN §5.3）。
// 工具直接调用领域 Service（不走本机 HTTP 回调自身）；每个工具声明输入
// JSON Schema、只读标记、超时与结果裁剪。模型只能调用注册表内的工具。

// ToolDeps 工具依赖的领域服务（全部必装配；主服务启动时注入）。
type ToolDeps struct {
	Overview  *overview.Service
	Alerts    *alert.Repository
	Incidents *incident.Service
	Topology  *topology.Service
	// 知识检索（Phase 2；nil = RAG 关闭 → search_knowledge/get_similar_incidents 不注册）
	RAG *rag.Service
	// 剧本元数据（nil = 未装配 → list_playbooks 不注册）
	Playbooks *playbook.Service
	// 数据源证据（可选；nil = 未配置数据源 → 对应工具不注册）
	Prometheus *evidence.PromClient
	Loki       *evidence.LokiClient
}

// ToolCallCtx 单次工具调用的上下文。
type ToolCallCtx struct {
	TenantID string
	RunID    string
	StepID   string
	Args     map[string]any // 已通过 schema 校验
	// Citations 工具结果命中的证据引用收集器（Phase 2：由 runner 提供；
	// 工具可 append——命中证据的检索类工具填写，非检索工具不填）。
	Citations *[]Citation
}

// addCitations 追加引用（收集器为空时忽略——工具单测可不传）。
func (in ToolCallCtx) addCitations(cs ...Citation) {
	if in.Citations == nil {
		return
	}
	*in.Citations = append(*in.Citations, cs...)
}

// Tool 工具定义。
type Tool struct {
	Name        string
	Description string
	JSONSchema  map[string]any
	ReadOnly    bool // true = 直接执行返回结果；false = 动作工具（Phase 3：只提出确认请求，不执行）
	// Action 动作规范（非 nil = 动作工具）：模型只能提出请求，执行在确认路径（Service 执行器）。
	Action *ActionSpec
	Timeout time.Duration
	Call    func(ctx context.Context, in ToolCallCtx) (any, error) // 只读工具用；动作工具为 nil
}

// ActionSpec 动作工具规范（§9.2 动作隔离：提出时校验、确认时重验）。
type ActionSpec struct {
	RiskLevel string                                            // low | medium | high（确认卡展示）
	Validate  func(ctx context.Context, in ToolCallCtx) error   // 提出时的存在性/合法性预检
}

// Registry 工具注册表。
type Registry struct {
	tools map[string]Tool
}

// NewRegistry 构建注册表并注册全部只读工具。
func NewRegistry(deps ToolDeps) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	registerReadTools(r, deps)
	registerKnowledgeTools(r, deps) // Phase 2：RAG/playbook 依赖缺失的工具不注册
	registerActionTools(r, deps)    // Phase 3：人工确认动作工具（依赖缺失不注册）
	return r
}

// Get 按名取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// ToLLMTools 转模型 tools 参数（JSON Schema 原样透传）。
func (r *Registry) ToLLMTools() []llm.Tool {
	out := make([]llm.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		schema, _ := json.Marshal(t.JSONSchema)
		out = append(out, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name: t.Name, Description: t.Description, Parameters: schema,
			},
		})
	}
	return out
}

// stringArg 安全取字符串参数（缺失返回 ""）。
func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

// intArg 安全取整数参数（缺失返回 def）。
func intArg(args map[string]any, key string, def int) int {
	f, ok := args[key].(float64)
	if !ok {
		return def
	}
	return int(f)
}

// schema 通用构造。
func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func enumProp(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}

func intProp(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}
