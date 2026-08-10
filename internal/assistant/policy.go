package assistant

import (
	"fmt"
	"time"
)

// policy.go：Agent 循环硬限制与工具参数校验（AGENT_CHAT_PLAN §5.2/§9.4）。
// 超限、超时、模型失败时输出明确的降级回答，并保留已取得的确定性事实。

// Limits 单 run 硬限制（来自 AIOPS_ASSISTANT* 配置，装配时钳制）。
type Limits struct {
	MaxConcurrency int           // 并发 run worker 数（默认 4）
	MaxModelRounds int           // 最多模型回合（默认 8）
	MaxToolCalls   int           // 最多工具调用（默认 12）
	RunTimeout     time.Duration // 单 run 总超时（默认 60s；ctx 取消传播）
	ToolTimeout    time.Duration // 单工具执行超时（默认 3s）
}

// MaxResultBytes 工具结果裁剪上限（rune 安全；写入 steps/回喂模型的摘要 ≤ 此值）。
const MaxResultBytes = 8 << 10

// validateToolArgs 工具参数二次校验（模型生成的参数不可信）：
// 白名单字段 + 类型（string/number/integer/boolean）+ 枚举 + 必填。
// 拒绝未知字段与类型错误——模型不能绕过工具定义注入任意参数。
func validateToolArgs(schema map[string]any, args map[string]any) error {
	props, _ := schema["properties"].(map[string]any)
	requiredSet := map[string]bool{}
	for _, r := range toStrings(schema["required"]) {
		requiredSet[r] = true
	}
	// 未知字段拒绝
	for k := range args {
		if _, ok := props[k]; !ok {
			return fmt.Errorf("tool args: unknown field %q", k)
		}
	}
	// 必填缺失
	for k := range requiredSet {
		if _, ok := args[k]; !ok {
			return fmt.Errorf("tool args: missing required field %q", k)
		}
	}
	// 类型 + 枚举
	for k, spec := range props {
		prop, _ := spec.(map[string]any)
		v, present := args[k]
		if !present {
			continue
		}
		typ, _ := prop["type"].(string)
		switch typ {
		case "string":
			if _, ok := v.(string); !ok {
				return fmt.Errorf("tool args: field %q 期望 string，got %T", k, v)
			}
		case "integer":
			if !isInt(v) {
				return fmt.Errorf("tool args: field %q 期望 integer，got %T", k, v)
			}
		case "number":
			switch v.(type) {
			case float64, float32, int, int64:
			default:
				return fmt.Errorf("tool args: field %q 期望 number，got %T", k, v)
			}
		case "boolean":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("tool args: field %q 期望 boolean，got %T", k, v)
			}
		}
		enum := toStrings(prop["enum"])
		if len(enum) > 0 {
			matched := false
			for _, e := range enum {
				if fmt.Sprint(e) == fmt.Sprint(v) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("tool args: field %q 值 %v 不在枚举内", k, v)
			}
		}
	}
	return nil
}

// isInt JSON 数值（float64）或 Go 整数且无小数部分。
func isInt(v any) bool {
	switch x := v.(type) {
	case int:
		return true
	case int64:
		return true
	case float64:
		return x == float64(int64(x))
	case float32:
		return x == float32(int32(x))
	}
	return false
}

// toStrings 兼容 []string 与 []any 的字符串切片读取。
func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

// clampInt 钳制 int 到 [min, max]。
func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
