package alert

import (
	"encoding/json"
	"sort"

	"aiops/internal/util"
)

// Fingerprint 计算告警逻辑去重键。
// 语义：相同 provider + 名称 + 关键标签（排除易变字段）的告警视为同一逻辑告警。
// 参与指纹的字段：名称 + 目标 + 排序后标签。
func Fingerprint(provider, name string, labels map[string]any) string {
	// 参与指纹的标签：排除易变/无意义的字段
	exclude := map[string]bool{"timestamp": true, "startsAt": true, "endsAt": true, "instance": true, "job": true}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if !exclude[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []any
	parts = append(parts, provider, name)
	for _, k := range keys {
		parts = append(parts, k, labels[k])
	}
	return util.SHA256(jsonString(parts))
}

// ContentHash 计算告警内容物理去重键（event 快照的稳定哈希）。
func ContentHash(event map[string]any) string {
	return util.HashEvent(event)
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
