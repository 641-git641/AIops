package evidence

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// collect.go：RCA 自动证据收集器——事故分析时自动抓数据源证据，追加进 evidence []string。
// 不阻塞主链：Collect 总预算 2s（ctx 超时），任何失败只追加降级说明句子。
// Prometheus 用预置模板（默认空=不查——指标名无通用约定）；Loki 用 {service="x"} 通用查询。

// 日志错误级关键词（ERROR/exception/panic/fatal）。
var reLogError = regexp.MustCompile(`(?i)(\berror\b|exception|panic|fatal)`)

// EvidenceCollector 数据源证据收集器（nil 字段=该数据源未配置）。
type EvidenceCollector struct {
	Prom        *PromClient
	Loki        *LokiClient
	PromQueries []string      // PromQL 模板，%s 替换为 service；空=不查指标
	LogWindow   time.Duration // Loki 查询时间窗（默认 30min）
	LogLimit    int           // Loki 返回条数（默认 50）
}

// NewEvidenceCollector 创建收集器。
func NewEvidenceCollector(prom *PromClient, loki *LokiClient, promQueries []string) *EvidenceCollector {
	return &EvidenceCollector{Prom: prom, Loki: loki, PromQueries: promQueries,
		LogWindow: 30 * time.Minute, LogLimit: 50}
}

// Collect 抓取证据句子（总预算 2s；失败 → 降级说明句子，不返回 error——不阻塞主链）。
func (c *EvidenceCollector) Collect(ctx context.Context, service string, since time.Time) []string {
	if c == nil {
		return nil
	}
	budgetCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var sentences []string
	if c.Loki != nil {
		sentences = append(sentences, c.collectLogs(budgetCtx, service)...)
	}
	if c.Prom != nil && len(c.PromQueries) > 0 {
		sentences = append(sentences, c.collectMetrics(budgetCtx, service)...)
	}
	return sentences
}

// collectLogs 查 Loki：{service="x"} 时间窗内最近 N 行，统计错误级并取样。
func (c *EvidenceCollector) collectLogs(ctx context.Context, service string) []string {
	now := time.Now().UTC()
	window := c.LogWindow
	if window <= 0 {
		window = 30 * time.Minute
	}
	limit := c.LogLimit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	query := fmt.Sprintf(`{service=%q}`, service)
	res, err := c.Loki.QueryRange(ctx, query, now.Add(-window), now, limit)
	if err != nil {
		return []string{fmt.Sprintf("数据源日志（Loki）不可达：%s（证据缺失，不纳入归因）", truncate(err.Error(), 100))}
	}
	if len(res.Entries) == 0 {
		return []string{fmt.Sprintf("数据源日志：%s 近 %d 分钟无日志（排除日志因素）", service, int(window.Minutes()))}
	}
	// 统计错误级行 + 取样（脱敏后最多 3 条）
	var errorLines []string
	errorCount := 0
	for _, e := range res.Entries {
		line := RedactLogLine(e.Line)
		if reLogError.MatchString(line) {
			errorCount++
			if len(errorLines) < 3 {
				errorLines = append(errorLines, fmt.Sprintf("%s %s",
					e.Timestamp.Format("15:04:05"), truncate(line, 120)))
			}
		}
	}
	if errorCount == 0 {
		return []string{fmt.Sprintf("数据源日志：%s 近 %d 分钟 %d 行日志，无 ERROR/异常级（排除日志因素）",
			service, int(window.Minutes()), len(res.Entries))}
	}
	msg := fmt.Sprintf("数据源日志（Loki）：%s 近 %d 分钟 %d 行日志中含 %d 条错误级（ERROR/exception/panic/fatal）",
		service, int(window.Minutes()), len(res.Entries), errorCount)
	if len(errorLines) > 0 {
		msg += "，样本：" + strings.Join(errorLines, " | ")
	}
	return []string{msg}
}

// collectMetrics 查 Prometheus 预置模板：非空 series 且跳变（末>2×首 或 首为 0 末>0）→ 句子。
func (c *EvidenceCollector) collectMetrics(ctx context.Context, service string) []string {
	now := time.Now().UTC()
	var sentences []string
	for _, tmpl := range c.PromQueries {
		q := strings.ReplaceAll(tmpl, "%s", service)
		res, err := c.Prom.QueryRange(ctx, q, now.Add(-30*time.Minute), now, time.Minute)
		if err != nil {
			sentences = append(sentences,
				fmt.Sprintf("监控指标（Prometheus）查询失败：%s（证据缺失）", truncate(err.Error(), 100)))
			continue
		}
		for _, s := range res.Series {
			if len(s.Points) < 2 {
				continue
			}
			first, last := s.Points[0][1], s.Points[len(s.Points)-1][1]
			jump := last > first*2 || (first == 0 && last > 0)
			if !jump {
				continue
			}
			name := s.Labels["__name__"]
			if name == "" {
				name = q
			}
			sentences = append(sentences,
				fmt.Sprintf("监控指标（Prometheus）%s：%s 近 30 分钟值从 %.2f 跳变至 %.2f（异常信号）",
					name, service, first, last))
		}
	}
	return sentences
}
