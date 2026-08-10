package rag

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"aiops/internal/util"

	"gorm.io/gorm"
)

// repository.go：知识库 PostgreSQL 查询与写入（RAG_PLAN §11.1）。
// 向量列与 tsvector 列不在 GORM 模型中，相关 DML/DDL 全部走裸 SQL（::vector/::text 显式转换）。
// 所有查询在 SQL 层强制 tenant 过滤（禁止跨租户检索后在内存过滤）。

// jobLease = worker 崩溃后 running 任务复活阈值。
// 权衡（M7）：单任务处理超过 lease（如 embedding 32 片 × 10s 超时）可能被第二个 worker
// 重复领取并发处理——数据写入幂等（ON CONFLICT）故无害，但会产生重复外部 embedding 调用
// 且 attempts 虚增。可接受：批量 embedding 通常秒级完成；如需严格单消费者可动态化 lease。
const jobLease = 2 * time.Minute

// jobMaxAttempts = 重试上限（超过进 dead 并告警，人工介入）。
const jobMaxAttempts = 8

// maxClaimBatch 单轮领取上限（限制对知识库连接的冲击）。
const maxClaimBatch = 50

// ---------- 索引任务 ----------

// EnqueueJob 幂等入队（(tenant, source_type, source_id, source_version, operation) 唯一）。
// ON CONFLICT 复活 failed/dead 任务（重试）；pending/running/succeeded 不动（防重复消费）。
func EnqueueJob(ctx context.Context, db *gorm.DB, tenantID, sourceType, sourceID string, sourceVersion int64, op string) error {
	return db.WithContext(ctx).Exec(`
		INSERT INTO knowledge_jobs (id, tenant_id, operation, source_type, source_id, source_version,
			status, attempts, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', 0, now(), now(), now())
		ON CONFLICT (tenant_id, source_type, source_id, source_version, operation)
		DO UPDATE SET status = 'pending', attempts = 0, next_run_at = now(),
			locked_at = NULL, locked_by = '', last_error = '', updated_at = now()
		WHERE knowledge_jobs.status IN ('failed', 'dead')`,
		util.NewID(), tenantID, op, sourceType, sourceID, sourceVersion).Error
}

// ClaimJobs 领取到期任务（FOR UPDATE SKIP LOCKED；running 超过租约复活）。
func ClaimJobs(ctx context.Context, db *gorm.DB, workerID string, limit int) ([]KnowledgeJob, error) {
	if limit <= 0 || limit > maxClaimBatch {
		limit = maxClaimBatch
	}
	var jobs []KnowledgeJob
	err := db.WithContext(ctx).Raw(`
		UPDATE knowledge_jobs SET status = 'running', locked_at = now(), locked_by = ?,
			attempts = attempts + 1, updated_at = now()
		WHERE id IN (
			SELECT id FROM knowledge_jobs
			WHERE (status = 'pending' AND next_run_at <= now())
			   OR (status = 'running' AND locked_at < now() - make_interval(secs => ?))
			ORDER BY next_run_at ASC
			LIMIT ?
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, tenant_id, operation, source_type, source_id, source_version, attempts, status`,
		workerID, int(jobLease.Seconds()), limit).Scan(&jobs).Error
	return jobs, err
}

// CompleteJob 标记成功。
func CompleteJob(ctx context.Context, db *gorm.DB, id string) error {
	return db.WithContext(ctx).Exec(
		`UPDATE knowledge_jobs SET status = 'succeeded', locked_at = NULL, locked_by = '',
			last_error = '', updated_at = now() WHERE id = ?`, id).Error
}

// FailJob 标记失败：达到上限 → dead（告警由调用方日志）；否则指数退避（5s 起、30min 封顶）。
func FailJob(ctx context.Context, db *gorm.DB, id string, attempts int, err error) error {
	status := JobFailed
	backoff := time.Second * 5 << attempts // 5s, 10s, ... 上限前约 10 分钟
	if backoff > 30*time.Minute {
		backoff = 30 * time.Minute
	}
	if attempts >= jobMaxAttempts {
		status = JobDead
		slog.Error("rag: index job dead", "job", id, "attempts", attempts, "err", err)
	} else {
		slog.Warn("rag: index job failed, will retry", "job", id, "attempts", attempts, "backoff", backoff, "err", err)
	}
	return db.WithContext(ctx).Exec(
		`UPDATE knowledge_jobs SET status = ?, next_run_at = now() + make_interval(secs => ?),
			last_error = ?, locked_at = NULL, locked_by = '', updated_at = now() WHERE id = ?`,
		status, int(backoff.Seconds()), truncateStr(err.Error(), 1000), id).Error
}

// ---------- 文档 ----------

