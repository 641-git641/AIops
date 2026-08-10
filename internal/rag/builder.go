package rag

import (
	"encoding/json"
	"strings"
	"time"
)

// 文档规范化（RAG_PLAN §8.1）：结构化字段稳定生成，区分「告警现象」「确定性证据」
// 「人工确认根因」「模型推测」——不得拼成无法区分来源的一段文字。
// 同输入两次构建字节级一致（确定性），checksum 基于规范化正文。

// ResolutionInfo 结案数据（来自 incident_resolutions）。
type ResolutionInfo struct {
	ConfirmedRootCause string
	Resolution         string
	Outcome            string
	Verification       string
	VerificationStatus string
	ResolvedBy         string
	ResolvedAt         time.Time
	Version            int64
}

// IncidentInfo 事故索引源（由 repository 读取 incidents/alerts/incident_states 填充）。
type IncidentInfo struct {
	ID         string
	TenantID   string
	Name       string
	Summary    string
	Severity   string
	Status     string
	CreatedAt  time.Time
	AlertNames []string
	Labels     map[string]string // 白名单标签（service/error_code 等）
	// 确定性证据（node_results.rca：rootCause/evidence/paths/changes；agent 路径才有）
	RCAEvidence []string
	RCAChanges  []string
	RCAPaths    []string
	RootCause   string
	Res         *ResolutionInfo // nil = 已解决但未填结案
}

// PlaybookInfo 剧本索引源（命令模板不写入正文：默认不把完整命令暴露给外部模型）。
type PlaybookInfo struct {
	ID                string
	TenantID          string
	Name              string
	Level             string
	BlastRadiusFactor float64
	DryRunFirst       bool
	HasRollback       bool
	Enabled           bool
	UpdatedAt         time.Time
}

// DocBuild 构建结果。
type DocBuild struct {
	Title    string
	Body     string
	Service  string
	Severity string
	Trust    string // T0|T1|T2|T3
	Verif    string // verified|unverified|rejected
	// Index bool：rejected 或不应入库（未解决事故）时为 false → 按 delete 传播
	Index  bool
	Meta   map[string]any // 白名单 metadata（alert_names/error_codes/level/outcome/occurred_at...）
	Source VersionKey
}

// VersionKey 版本唯一键。
type VersionKey struct {
	TenantID      string
	SourceType    string
	SourceID      string
	SourceVersion int64
}

// TrustOfResolution 可信等级（RAG_PLAN §5.3）：verified+success → T3；
// verified+partial/failed → T2；false_positive/unverified/无结案 → T1；rejected → 不入库。
func TrustOfResolution(r *ResolutionInfo) (trust, verif string, index bool) {
	if r == nil {
		return TrustT1, VerifUnverified, true
	}
	switch r.VerificationStatus {
	case VerifRejected:
		return "", VerifRejected, false
	case VerifVerified:
		switch r.Outcome {
		case "success":
			return TrustT3, VerifVerified, true
		case "partial", "failed":
			return TrustT2, VerifVerified, true
		default: // false_positive：现象雷同但非真实故障，仅作相似性提示
			return TrustT1, VerifVerified, true
		}
	default:
		return TrustT1, VerifUnverified, true
	}
}

// BuildIncident 构建事故文档（已解决事故入知识库；未解决不入库 → Index=false 由调用方按 delete 传播）。
func BuildIncident(in IncidentInfo) DocBuild {
	b := DocBuild{
		Title:    in.Name,
		Service:  serviceOf(in.Labels),
		Severity: in.Severity,
		Meta: map[string]any{
			"alert_names": in.AlertNames,
			"occurred_at": in.CreatedAt.Format(time.RFC3339),
		},
		Source: VersionKey{TenantID: in.TenantID, SourceType: SourceIncident, SourceID: in.ID},
	}
	if in.Status != "resolved" {
		return b // 未解决事故默认不召回（T0），不建文档
	}
	if in.Res != nil {
		b.Meta["outcome"] = in.Res.Outcome
		b.Meta["verification_status"] = in.Res.VerificationStatus
	}
	b.Trust, b.Verif, b.Index = TrustOfResolution(in.Res)
	if !b.Index {
		return b
	}
	if codes := extractErrorCodes(in.Summary, in.Labels); len(codes) > 0 {
		b.Meta["error_codes"] = codes
	}
	lines := []string{
		"现象：" + in.Summary,
	}
	if len(in.AlertNames) > 0 {
		lines = append(lines, "告警："+strings.Join(in.AlertNames, ", "))
	}
	// 确定性证据（RCA 结构化结论，标注来源——不是人工根因）
	if in.RootCause != "" {
		lines = append(lines, "确定性证据：rootCause="+in.RootCause)
	}
	if len(in.RCAEvidence) > 0 {
		lines = append(lines, "确定性证据：证据="+strings.Join(in.RCAEvidence, "；"))
	}
	if len(in.RCAPaths) > 0 {
		lines = append(lines, "确定性证据：依赖路径="+strings.Join(in.RCAPaths, " → "))
	}
	if len(in.RCAChanges) > 0 {
		lines = append(lines, "确定性证据：近期变更="+strings.Join(in.RCAChanges, "；"))
	}
	// 人工确认内容（高可信，与证据分区明确区分）
	if in.Res != nil {
		if in.Res.ConfirmedRootCause != "" {
			lines = append(lines, "确认根因："+in.Res.ConfirmedRootCause)
		}
		if in.Res.Resolution != "" {
			lines = append(lines, "实际处置："+in.Res.Resolution)
		}
		if in.Res.Verification != "" {
			lines = append(lines, "验证："+in.Res.Verification)
		}
		lines = append(lines, "结果："+in.Res.Outcome)
		if !in.Res.ResolvedAt.IsZero() {
			lines = append(lines, "确认时间："+in.Res.ResolvedAt.Format(time.RFC3339))
		}
	}
	lines = append(lines, "发生时间："+in.CreatedAt.Format(time.RFC3339))
	b.Body = strings.Join(lines, "\n")
	return b
}

