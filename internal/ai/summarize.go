package ai

import (
	"fmt"
	"strings"
)

// Summarize 为告警簇生成名称与摘要（确定性模板，不依赖 LLM）。
func Summarize(c *Cluster) (name, summary string) {
	name = fmt.Sprintf("%s 服务异常（%d 条关联告警）", c.Service, c.Count)
	if c.Severity != "" {
		name = fmt.Sprintf("%s %s（%d 条关联告警）", sevCN(c.Severity), c.Service, c.Count)
	}

	names := strings.Join(c.Names, "、")
	timeRange := ""
	if !c.StartedAt.IsZero() && !c.LastSeenAt.IsZero() {
		timeRange = fmt.Sprintf("于 %s~%s", c.StartedAt.Format("15:04"), c.LastSeenAt.Format("15:04"))
	}
	summary = fmt.Sprintf(
		"AI 聚类：%d 条 firing 告警%s聚为同一故障（最高严重度 %s），涉及：%s。已确认生成事故，建议人工或编排器跟进根因。",
		c.Count, timeRange, c.Severity, names,
	)
	return name, summary
}

func sevCN(s string) string {
	switch s {
	case "critical":
		return "严重"
	case "high":
		return "高危"
	case "medium":
		return "中危"
	case "low":
		return "低危"
	}
	return s
}