// upsertDocument 文档幂等写入：同 (tenant, source, version) 原地更新并复活为 active。
// 返回 quarantined=true 表示冲突行已被 unsafe 反馈隔离——粘性保持退出召回（不复活）。
func upsertDocument(ctx context.Context, db *gorm.DB, doc DocBuild, body string, checksum string) (id string, quarantined bool, err error) {
	err = db.WithContext(ctx).Raw(`
		INSERT INTO knowledge_documents (id, tenant_id, source_type, source_id, source_version,
			title, body, service, severity, trust_level, verification_status, status,
			valid_from, checksum, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, 'active', now(), ?, ?::jsonb, now(), now())
		ON CONFLICT (tenant_id, source_type, source_id, source_version) DO UPDATE SET
			title = EXCLUDED.title, body = EXCLUDED.body, service = EXCLUDED.service,
			severity = EXCLUDED.severity, trust_level = EXCLUDED.trust_level,
			verification_status = EXCLUDED.verification_status, status = 'active',
			valid_until = NULL, checksum = EXCLUDED.checksum, metadata = EXCLUDED.metadata,
			updated_at = now()
		WHERE knowledge_documents.status <> 'quarantined'
		RETURNING id`,
		util.NewID(), doc.Source.TenantID, doc.Source.SourceType, doc.Source.SourceID, doc.Source.SourceVersion,
		doc.Title, body, doc.Service, doc.Severity, doc.Trust, doc.Verif, checksum, string(doc.MetaJSON())).
		Scan(&id).Error
	if err != nil {
		return "", false, err
	}
	if id == "" {
		// 冲突但被 WHERE 挡住（quarantined）：确认状态后返回粘性标记
		var st string
		if gerr := db.WithContext(ctx).Raw(
			`SELECT status FROM knowledge_documents WHERE tenant_id = ? AND source_type = ? AND source_id = ? AND source_version = ?`,
			doc.Source.TenantID, doc.Source.SourceType, doc.Source.SourceID, doc.Source.SourceVersion).Scan(&st).Error; gerr == nil && st == DocQuarantined {
			return "", true, nil
		}
		return "", false, errors.New("rag: upsert document returned no row")
	}
	return id, false, nil
}

// supersedeOlderVersions 旧版本退出在线检索（保留审计行；RAG_PLAN §5.4）。
func supersedeOlderVersions(ctx context.Context, db *gorm.DB, tenantID, sourceType, sourceID string, version int64) error {
	return db.WithContext(ctx).Exec(`
		UPDATE knowledge_documents SET status = 'superseded', valid_until = now(), updated_at = now()
		WHERE tenant_id = ? AND source_type = ? AND source_id = ? AND source_version < ? AND status = 'active'`,
		tenantID, sourceType, sourceID, version).Error
}

// deleteSource 删除传播：active 文档软删（保留审计），chunk/embedding 物理撤销（RAG_PLAN §5.4）。
// 任一步失败透传错误（避免半删状态；重试幂等）。
func deleteSource(ctx context.Context, db *gorm.DB, tenantID, sourceType, sourceID string) error {
	if err := db.WithContext(ctx).Exec(`
		UPDATE knowledge_documents SET status = 'deleted', valid_until = now(), updated_at = now()
		WHERE tenant_id = ? AND source_type = ? AND source_id = ? AND status = 'active'`,
		tenantID, sourceType, sourceID).Error; err != nil {
		return err
	}
	// 该源全部版本的 chunk 与 embedding 撤销（旧版本仅保留文档行审计）
	if err := db.WithContext(ctx).Exec(`
		DELETE FROM knowledge_embeddings e USING knowledge_chunks c, knowledge_documents d
		WHERE e.chunk_id = c.id AND c.document_id = d.id
		  AND d.tenant_id = ? AND d.source_type = ? AND d.source_id = ?`,
		tenantID, sourceType, sourceID).Error; err != nil {
		return err
	}
	return db.WithContext(ctx).Exec(`
		DELETE FROM knowledge_chunks c USING knowledge_documents d
		WHERE c.document_id = d.id AND d.tenant_id = ? AND d.source_type = ? AND d.source_id = ?`,
		tenantID, sourceType, sourceID).Error
}

// quarantineDocument 命中 unsafe 反馈：立即退出召回（保留数据待人工复核）。
func quarantineDocument(ctx context.Context, db *gorm.DB, tenantID, documentID string) error {
	return db.WithContext(ctx).Exec(
		`UPDATE knowledge_documents SET status = 'quarantined', valid_until = now(), updated_at = now()
		WHERE tenant_id = ? AND id = ? AND status = 'active'`, tenantID, documentID).Error
}

