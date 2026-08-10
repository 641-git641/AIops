package rag

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// 切片规则（RAG_PLAN §8.2）：按语义字段/步骤边界切分，不按固定字符暴力切分；
// 单 chunk 目标 400~800 tokens；命令、错误码、服务名不能被截断（只允许在换行处硬切）。
// 具体数值是初始值，通过检索评测调整，不作为永久常量。

const (
	chunkTargetTokens = 600 // 目标 token 数（400~800 区间中值）
	chunkMaxTokens    = 900 // 硬上限（超出则按行硬切）
)

// EstimateTokens 粗略 token 估算（混合中英文：CJK 约 1 字/token，ASCII 约 4 字符/token）。
// 仅用于上下文预算与切片，不参与检索评分。
func EstimateTokens(s string) int {
	runes := utf8.RuneCountInString(s)
	bytes := len(s)
	// CJK 字符占 runes 的主要部分：runes/3 是对纯 CJK 偏保守、对纯 ASCII 偏宽松的折中
	return runes/3 + bytes/12 + 1
}

// section 语义切片（正文按「标签：内容」行分组）。
type section struct {
	Label string // 语义字段名（现象/确认根因/处置/验证/...）
	Lines []string
}

// Chunk 知识切片。
type Chunk struct {
	Ordinal    int
	Section    string // 主要语义字段
	Content    string // 带来源头的正文（标题/服务/版本）
	TokenCount int
	Checksum   string // 内容 SHA-256（相同 checksum 不重复 Embedding）
}

// chunkSource 来源信息（每个 chunk 重复携带最小来源：标题/服务/版本）。
type chunkSource struct {
	Title    string
	Service  string
	Version  int64
	Severity string
}

// ChunkBody 将规范化正文按语义行分组切片。
// body 格式：每行「标签：内容」，标签行开启新分组；长分组在换行处硬切（标识符不截断）。
func ChunkBody(body string, src chunkSource) []Chunk {
	sections := splitSections(body)
	var chunks []Chunk
	var cur section
	for _, sec := range sections {
		cur.Label = sec.Label // 当前分组以最新语义字段标记
		for _, line := range sec.Lines {
			lineTokens := EstimateTokens(line)
			if len(cur.Lines) > 0 && EstimateTokens(strings.Join(cur.Lines, "\n"))+lineTokens > chunkMaxTokens {
				// 硬切：只在换行处切，命令/错误码/服务名所在行完整保留
				chunks = append(chunks, flushSection(cur, src, len(chunks)))
				cur = section{Label: sec.Label}
			}
			cur.Lines = append(cur.Lines, line)
		}
		// 边界：单字段达到目标体量即切（保持语义边界干净）
		if len(cur.Lines) > 0 && EstimateTokens(strings.Join(cur.Lines, "\n")) >= chunkTargetTokens {
			chunks = append(chunks, flushSection(cur, src, len(chunks)))
			cur = section{}
		}
	}
	if len(cur.Lines) > 0 {
		chunks = append(chunks, flushSection(cur, src, len(chunks)))
	}
	return chunks
}

// splitSections 将「标签：内容」行分组为语义切片。
func splitSections(body string) []section {
	var out []section
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := labelLineRe.FindStringSubmatch(line); m != nil {
			out = append(out, section{Label: m[1]})
			continue
		}
		if len(out) == 0 {
			out = append(out, section{Label: "正文"})
		}
		last := &out[len(out)-1]
		last.Lines = append(last.Lines, line)
	}
	return out
}

// flushSection 产出 chunk：前置来源头（标题/服务/版本），正文与标签合并。
func flushSection(sec section, src chunkSource, ordinal int) Chunk {
	header := []string{}
	if sec.Label != "" {
		header = append(header, "【"+sec.Label+"】")
	}
	if src.Title != "" {
		header = append(header, "标题："+src.Title)
	}
	if src.Service != "" {
		header = append(header, "服务："+src.Service)
	}
	if src.Severity != "" {
		header = append(header, "严重度："+src.Severity)
	}
	header = append(header, "版本：v"+strconv.FormatInt(src.Version, 10))
	content := strings.Join(header, "\n")
	if len(sec.Lines) > 0 {
		content += "\n" + strings.Join(sec.Lines, "\n")
	}
	return Chunk{
		Ordinal:    ordinal,
		Section:    sec.Label,
		Content:    content,
		TokenCount: EstimateTokens(content),
		Checksum:   sha256Hex(content),
	}
}
