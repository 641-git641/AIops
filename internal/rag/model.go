// Package rag 实现 RAG 历史知识检索层（RAG_PLAN.md Phase 0~2）。
// 只增强证据，不替代确定性事实：检索结果是有类型的 EvidencePack，
// 供相似事故只读 API（Phase 2）与后续 RCA Shadow（Phase 3）使用。
//
// 数据模型遵循 RAG_PLAN §7：源文档-切片-向量分离；租户/状态/服务/可信度/
// 有效期使用显式列，metadata JSONB 只放白名单扩展字段。
package rag

import (
	"time"

	"gorm.io/datatypes"
)

// 知识来源类型。
const (
	SourceIncident   = "incident"   // 已解决事故（最高优先级）
	SourcePlaybook   = "playbook"   // 已启用剧本
	SourceRunbook    = "runbook"    // 预留（Phase 5 文档治理）
	SourcePostmortem = "postmortem" // 预留（Phase 5 文档治理）
)

// 可信等级（RAG_PLAN §5.3）。只允许通过人工确认/发布流程提升，模型不能自行提升。
const (
	TrustT3 = "T3" // 人工确认且验证成功的事故、已审核 Runbook：RCA 和动作依据
	TrustT2 = "T2" // 已发布 Playbook、人工确认但结果为 partial：RCA 参考
	TrustT1 = "T1" // 未验证历史事故、自动聚类摘要：相似性提示
	TrustT0 = "T0" // 未解决事故、模型自由生成：默认不召回
)

// 验证状态（incident_resolutions.verification_status 同语义）。
const (
	VerifVerified   = "verified"   // 人工确认根因且验证成功（T3 前提）
	VerifUnverified = "unverified" // 未验证（仅低权重参考）
	VerifRejected   = "rejected"   // 确认有误（退出召回）
)

// 文档状态（旧版本退出在线检索但保留审计，查询只取 active）。
const (
	DocActive      = "active"
	DocSuperseded  = "superseded"
	DocDeleted     = "deleted"
	DocFailed      = "failed"
	DocQuarantined = "quarantined" // 命中 unsafe 反馈，立即退出召回（可人工恢复）
)

// 索引任务操作类型。
const (
	OpUpsert  = "upsert"  // 新增/更新知识版本
	OpDelete  = "delete"  // 删除/停用传播
	OpRebuild = "rebuild" // 全量重建（忽略 checksum 跳过，写新 index_version）
)

// 索引任务状态。
const (
	JobPending   = "pending"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobDead      = "dead" // 达到重试上限，人工介入（运维手册）
)

// KnowledgeDocument 知识文档（源记录规范化快照；同源同版本一行，版本更新生成新行旧行 superseded）。
type KnowledgeDocument struct {
	ID                 string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID           string         `gorm:"size:64;uniqueIndex:uk_kd_src,priority:1;index:idx_kd_tenant_status,priority:1" json:"tenantId"`
	SourceType         string         `gorm:"size:16;uniqueIndex:uk_kd_src,priority:2;index:idx_kd_tenant_status,priority:2" json:"sourceType"`
	SourceID           string         `gorm:"size:36;uniqueIndex:uk_kd_src,priority:3" json:"sourceId"`
	SourceVersion      int64          `gorm:"uniqueIndex:uk_kd_src,priority:4" json:"sourceVersion"` // incident=resolution.version，playbook=UpdatedAt.UnixNano()
	Title              string         `gorm:"size:255" json:"title"`
	Body               string         `gorm:"type:text" json:"body"` // 规范化正文（含来源标签，区分现象/证据/人工根因/模型推测）
	Service            string         `gorm:"size:64;index" json:"service,omitempty"`
	Severity           string         `gorm:"size:16" json:"severity,omitempty"`
	TrustLevel         string         `gorm:"size:4;index" json:"trustLevel"` // T0|T1|T2|T3
	VerificationStatus string         `gorm:"size:16" json:"verificationStatus"`
	Status             string         `gorm:"size:16;index:idx_kd_tenant_status,priority:3" json:"status"` // active|superseded|deleted|failed|quarantined
	ValidFrom          time.Time      `json:"validFrom"`
	ValidUntil         *time.Time     `json:"validUntil,omitempty"`    // 过期/撤销时间（查询时 active 且未过期）
	Checksum           string         `gorm:"size:64" json:"checksum"` // 内容 SHA-256（幂等与 reconciliation 依据）
	Metadata           datatypes.JSON `gorm:"type:jsonb" json:"metadata,omitempty"`
	CreatedAt          time.Time      `json:"createdAt"`
	UpdatedAt          time.Time      `json:"updatedAt"`
}

// TableName 表名。
func (KnowledgeDocument) TableName() string { return "knowledge_documents" }

// KnowledgeChunk 知识切片（content_tsv tsvector 列由 EnsureSchema 显式添加，
// GORM 模型不映射；同一 document 同 ordinal 同 checksum 幂等）。
type KnowledgeChunk struct {
	ID         string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID   string         `gorm:"size:64;index" json:"tenantId"`
	DocumentID string         `gorm:"size:36;uniqueIndex:uk_kc_doc,priority:1;index:idx_kc_doc_ordinal,priority:1" json:"documentId"`
	Ordinal    int            `gorm:"uniqueIndex:uk_kc_doc,priority:2;index:idx_kc_doc_ordinal,priority:2" json:"ordinal"`
	Section    string         `gorm:"size:64" json:"section"` // 语义字段名（现象/确认根因/处置/验证/...）
	Content    string         `gorm:"type:text" json:"content"`
	TokenCount int            `json:"tokenCount"`
	Checksum   string         `gorm:"size:64;uniqueIndex:uk_kc_doc,priority:3" json:"checksum"` // 内容 SHA-256（相同 checksum 不重复 Embedding）
	Metadata   datatypes.JSON `gorm:"type:jsonb" json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"createdAt"`
}

