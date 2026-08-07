package workflow

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Def 工作流结构化定义（YAML → 内存对象，keep 式 schema 的精简版）。
type Def struct {
	Workflow DefWorkflow `yaml:"workflow"`
}

// DefWorkflow 工作流本体。triggers 决定触发时机，steps 先执行（富化/记录），actions 后执行（处置）。
type DefWorkflow struct {
	ID          string    `yaml:"id"`
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Disabled    bool      `yaml:"disabled"`
	Triggers    []Trigger `yaml:"triggers"`
	Steps       []Step    `yaml:"steps"`
	Actions     []Step    `yaml:"actions"`
}

// Trigger 触发器。type: alert（告警事件，filters 全部满足才命中）| interval（定时触发，with.interval 周期）| manual（仅手动触发）。
type Trigger struct {
	Type    string         `yaml:"type"`
	Filters []Filter       `yaml:"filters"`
	With    map[string]any `yaml:"with"` // interval: {"interval": "5m"}（Go duration 或纯数字秒）
}

// Interval 返回 interval 触发器的周期。with.interval 支持 Go duration（"5m"/"1h"）或纯数字秒。
func (t Trigger) Interval() (time.Duration, bool) {
	if t.Type != "interval" {
		return 0, false
	}
	v, ok := t.With["interval"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case string:
		d, err := time.ParseDuration(n)
		if err == nil && d > 0 {
			return d, true
		}
	case int:
		if n > 0 {
			return time.Duration(n) * time.Second, true
		}
	case float64:
		if n > 0 {
			return time.Duration(n) * time.Second, true
		}
	}
	return 0, false
}

// Filter 触发过滤条件。key: severity|service|name|provider|任意 labels 键；value 精确匹配，severity 支持 >=/<=。
type Filter struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"`
}

// Step 步骤（steps 与 actions 同一结构，执行顺序即声明顺序）。
type Step struct {
	Name     string   `yaml:"name"`
	Provider Provider `yaml:"provider"`
}

// Provider 步骤执行者。type: log|http|agent；with 为步骤参数。
type Provider struct {
	Type string         `yaml:"type"`
	With map[string]any `yaml:"with"`
}

// Parse 解析并校验 YAML 工作流定义。
func Parse(src []byte) (*Def, error) {
	var def Def
	if err := yaml.Unmarshal(src, &def); err != nil {
		return nil, fmt.Errorf("workflow: parse yaml: %w", err)
	}
	if def.Workflow.ID == "" {
		return nil, fmt.Errorf("workflow: missing workflow.id")
	}
	if len(def.Workflow.Triggers) == 0 {
		return nil, fmt.Errorf("workflow: at least one trigger required")
	}
	for _, tr := range def.Workflow.Triggers {
		switch tr.Type {
		case "", "alert", "manual":
		case "interval":
			if _, ok := tr.Interval(); !ok {
				return nil, fmt.Errorf("workflow: interval trigger missing with.interval (e.g. \"5m\")")
			}
		default:
			return nil, fmt.Errorf("workflow: unsupported trigger type %q", tr.Type)
		}
	}
	all := make([]Step, 0, len(def.Workflow.Steps)+len(def.Workflow.Actions))
	all = append(all, def.Workflow.Steps...)
	all = append(all, def.Workflow.Actions...)
	for _, s := range all {
		if s.Name == "" {
			return nil, fmt.Errorf("workflow: step/action missing name")
		}
		if s.Provider.Type == "" {
			return nil, fmt.Errorf("workflow: step %q missing provider.type", s.Name)
		}
	}
	return &def, nil
}

// defaultWorkflowYAML 内置演示工作流：medium 的 order 告警默认不进 Agent 路径，
// 通过本工作流显式接入编排器（monitor→rca→heal→change），并演示 log 富化步骤。
const defaultWorkflowYAML = `workflow:
  id: order-medium-agent-demo
  name: 订单中危告警自动接入 Agent 编排
  description: 内置演示：medium 的 order 告警默认不进 Agent 路径，通过工作流显式接入编排器
  triggers:
    - type: alert
      filters:
        - key: service
          value: order
        - key: severity
          value: ">=medium"
  steps:
    - name: enrich
      provider:
        type: log
        with:
          message: "工作流命中 order 告警 {{.alert.name}}，即将接入 Agent 编排"
  actions:
    - name: run-agent
      provider:
        type: agent
`
