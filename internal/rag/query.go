package rag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// RetrievalQuery 结构化检索查询（RAG_PLAN §9.1）：RCA/相似事故不直接把整段
// Prompt 作为查询，先生成结构化对象再决定三路召回。
type RetrievalQuery struct {
	TenantID        string
	IncidentID      string // 排除自身
	Service         string
	AlertNames      []string
	Severity        string
	Summary         string
	Labels          map[string]string // 白名单标签
	ErrorCodes      []string
	DeterministicRC string
	Changes         []string
	OccurredAt      time.Time
}

// 查询标签白名单（RAG_PLAN §9.1）：高基数、敏感或无解释意义的标签不拼入语义查询。
var queryLabelAllowlist = map[string]bool{
	"service": true, "service_name": true, "target_service": true,
	"alertname": true, "severity": true,
	"env": true, "region": true, "job": true, "namespace": true,
	"error_code": true, "errno": true, "code": true, "status": true, "status_code": true,
}

// QueryText 语义查询文本（向量/关键词路共用）：服务 + 告警名 + 摘要 + 错误码 + 确定性根因。
func (q RetrievalQuery) QueryText() string {
	var parts []string
	if q.Service != "" {
		parts = append(parts, q.Service)
	}
	parts = append(parts, q.AlertNames...)
	if q.Summary != "" {
		parts = append(parts, q.Summary)
	}
	if len(q.ErrorCodes) > 0 {
		parts = append(parts, q.ErrorCodes...)
	}
	if q.DeterministicRC != "" && q.DeterministicRC != "no_topology" && q.DeterministicRC != "service_not_found" {
		parts = append(parts, q.DeterministicRC)
	}
	if len(q.Changes) > 0 {
		parts = append(parts, q.Changes...)
	}
	return strings.Join(parts, " ")
}

// Hash 查询指纹（trace 去重/缓存键；不含正文细节）。
func (q RetrievalQuery) Hash() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		q.TenantID, q.Service, strings.Join(q.AlertNames, ","),
		q.Severity, q.Summary, strings.Join(q.ErrorCodes, ","),
		q.DeterministicRC, strings.Join(q.Changes, ","),
		q.OccurredAt.Format(time.RFC3339),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// BuildQuery 从事故与告警构造查询（白名单标签过滤；错误码提取）。
func BuildQuery(tenantID, incidentID, severity, summary, service string, alertNames []string, labels map[string]string, occurredAt time.Time) RetrievalQuery {
	q := RetrievalQuery{
		TenantID:   tenantID,
		IncidentID: incidentID,
		Severity:   severity,
		Summary:    summary,
		Labels:     map[string]string{},
		OccurredAt: occurredAt,
	}
	for k, v := range labels {
		if queryLabelAllowlist[k] && v != "" {
			q.Labels[k] = v
		}
	}
	if service == "" {
		service = serviceOf(q.Labels)
	}
	q.Service = service
	q.AlertNames = dedupStrings(alertNames)
	q.ErrorCodes = extractErrorCodes(summary, q.Labels)
	return q
}

// 错误码模式：常见 errno 字面量 + 标签中的 code/status 类字段。
var reErrno = regexp.MustCompile(`\b(ECONNREFUSED|ECONNRESET|ETIMEDOUT|ENOSPC|EACCES|EEXIST|ECONNABORTED|EIO|ENOMEM)\b`)

// extractErrorCodes 从摘要与白名单标签提取错误码（精确匹配用）。
func extractErrorCodes(summary string, labels map[string]string) []string {
	var codes []string
	for _, k := range []string{"error_code", "errno", "code", "status", "status_code"} {
		if v := labels[k]; v != "" {
			codes = append(codes, v)
		}
	}
	for _, m := range reErrno.FindAllString(summary, -1) {
		codes = append(codes, m)
	}
	return dedupStrings(codes)
}

// dedupStrings 保序去重。
func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// exactMetaJSON 精确路 metadata 匹配 JSONB（alert_names / error_codes 包含查询）。
func (q RetrievalQuery) exactMetaJSON() []byte {
	// 只发数组键（jsonb @> 数组包含语义）
	m := map[string]any{}
	if len(q.AlertNames) > 0 {
		m["alert_names"] = q.AlertNames
	}
	if len(q.ErrorCodes) > 0 {
		m["error_codes"] = q.ErrorCodes
	}
	raw, _ := json.Marshal(m)
	return raw
}