// insertChunk 幂等写切片（同 (document_id, ordinal, checksum) 不重复）。
// 返回 chunkID 与是否新插入（新插入才需要 embedding）。
// 注意：ON CONFLICT DO NOTHING 无插入行时 GORM Scan 返回 ErrRecordNotFound——
// 只把该场景视为"已存在"，其余错误必须透传（避免假成功产生悬挂数据）。
func insertChunk(ctx context.Context, db *gorm.DB, tenantID, docID string, c Chunk) (string, bool, error) {
	var id string
	err := db.WithContext(ctx).Raw(`
		INSERT INTO knowledge_chunks (id, tenant_id, document_id, ordinal, section, content,
			token_count, content_tsv, checksum, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, to_tsvector('simple', ?), ?, ?::jsonb, now())
		ON CONFLICT (document_id, ordinal, checksum) DO NOTHING
		RETURNING id`, util.NewID(), tenantID, docID, c.Ordinal, c.Section, c.Content,
		c.TokenCount, c.Content, c.Checksum, "{}").Scan(&id).Error
	switch {
	case err == nil && id != "":
		return id, true, nil
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		return "", false, err // 真实 DB 错误透传
	case id == "":
		// 冲突（已存在）：取既有 id；取不到也报错（不静默）
		var old string
		if gerr := db.WithContext(ctx).Raw(
			`SELECT id FROM knowledge_chunks WHERE document_id = ? AND ordinal = ? AND checksum = ?`,
			docID, c.Ordinal, c.Checksum).Scan(&old).Error; gerr == nil && old != "" {
			return old, false, nil
		}
		return "", false, errors.New("rag: insertChunk no row and no existing chunk")
	}
	return "", false, err
}

// embeddingExists 同 (chunk, model, index_version) 已有向量（跳过重复 embedding）。
func embeddingExists(ctx context.Context, db *gorm.DB, chunkID, modelID, indexVersion string) (bool, error) {
	var n int64
	err := db.WithContext(ctx).Raw(
		`SELECT count(*) FROM knowledge_embeddings WHERE chunk_id = ? AND model_id = ? AND index_version = ?`,
		chunkID, modelID, indexVersion).Scan(&n).Error
	return n > 0, err
}

// insertEmbedding 写入向量（裸 SQL，?::vector 显式转换）。
func insertEmbedding(ctx context.Context, db *gorm.DB, tenantID, chunkID, modelID, indexVersion string, v Vec) error {
	return db.WithContext(ctx).Exec(`
		INSERT INTO knowledge_embeddings (id, tenant_id, chunk_id, model_id, index_version, embedding, created_at)
		VALUES (?, ?, ?, ?, ?, ?::vector, now())
		ON CONFLICT (chunk_id, model_id, index_version) DO NOTHING`,
		util.NewID(), tenantID, chunkID, modelID, indexVersion, v.String()).Error
}

// ---------- 源读取（索引 worker 用） ----------