// TableName 表名。
func (KnowledgeChunk) TableName() string { return "knowledge_chunks" }

// KnowledgeEmbedding 知识向量（embedding vector(D) 列由 EnsureSchema 显式添加并
// 以 ::vector/::text 裸 SQL 读写；模型/维度变更必须换 index_version 重建，禁止混入同一索引）。
type KnowledgeEmbedding struct {
	ID           string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID     string    `gorm:"size:64;index" json:"tenantId"`
	ChunkID      string    `gorm:"size:36;uniqueIndex:uk_ke_chunk,priority:1" json:"chunkId"`
	ModelID      string    `gorm:"size:64;uniqueIndex:uk_ke_chunk,priority:2" json:"modelId"`
	IndexVersion string    `gorm:"size:32;uniqueIndex:uk_ke_chunk,priority:3" json:"indexVersion"`
	CreatedAt    time.Time `json:"createdAt"`
}

// TableName 表名。
func (KnowledgeEmbedding) TableName() string { return "knowledge_embeddings" }

// KnowledgeJob 持久化索引任务（RAG_PLAN §8.4：DB 任务而非内存队列；
// (tenant, source_type, source_id, source_version, operation) 幂等）。
type KnowledgeJob struct {
	ID            string     `gorm:"primaryKey;size:36" json:"id"`
	TenantID      string     `gorm:"size:64;uniqueIndex:uk_kj_src,priority:1;index:idx_kj_tenant_status,priority:1" json:"tenantId"`
	Operation     string     `gorm:"size:16;uniqueIndex:uk_kj_src,priority:2" json:"operation"` // upsert|delete|rebuild
	SourceType    string     `gorm:"size:16;uniqueIndex:uk_kj_src,priority:3" json:"sourceType"`
	SourceID      string     `gorm:"size:36;uniqueIndex:uk_kj_src,priority:4" json:"sourceId"`
	SourceVersion int64      `gorm:"uniqueIndex:uk_kj_src,priority:5" json:"sourceVersion"`
	Status        string     `gorm:"size:16;index:idx_kj_tenant_status,priority:2" json:"status"` // pending|running|succeeded|failed|dead
	Attempts      int        `json:"attempts"`
	NextRunAt     time.Time  `json:"nextRunAt"`
	LastError     string     `gorm:"type:text" json:"lastError,omitempty"`
	LockedAt      *time.Time `json:"lockedAt,omitempty"` // 租约：worker 崩溃后超时复活
	LockedBy      string     `gorm:"size:64" json:"lockedBy,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// TableName 表名。
func (KnowledgeJob) TableName() string { return "knowledge_jobs" }

// RAGTrace 检索轨迹（每次相似事故检索/后续 RCA 检索落一条；调试与管理用途）。
type RAGTrace struct {
	ID               string         `gorm:"primaryKey;size:36" json:"id"`
	TenantID         string         `gorm:"size:64;index" json:"tenantId"`
	IncidentID       string         `gorm:"size:36;index" json:"incidentId"`
	QueryHash        string         `gorm:"size:64" json:"queryHash"`
	RetrievalVersion string         `gorm:"size:32" json:"retrievalVersion"`
	IndexVersion     string         `gorm:"size:32" json:"indexVersion"`
	CandidateIDs     datatypes.JSON `gorm:"type:jsonb" json:"candidateIds"` // 召回候选 chunk_id 集合
	SelectedIDs      datatypes.JSON `gorm:"type:jsonb" json:"selectedIds"`  // 融合后入选 chunk_id 集合
	LatencyMS        datatypes.JSON `gorm:"type:jsonb" json:"latencyMs"`    // {exact,keyword,vector,fusion}
	DegradedReason   string         `gorm:"size:64" json:"degradedReason"`
	CreatedAt        time.Time      `json:"createdAt"`
}

// TableName 表名。
func (RAGTrace) TableName() string { return "rag_traces" }

// RAGFeedback 人工反馈（不直接改索引内容；unsafe 触发 quarantine 快速退出召回）。
type RAGFeedback struct {
	ID                 string    `gorm:"primaryKey;size:36" json:"id"`
	TenantID           string    `gorm:"size:64;index" json:"tenantId"`
	IncidentID         string    `gorm:"size:36;index" json:"incidentId"`
	TraceID            string    `gorm:"size:36" json:"traceId,omitempty"`
	DocumentID         string    `gorm:"size:36;index" json:"documentId"`
	Rating             string    `gorm:"size:16" json:"rating"` // useful|not_useful|wrong|unsafe
	CorrectedRootCause string    `gorm:"type:text" json:"correctedRootCause,omitempty"`
	Actor              string    `gorm:"size:64" json:"actor,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
}

// TableName 表名。
func (RAGFeedback) TableName() string { return "rag_feedback" }

// 反馈评分。
const (
	RatingUseful    = "useful"
	RatingNotUseful = "not_useful"
	RatingWrong     = "wrong"
	RatingUnsafe    = "unsafe"
)
