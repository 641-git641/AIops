// Package cel 封装 google/cel-go：构建环境、表达式预编译缓存、布尔求值。
// 提供 keep 式"一处表达式处处复用"的基础：规则匹配、Agent 条件路由、告警过滤。
package cel

import (
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// Engine 持有编译环境与表达式缓存。
type Engine struct {
	env   *cel.Env
	cache sync.Map // expr string -> cel.Program
}

// New 构建 CEL 引擎。声明统一的告警上下文变量：
//
//	alert  map<string, dyn>  告警结构化字段（severity/provider/name/status/labels）
//	event  map<string, dyn>  原始事件快照
//	labels map<string, dyn>  标签
func New() (*Engine, error) {
	decls := []cel.EnvOption{
		cel.Variable("alert", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("labels", cel.MapType(cel.StringType, cel.DynType)),
	}
	env, err := cel.NewEnv(decls...)
	if err != nil {
		return nil, fmt.Errorf("cel: build env: %w", err)
	}
	return &Engine{env: env}, nil
}

// program 获取（或编译缓存）表达式的可执行程序。
func (e *Engine) program(expr string) (cel.Program, error) {
	if v, ok := e.cache.Load(expr); ok {
		return v.(cel.Program), nil
	}
	ast, iss := e.env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("cel: compile %q: %w", expr, iss.Err())
	}
	prg, err := e.env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("cel: program %q: %w", expr, err)
	}
	e.cache.Store(expr, prg)
	return prg, nil
}

// EvalBool 求值表达式并转布尔（规则/条件用）。
// vars 是告警上下文，如 {"alert": {...}, "event": {...}}。
func (e *Engine) EvalBool(expr string, vars map[string]any) (bool, error) {
	prg, err := e.program(expr)
	if err != nil {
		return false, err
	}
	out, _, err := prg.Eval(vars)
	if err != nil {
		return false, err
	}
	return refToBool(out)
}

// Eval 求值表达式并返回原始值（过滤/透传用）。
func (e *Engine) Eval(expr string, vars map[string]any) (any, error) {
	prg, err := e.program(expr)
	if err != nil {
		return nil, err
	}
	out, _, err := prg.Eval(vars)
	if err != nil {
		return nil, err
	}
	return refValue(out), nil
}

func refToBool(v ref.Val) (bool, error) {
	if v == nil {
		return false, fmt.Errorf("cel: nil result")
	}
	b, ok := v.Value().(bool)
	if !ok {
		return false, fmt.Errorf("cel: expected bool, got %T", v.Value())
	}
	return b, nil
}

func refValue(v ref.Val) any {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case types.Bool:
		return bool(t)
	case types.Int:
		return int64(t)
	case types.Double:
		return float64(t)
	case types.String:
		return string(t)
	case types.Timestamp: // 新版 cel-go 中 Timestamp 即 time.Time 别名
		return t
	default:
		return v.Value()
	}
}
