package workflow

import (
	"encoding/json"
	"fmt"
	"strings"

	"aiops/internal/alert"
	"aiops/internal/util"
)

// MatchesTrigger 判断告警是否命中 alert 触发器。type=alert（空视为 alert），filters 全部满足才命中，无 filters 全命中。
// interval/manual 触发不经此匹配（由调度器/手动 API 驱动）。
func MatchesTrigger(t Trigger, a *alert.Alert) bool {
	if t.Type != "" && t.Type != "alert" {
		return false
	}
	labels := map[string]any{}
	_ = json.Unmarshal(a.Labels, &labels)
	for _, f := range t.Filters {
		if !matchFilter(f, a, labels) {
			return false
		}
	}
	return true
}

// matchFilter 单条件匹配。key 支持 severity（>=/<= 运算符）/service/name/provider/source/任意 labels 键。
func matchFilter(f Filter, a *alert.Alert, labels map[string]any) bool {
	switch f.Key {
	case "severity":
		return matchSeverity(f.Value, a.Severity)
	case "service":
		return fmt.Sprintf("%v", labels["service"]) == f.Value
	case "name":
		return a.Name == f.Value
	case "provider", "source":
		return a.Provider == f.Value
	default:
		return fmt.Sprintf("%v", labels[f.Key]) == f.Value
	}
}

// matchSeverity 精确匹配或 >=/<= 范围匹配（按 SeverityRank 比较，未知严重度 rank=0）。
func matchSeverity(want, got string) bool {
	switch {
	case strings.HasPrefix(want, ">="):
		return alert.SeverityRank(got) >= alert.SeverityRank(strings.TrimPrefix(want, ">="))
	case strings.HasPrefix(want, "<="):
		return alert.SeverityRank(got) <= alert.SeverityRank(strings.TrimPrefix(want, "<="))
	default:
		return got == want
	}
}

// DedupKey 工作流运行的并发防重键：SHA256(workflow_id:alert.fingerprint)。
// 同一工作流对同一告警（指纹）只允许一条运行记录，DB 唯一索引保证并发首达仅一个赢者。
func DedupKey(workflowID, fingerprint string) string {
	return util.SHA256(workflowID + ":" + fingerprint)
}
