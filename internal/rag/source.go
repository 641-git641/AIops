package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"aiops/internal/util"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// source.go：个人知识源（runbook/SOP 原文库，KEEPON M3 §6.1）。
// knowledge_sources 存原文（含 front matter），规范化快照在 knowledge_documents；
// 发布=版本+1 并入队索引，更新/删除走版本与删除传播，不重复建设数据链。

// 知识源状态（用户动作态；文档级 quarantine 由 unsafe 反馈联动置位作展示辅助）。
const (
	SrcDraft       = "draft"       // 草稿：不索引、不入队
	SrcPublished   = "published"   // 已发布：入队 upsert
	SrcDeleted     = "deleted"     // 已删除（软删保留记录）：入队 delete
	SrcQuarantined = "quarantined" // unsafe 反馈联动（展示辅助；重新发布=新版本天然复活）
)

// maxSourceBytes 知识源内容上限（1MB；预览/创建校验）。
const maxSourceBytes = 1 << 20

// KnowledgeSource 个人知识源（runbook/SOP 原文；规范化快照在 knowledge_documents）。
type KnowledgeSource struct {
	ID          string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID    string         `gorm:"size:64;index" json:"tenantId"`
	Name        string         `gorm:"size:255" json:"name"` // 文件名/标题
	SourceType  string         `gorm:"size:16" json:"sourceType"`
	Content     string         `gorm:"type:text" json:"content"`      // 原始 markdown（含 front matter 原文）
	FrontMatter datatypes.JSON `gorm:"type:jsonb" json:"frontMatter"` // 解析结果 {title,service,tags,updated_at}
	Service     string         `gorm:"size:64" json:"service,omitempty"`
	Status      string         `gorm:"size:16;index" json:"status"`
	Version     int64          `gorm:"index" json:"version"` // 发布版本（= document.source_version）
	ContentHash string         `gorm:"size:64" json:"contentHash"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
}

// TableName 表名。
func (KnowledgeSource) TableName() string { return "knowledge_sources" }

// FrontMatter 解析出的元数据（白名单字段）。
type FrontMatter struct {
	Title     string     `json:"title,omitempty"`
	Service   string     `json:"service,omitempty"`
	Tags      []string   `json:"tags,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// SourcePreview 上传预览（无副作用：解析 + 脱敏检查结果）。
type SourcePreview struct {
	Name        string       `json:"name"`
	FrontMatter FrontMatter  `json:"frontMatter"`
	Body        string       `json:"body"`       // 剥离 front matter 的正文（预览用）
	Redactions  []RedactInfo `json:"redactions"` // Sanitize 检查结果（敏感类别）
	Injection   bool         `json:"injection"`
	Bytes       int          `json:"bytes"`
	Warning     string       `json:"warning,omitempty"`
}

// RedactInfo Sanitize 的脱敏条目（复用 sanitizer 类型）。
type RedactInfo struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

// ParseFrontMatter 解析首部 --- 块（白名单 title/service/tags/updated_at）；
// 无 front matter 或解析失败时 body=全文。仅文件首部判定，正文中的 --- 不受影响。
func ParseFrontMatter(content string) (FrontMatter, string) {
	var fm FrontMatter
	body := content
	trimmed := strings.TrimPrefix(content, "\uFEFF") // 容忍 BOM（字面 U+FEFF 转义）
	if strings.HasPrefix(trimmed, "---\n") || strings.HasPrefix(trimmed, "---\r\n") {
		rest := trimmed[4:]
		if idx := strings.Index(rest, "\n---\n"); idx >= 0 {
			block := rest[:idx]
			body = rest[idx+5:]
			for _, line := range strings.Split(block, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				kv := strings.SplitN(line, ":", 2)
				if len(kv) != 2 {
					continue
				}
				key, val := strings.ToLower(strings.TrimSpace(kv[0])), strings.TrimSpace(kv[1])
				switch key {
				case "title":
					fm.Title = strings.Trim(val, "\"'")
				case "service":
					fm.Service = strings.Trim(val, "\"'")
				case "tags":
					fm.Tags = parseTags(val)
				case "updated_at":
					if t, err := time.Parse(time.RFC3339, val); err == nil {
						fm.UpdatedAt = &t
					} else if t, err := time.Parse("2006-01-02", val); err == nil {
						fm.UpdatedAt = &t
					}
				}
				// 非白名单字段忽略（含来源行不报错）
			}
		}
	}
	return fm, body
}

// parseTags 解析 tags：[a, b] / "a, b" / a,b / a b。
func parseTags(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		p = strings.Trim(p, `"'`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SourceInput 创建/更新知识源请求（JSON 携带内容，前端读文件——免 multipart）。
type SourceInput struct {
	Name    string `json:"name" binding:"required"`
	Content string `json:"content" binding:"required"`
}

// PreviewSource 上传预览（无副作用）：front matter 解析 + Sanitize 检查。
func (s *Service) PreviewSource(name, content string) (*SourcePreview, error) {
	if len(content) > maxSourceBytes {
		return nil, fmt.Errorf("知识源超过 %dKB 上限", maxSourceBytes>>10)
	}
	fm, body := ParseFrontMatter(content)
	res, err := Sanitize(body)
	if err != nil {
		return nil, fmt.Errorf("内容安全检查失败: %w", err)
	}
	p := &SourcePreview{Name: name, FrontMatter: fm, Body: body, Bytes: len(content), Injection: res.Injection}
	if len(res.Redacted) > 0 {
		counts := map[string]int{}
		for _, c := range res.Redacted {
			counts[c]++
		}
		for cat, n := range counts {
			p.Redactions = append(p.Redactions, RedactInfo{Category: cat, Count: n})
		}
	}
	if res.Injection {
		p.Warning = "检测到疑似提示注入内容，发布后仍会脱敏并标记，请确认内容来源可信"
	}
	return p, nil
}

// CreateSource 创建草稿（不索引）。
func (s *Service) CreateSource(ctx context.Context, tenantID string, in SourceInput) (*KnowledgeSource, error) {
	if len(in.Content) > maxSourceBytes {
		return nil, fmt.Errorf("知识源超过 %dKB 上限", maxSourceBytes>>10)
	}
	fm, _ := ParseFrontMatter(in.Content)
	src := &KnowledgeSource{
		ID: util.NewID(), TenantID: tenantID, Name: in.Name, SourceType: SourceRunbook,
		Content: in.Content, Status: SrcDraft, Version: 0,
		ContentHash: contentHash(in.Content), CreatedAt: time.Now().UTC(),
	}
	src.FrontMatter = mustJSON(fm)
	src.Service = fm.Service
	if err := s.db.WithContext(ctx).Create(src).Error; err != nil {
		return nil, err
	}
	return src, nil
}

// PublishSource 发布草稿/重新发布：Sanitize 预检（error → 阻断）→ version+1 →
// 同事务入队 upsert（draft 首次发布 version=1）。
func (s *Service) PublishSource(ctx context.Context, tenantID, id string) (*KnowledgeSource, error) {
	src, err := s.getSource(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if src.Status == SrcDeleted {
		return nil, errors.New("知识源已删除")
	}
	if _, err := Sanitize(bodyOf(src.Content)); err != nil {
		return nil, fmt.Errorf("内容安全检查失败，发布已阻断: %w", err)
	}
	now := time.Now().UTC()
	src.Version++
	src.Status = SrcPublished
	src.UpdatedAt = now
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&KnowledgeSource{}).Where("tenant_id = ? AND id = ?", tenantID, id).
			Updates(map[string]any{"status": SrcPublished, "version": src.Version, "updated_at": now}).Error; err != nil {
			return err
		}
		return EnqueueJob(ctx, tx, tenantID, SourceRunbook, id, src.Version, OpUpsert)
	})
	if err != nil {
		return nil, err
	}
	return src, nil
}

// UpdateSource 更新内容：draft 仅存内容；published 且内容变化 → version+1 + 同事务入队
// （保存即发布新版本，与结案体验一致；ContentHash 未变不产生新版本）。
func (s *Service) UpdateSource(ctx context.Context, tenantID, id string, in SourceInput) (*KnowledgeSource, error) {
	if len(in.Content) > maxSourceBytes {
		return nil, fmt.Errorf("知识源超过 %dKB 上限", maxSourceBytes>>10)
	}
	src, err := s.getSource(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if src.Status == SrcDeleted {
		return nil, errors.New("知识源已删除")
	}
	fm, _ := ParseFrontMatter(in.Content)
	hash := contentHash(in.Content)
	now := time.Now().UTC()
	updates := map[string]any{
		"name": in.Name, "content": in.Content, "front_matter": mustJSON(fm),
		"service": fm.Service, "content_hash": hash, "updated_at": now,
	}
	enqueue := false
	if src.Status == SrcPublished && hash != src.ContentHash {
		src.Version++
		updates["version"] = src.Version
		enqueue = true
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&KnowledgeSource{}).Where("tenant_id = ? AND id = ?", tenantID, id).
			Updates(updates).Error; err != nil {
			return err
		}
		if enqueue {
			return EnqueueJob(ctx, tx, tenantID, SourceRunbook, id, src.Version, OpUpsert)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	src.FrontMatter = mustJSON(fm)
	src.Service = fm.Service
	src.Content = in.Content
	src.Name = in.Name
	src.ContentHash = hash
	src.UpdatedAt = now
	return src, nil
}

// DeleteSource 删除知识源：软删 + 入队 delete（indexer 传播撤销文档/chunk/embedding）。
func (s *Service) DeleteSource(ctx context.Context, tenantID, id string) error {
	src, err := s.getSource(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if src.Status == SrcDeleted {
		return nil
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&KnowledgeSource{}).Where("tenant_id = ? AND id = ?", tenantID, id).
			Updates(map[string]any{"status": SrcDeleted, "updated_at": now}).Error; err != nil {
			return err
		}
		return EnqueueJob(ctx, tx, tenantID, SourceRunbook, id, src.Version, OpDelete)
	})
}

// GetSource 知识源详情（含原文；编辑回显用）。
func (s *Service) GetSource(ctx context.Context, tenantID, id string) (*KnowledgeSource, error) {
	return s.getSource(ctx, tenantID, id)
}

// ListSources 知识源列表（派生索引状态：最近 job + 最新文档，便于前端状态列）。
func (s *Service) ListSources(ctx context.Context, tenantID string) ([]SourceRow, error) {
	var rows []SourceRow
	err := s.db.WithContext(ctx).Raw(`
		SELECT s.id, s.name, s.service, s.status, s.version, s.updated_at,
		       j.status AS job_status, j.last_error AS job_error, j.source_version AS job_version,
		       d.status AS doc_status, d.source_version AS doc_version, d.trust_level
		FROM knowledge_sources s
		LEFT JOIN LATERAL (
			SELECT status, last_error, source_version FROM knowledge_jobs
			WHERE tenant_id = s.tenant_id AND source_type = 'runbook' AND source_id = s.id
			ORDER BY created_at DESC LIMIT 1
		) j ON true
		LEFT JOIN LATERAL (
			SELECT status, source_version, trust_level FROM knowledge_documents
			WHERE tenant_id = s.tenant_id AND source_type = 'runbook' AND source_id = s.id
			ORDER BY source_version DESC LIMIT 1
		) d ON true
		WHERE s.tenant_id = ?
		ORDER BY s.updated_at DESC`, tenantID).Scan(&rows).Error
	return rows, err
}

// SourceRow 知识源列表行（含派生索引状态）。
type SourceRow struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Service    string    `json:"service,omitempty"`
	Status     string    `json:"status"`
	Version    int64     `json:"version"`
	UpdatedAt  time.Time `json:"updatedAt"`
	JobStatus  string    `json:"jobStatus,omitempty"`
	JobError   string    `json:"jobError,omitempty"`
	JobVersion int64     `json:"jobVersion,omitempty"`
	DocStatus  string    `json:"docStatus,omitempty"`
	DocVersion int64     `json:"docVersion,omitempty"`
	TrustLevel string    `json:"trustLevel,omitempty"`
}

func (s *Service) getSource(ctx context.Context, tenantID, id string) (*KnowledgeSource, error) {
	var src KnowledgeSource
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&src).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.New("知识源不存在")
	}
	return &src, err
}

// bodyOf 剥离 front matter 后的正文（索引/预检用）。
func bodyOf(content string) string {
	_, body := ParseFrontMatter(content)
	return body
}

func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func mustJSON(v any) datatypes.JSON {
	b, err := json.Marshal(v)
	if err != nil {
		return datatypes.JSON(`{}`)
	}
	return datatypes.JSON(b)
}