// RunbookInfo 知识源（runbook/SOP）构建输入。
type RunbookInfo struct {
	ID        string
	TenantID  string
	Name      string
	Service   string
	Tags      []string
	Content   string // 已剥离 front matter 的 markdown 正文
	Status    string // published → 建文档；draft → Index=false
	UpdatedAt time.Time
	Version   int64
}

// BuildRunbook 构建知识源文档（KEEPON M3：个人 runbook 导入；published → T2/verified）。
// 首期为个人导入即 T2（对齐"已发布 Playbook"等级），T3 留人工复核流程后续。
func BuildRunbook(in RunbookInfo) DocBuild {
	b := DocBuild{
		Title:   "runbook：" + in.Name,
		Service: in.Service,
		Meta: map[string]any{
			"tags":        in.Tags,
			"source_name": in.Name,
		},
		Source: VersionKey{TenantID: in.TenantID, SourceType: SourceRunbook, SourceID: in.ID, SourceVersion: in.Version},
	}
	if in.Status != SrcPublished {
		return b // 草稿/已删除不建文档
	}
	b.Trust, b.Verif, b.Index = TrustT2, VerifVerified, true
	// 正文对齐"标签：内容"行风格（与 BuildIncident 一致；markdown 保留原始行）
	lines := []string{
		"标题：" + in.Name,
	}
	if in.Service != "" {
		lines = append(lines, "服务："+in.Service)
	}
	if len(in.Tags) > 0 {
		lines = append(lines, "标签："+strings.Join(in.Tags, ", "))
	}
	body := strings.TrimSpace(in.Content)
	if body != "" {
		lines = append(lines, body)
	}
	lines = append(lines, "更新时间："+in.UpdatedAt.Format(time.RFC3339))
	b.Body = strings.Join(lines, "\n")
	return b
}

// BuildPlaybook 构建剧本文档（仅启用剧本入知识库；命令模板不出域，正文只用适用说明）。
func BuildPlaybook(p PlaybookInfo) DocBuild {
	b := DocBuild{
		Title:   "剧本：" + p.Name,
		Service: "",
		Meta: map[string]any{
			"level":        p.Level,
			"dry_run":      p.DryRunFirst,
			"has_rollback": p.HasRollback,
		},
		Source: VersionKey{TenantID: p.TenantID, SourceType: SourcePlaybook, SourceID: p.ID, SourceVersion: p.UpdatedAt.UnixNano()},
	}
	if !p.Enabled {
		return b
	}
	b.Trust, b.Verif, b.Index = TrustT2, VerifVerified, true // 已启用剧本 = 已发布（T2）
	// 命令模板不写入正文（§5.1.C：检索正文优先使用动作说明与适用条件）
	lines := []string{
		"适用动作：" + p.Name,
		"等级：" + p.Level,
		"爆炸半径：" + formatFactor(p.BlastRadiusFactor),
	}
	if p.DryRunFirst {
		lines = append(lines, "前置要求：强制 dry-run")
	}
	if p.HasRollback {
		lines = append(lines, "回滚能力：有")
	}
	lines = append(lines, "更新于："+p.UpdatedAt.Format(time.RFC3339))
	b.Body = strings.Join(lines, "\n")
	return b
}

// MetaJSON metadata 白名单字段序列化（调用方落库）。
func (b DocBuild) MetaJSON() []byte {
	raw, _ := json.Marshal(b.Meta)
	return raw
}

// serviceOf 从白名单标签提取服务名（与 monitor agent 一致：service/service_name）。
func serviceOf(labels map[string]string) string {
	for _, k := range []string{"service", "service_name", "target_service"} {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

func formatFactor(f float64) string {
	return strings.TrimRight(strings.TrimRight(jsonNumber(f), "0"), ".")
}

// jsonNumber 浮点最简文本（避免 0.15 → "0.15000000000000002"）。
func jsonNumber(f float64) string {
	raw, _ := json.Marshal(f)
	return string(raw)
}