// loadIncidentSource 读取事故索引源（incidents + 结案 + 关联告警白名单 + RCA 确定性证据）。
// 源不存在返回 (nil, nil)（按删除传播）。
func loadIncidentSource(ctx context.Context, db *gorm.DB, tenantID, id string) (*IncidentInfo, error) {
	var row struct {
		ID, TenantID, Name, Summary, Severity, Status string
		CreatedAt                                     time.Time
	}
	if err := db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, name, summary, severity, status, created_at
		 FROM incidents WHERE tenant_id = ? AND id = ?`, tenantID, id).Scan(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	info := &IncidentInfo{
		ID: row.ID, TenantID: row.TenantID, Name: row.Name, Summary: row.Summary,
		Severity: row.Severity, Status: row.Status, CreatedAt: row.CreatedAt,
		Labels: map[string]string{},
	}
	// 结案数据（GORM 不直接扫描 **struct，先落局部变量；resolved_at 可能为 NULL（手动补录），用指针）
	var res struct {
		ConfirmedRootCause string
		Resolution         string
		Outcome            string
		Verification       string
		VerificationStatus string
		ResolvedBy         string
		ResolvedAt         *time.Time
		Version            int64
	}
	if err := db.WithContext(ctx).Raw(`
		SELECT confirmed_root_cause, resolution, outcome, verification, verification_status,
			resolved_by, resolved_at, version
		FROM incident_resolutions WHERE tenant_id = ? AND incident_id = ?`, tenantID, id).Scan(&res).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	} else {
		info.Res = &ResolutionInfo{
			ConfirmedRootCause: res.ConfirmedRootCause,
			Resolution:         res.Resolution,
			Outcome:            res.Outcome,
			Verification:       res.Verification,
			VerificationStatus: res.VerificationStatus,
			ResolvedBy:         res.ResolvedBy,
			Version:            res.Version,
		}
		if res.ResolvedAt != nil {
			info.Res.ResolvedAt = *res.ResolvedAt
		}
	}
	// 关联告警（白名单标签 + 告警名；原始事件不出域）
	var alertRows []struct {
		Name   string
		Labels string // jsonb → 文本
	}
	if err := db.WithContext(ctx).Raw(`
		SELECT a.name, a.labels::text AS labels
		FROM alert_to_incidents l JOIN alerts a ON a.id = l.alert_id AND a.tenant_id = l.tenant_id
		WHERE l.tenant_id = ? AND l.incident_id = ?`, tenantID, id).Scan(&alertRows).Error; err != nil {
		return nil, err
	}
	for _, ar := range alertRows {
		info.AlertNames = append(info.AlertNames, ar.Name)
		var m map[string]string
		_ = json.Unmarshal([]byte(ar.Labels), &m)
		for k, v := range m {
			if queryLabelAllowlist[k] {
				info.Labels[k] = v
			}
		}
	}
	info.AlertNames = dedupStrings(info.AlertNames)
	// RCA 确定性证据（node_results.rca；agent 路径才有）——与同文件其他查询一致强制 tenant
	var st struct{ NodeResults string }
	if err := db.WithContext(ctx).Raw(
		`SELECT node_results::text AS node_results FROM incident_states WHERE tenant_id = ? AND id = ?`,
		tenantID, id).Scan(&st).Error; err == nil && st.NodeResults != "" {
		var results map[string]map[string]any
		if json.Unmarshal([]byte(st.NodeResults), &results) == nil {
			if rca, ok := results["rca"]; ok {
				info.RootCause, _ = rca["rootCause"].(string)
				info.RCAEvidence = strSlice(rca["evidence"])
				info.RCAPaths = strSlice(rca["paths"])
				info.RCAChanges = strSlice(rca["changes"])
			}
		}
	}
	return info, nil
}

// loadPlaybookSource 读取剧本索引源（命令模板不进入 body，见 BuildPlaybook）。
func loadPlaybookSource(ctx context.Context, db *gorm.DB, tenantID, id string) (*PlaybookInfo, error) {
	var p PlaybookInfo
	err := db.WithContext(ctx).Raw(`
		SELECT id, tenant_id, name, level, blast_radius_factor,
			dry_run_first, (rollback_command <> '' AND rollback_command IS NOT NULL) AS has_rollback,
			enabled, updated_at
		FROM playbooks WHERE tenant_id = ? AND id = ?`, tenantID, id).Scan(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// loadRunbookSource 加载知识源（KEEPON M3；源不存在/已删除 → nil 按删除传播）。
func loadRunbookSource(ctx context.Context, db *gorm.DB, tenantID, id string) (*RunbookInfo, error) {
	var src KnowledgeSource
	err := db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&src).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fm, body := ParseFrontMatter(src.Content)
	tags := fm.Tags
	if len(tags) == 0 {
		if raw, err := json.Marshal(src.FrontMatter); err == nil {
			var parsed FrontMatter
			if json.Unmarshal(raw, &parsed) == nil {
				tags = parsed.Tags
			}
		}
	}
	return &RunbookInfo{
		ID: src.ID, TenantID: src.TenantID, Name: src.Name,
		Service: src.Service, Tags: tags, Content: body,
		Status: src.Status, UpdatedAt: src.UpdatedAt, Version: src.Version,
	}, nil
}

// strSlice 将 JSON 数组（rca 的 evidence/paths/changes）转为字符串切片。
func strSlice(v any) []string {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out []string
	if json.Unmarshal(raw, &out) == nil {
		return out
	}
	return nil
}

// ---------- 状态观测 ----------

// jobCounts 按状态统计。
func jobCounts(ctx context.Context, db *gorm.DB) map[string]int {
	out := map[string]int{}
	rows, err := db.WithContext(ctx).Raw(
		`SELECT status, count(*) FROM knowledge_jobs GROUP BY status`).Rows()
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		_ = rows.Scan(&st, &n)
		out[st] = n
	}
	return out
}

// indexLagSeconds 索引滞后：最近成功任务距现在秒数。
func indexLagSeconds(ctx context.Context, db *gorm.DB) float64 {
	var lag *float64
	if err := db.WithContext(ctx).Raw(
		`SELECT EXTRACT(EPOCH FROM (now() - MAX(updated_at)))
		 FROM knowledge_jobs WHERE status = 'succeeded'`).Scan(&lag).Error; err != nil || lag == nil {
		return -1 // 尚无成功任务
	}
	return *lag
}

// truncateStr 按 rune 截断（字节截断会切碎 UTF-8 导致 last_error 写库失败、任务永久卡死）。
func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
