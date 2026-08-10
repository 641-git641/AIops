package rag

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// indexer.go：持久化知识索引 worker（RAG_PLAN §8.4）。
// 任务表驱动（knowledge_jobs），SKIP LOCKED 并发领取、指数退避、dead-letter、
// 租约复活、启动回填与定期 reconciliation。索引延迟可观测，不允许静默失步。

const (
	defaultIndexInterval  = 5 * time.Second
	defaultReconcileEvery = 10 * time.Minute
)

// processClaimed 处理单个任务（worker goroutine 内）。
func (s *Service) processClaimed(ctx context.Context, job *KnowledgeJob) {
	start := time.Now()
	err := s.processJob(ctx, job)
	if s.m != nil {
		s.m.Observe("aiops_rag_index_job_duration_seconds", time.Since(start).Seconds())
	}
	if err != nil {
		if ferr := FailJob(ctx, s.db, job.ID, job.Attempts, err); ferr != nil {
			slog.Error("rag: fail job", "job", job.ID, "err", ferr)
		}
		s.incIndexJob(JobFailed, job.Operation)
		return
	}
	if err := CompleteJob(ctx, s.db, job.ID); err != nil {
		slog.Error("rag: complete job", "job", job.ID, "err", err)
	}
	s.incIndexJob(JobSucceeded, job.Operation)
}

// processJob 按操作分发：delete → 删除传播；upsert/rebuild → 构建+写入。
// 未知操作返回错误（进 failed/dead 暴露数据/配置问题，不静默成功）。
func (s *Service) processJob(ctx context.Context, job *KnowledgeJob) error {
	switch job.Operation {
	case OpDelete:
		return deleteSource(ctx, s.db, job.TenantID, job.SourceType, job.SourceID)
	case OpUpsert, OpRebuild:
		return s.processUpsert(ctx, job)
	default:
		return errors.New("rag: unknown job operation " + job.Operation)
	}
}

// processUpsert 构建源文档 → 脱敏 → 切片 → embedding → 写入。
func (s *Service) processUpsert(ctx context.Context, job *KnowledgeJob) error {
	var doc DocBuild
	switch job.SourceType {
	case SourceIncident:
		info, err := loadIncidentSource(ctx, s.db, job.TenantID, job.SourceID)
		if err != nil {
			return err
		}
		if info == nil {
			return deleteSource(ctx, s.db, job.TenantID, job.SourceType, job.SourceID) // 源已消失
		}
		doc = BuildIncident(*info)
	case SourcePlaybook:
		info, err := loadPlaybookSource(ctx, s.db, job.TenantID, job.SourceID)
		if err != nil {
			return err
		}
		if info == nil {
			return deleteSource(ctx, s.db, job.TenantID, job.SourceType, job.SourceID)
		}
		doc = BuildPlaybook(*info)
	case SourceRunbook:
		// KEEPON M3：个人知识源（runbook/SOP）——源消失/停用 → 删除传播
		info, err := loadRunbookSource(ctx, s.db, job.TenantID, job.SourceID)
		if err != nil {
			return err
		}
		if info == nil {
			return deleteSource(ctx, s.db, job.TenantID, job.SourceType, job.SourceID)
		}
		doc = BuildRunbook(*info)
	default:
		return deleteSource(ctx, s.db, job.TenantID, job.SourceType, job.SourceID)
	}
	// 不入库（未解决事故 / rejected / 停用剧本）→ 按删除传播
	if !doc.Index {
		return deleteSource(ctx, s.db, job.TenantID, job.SourceType, job.SourceID)
	}
	doc.Source.SourceVersion = job.SourceVersion // 以任务版本为准（钩子与回填统一）
	return s.writeDocument(ctx, job, &doc)
}

// writeDocument 脱敏 → 切片 → 逐 chunk 幂等写入 + embedding（相同 checksum 不重复 Embedding）。
func (s *Service) writeDocument(ctx context.Context, job *KnowledgeJob, doc *DocBuild) error {
	// 脱敏失败 → 任务失败（绝不把未脱敏正文提交外部模型）
	sres, err := Sanitize(doc.Body)
	if err != nil {
		return err
	}
	doc.Body = sres.Text
	if sres.Injection {
		slog.Warn("rag: 文档命中提示注入风险标签", "source", job.SourceType, "sourceID", job.SourceID, "redacted", sres.Redacted)
	}
	chunks := ChunkBody(doc.Body, chunkSource{
		Title: doc.Title, Service: doc.Service, Severity: doc.Severity, Version: doc.Source.SourceVersion,
	})
	if len(chunks) == 0 {
		return nil
	}
	docID, quarantined, err := upsertDocument(ctx, s.db, *doc, doc.Body, sha256Hex(doc.Body))
	if err != nil {
		return err
	}
	if quarantined {
		// 文档被 unsafe 反馈隔离：保持退出召回（粘性），任务按成功跳过，不复活不重写
		slog.Warn("rag: skip index of quarantined document", "source", job.SourceType, "sourceID", job.SourceID)
		return nil
	}
	// 旧版本退出在线检索（RAG_PLAN §5.4：源更新生成新版本，旧版本 superseded 保留审计）
	if err := supersedeOlderVersions(ctx, s.db, job.TenantID, job.SourceType, job.SourceID, job.SourceVersion); err != nil {
		return err
	}
	// 逐 chunk：新 chunk 才需要 embedding；既有 chunk 但缺向量（如换 index_version 重建）补嵌
	type embedTask struct {
		chunkID string
		content string
	}
	var pending []embedTask
	for i := range chunks {
		chunk := &chunks[i]
		cid, inserted, err := insertChunk(ctx, s.db, job.TenantID, docID, *chunk)
		if err != nil {
			return err
		}
		needEmbed := inserted
		if !needEmbed && s.opts.Emb != nil {
			ok, err := embeddingExists(ctx, s.db, cid, s.opts.Emb.ModelID(), s.opts.IndexVersion)
			if err != nil {
				return err
			}
			needEmbed = !ok
		}
		if needEmbed && s.opts.Emb != nil {
			pending = append(pending, embedTask{chunkID: cid, content: chunk.Content})
		}
	}
	if len(pending) == 0 || s.opts.Emb == nil {
		return nil // keyword-only 或全部已有向量
	}
	// 批量 embedding（分片；相同 checksum 不重复 Embedding 已由 insertChunk 幂等保证）
	batch := s.opts.BatchSize
	if batch <= 0 {
		batch = 32
	}
	for start := 0; start < len(pending); start += batch {
		end := min(start+batch, len(pending))
		texts := make([]string, 0, end-start)
		for _, t := range pending[start:end] {
			texts = append(texts, t.content)
		}
		vs, err := s.embedMany(ctx, texts)
		if err != nil {
			return err
		}
		for i, v := range vs {
			if err := insertEmbedding(ctx, s.db, job.TenantID, pending[start+i].chunkID,
				s.opts.Emb.ModelID(), s.opts.IndexVersion, Vec(v)); err != nil {
				return err
			}
		}
	}
	return nil
}

// embedMany 批量 embedding（指标埋点：按批计数）。
func (s *Service) embedMany(ctx context.Context, texts []string) ([][]float32, error) {
	if s.m != nil && s.opts.Emb != nil {
		s.m.Inc("aiops_rag_embedding_requests_total", s.opts.Emb.ModelID(), "success")
		s.m.Inc("aiops_rag_embedding_tokens_total", s.opts.Emb.ModelID())
	}
	vs, err := s.opts.Emb.EmbedDocuments(ctx, texts)
	if err != nil && s.m != nil {
		s.m.Inc("aiops_rag_embedding_requests_total", s.opts.Emb.ModelID(), "error")
	}
	return vs, err
}

// embedOne 单条 embedding（指标埋点）。
func (s *Service) embedOne(ctx context.Context, text string) ([]float32, error) {
	if s.m != nil && s.opts.Emb != nil {
		s.m.Inc("aiops_rag_embedding_requests_total", s.opts.Emb.ModelID(), "success")
		s.m.Inc("aiops_rag_embedding_tokens_total", s.opts.Emb.ModelID())
	}
	vec, err := s.opts.Emb.EmbedQuery(ctx, text)
	if err != nil && s.m != nil {
		s.m.Inc("aiops_rag_embedding_requests_total", s.opts.Emb.ModelID(), "error")
	}
	return vec, err
}

// RunIndexer 启动索引 worker：N 个处理 goroutine + 领取/调度 ticker + 定期 reconciliation。
// ctx 取消时优雅退出（已领取任务处理完，未领取任务靠租约复活）。
func (s *Service) RunIndexer(ctx context.Context) {
	workers := s.opts.IndexWorkers
	if workers <= 0 {
		workers = 2
	}
	jobs := make(chan *KnowledgeJob, 64)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				s.processClaimed(ctx, j)
			}
		}()
	}
	interval := s.opts.Interval
	if interval <= 0 {
		interval = defaultIndexInterval
	}
	reconcileEvery := s.opts.ReconcileEvery
	if reconcileEvery <= 0 {
		reconcileEvery = defaultReconcileEvery
	}
	lastReconcile := time.Now()
	go func() {
		defer close(jobs)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		s.claimAndDispatch(ctx, jobs) // 启动立即处理一轮
		for {
			select {
			case <-ticker.C:
				s.claimAndDispatch(ctx, jobs)
				if time.Since(lastReconcile) >= reconcileEvery {
					if n, err := s.Reconcile(ctx); err != nil {
						slog.Warn("rag: reconcile", "err", err)
					} else if n > 0 {
						slog.Info("rag: reconcile enqueued", "jobs", n)
					}
					lastReconcile = time.Now()
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	slog.Info("rag: indexer stopped")
}

// claimAndDispatch 领取一批任务分发到处理通道。
func (s *Service) claimAndDispatch(ctx context.Context, jobs chan<- *KnowledgeJob) {
	claimed, err := ClaimJobs(ctx, s.db, s.workerID, maxClaimBatch)
	if err != nil {
		slog.Warn("rag: claim jobs", "err", err)
		return
	}
	for i := range claimed {
		j := claimed[i]
		select {
		case jobs <- &j:
		case <-ctx.Done():
			return
		}
	}
}

// Backfill 启动回填：已解决事故 + 已启用剧本（幂等，ON CONFLICT DO NOTHING）。
func (s *Service) Backfill(ctx context.Context) (int, error) {
	n := 0
	// 已解决事故（结案版本为 0 表示尚无结案数据：先建 T1 参考，补录后版本升级）
	res := s.db.WithContext(ctx).Exec(`
		INSERT INTO knowledge_jobs (id, tenant_id, operation, source_type, source_id, source_version,
			status, attempts, next_run_at, created_at, updated_at)
		SELECT gen_random_uuid()::text, i.tenant_id, 'upsert', 'incident', i.id, COALESCE(r.version, 0),
			'pending', 0, now(), now(), now()
		FROM incidents i
		LEFT JOIN incident_resolutions r ON r.tenant_id = i.tenant_id AND r.incident_id = i.id
		WHERE i.status = 'resolved'
		ON CONFLICT (tenant_id, source_type, source_id, source_version, operation) DO NOTHING`)
	if res.Error != nil {
		return n, res.Error
	}
	n += int(res.RowsAffected)
	// 已启用剧本
	var pbs []struct {
		TenantID  string
		ID        string
		UpdatedAt time.Time
	}
	if err := s.db.WithContext(ctx).Raw(
		`SELECT tenant_id, id, updated_at FROM playbooks WHERE enabled = true`).Scan(&pbs).Error; err != nil {
		return n, err
	}
	for _, p := range pbs {
		if err := EnqueueJob(ctx, s.db, p.TenantID, SourcePlaybook, p.ID, p.UpdatedAt.UnixNano(), OpUpsert); err != nil {
			return n, err
		}
		n++
	}
	// 已发布知识源（KEEPON M3：runbook/SOP）
	var srcs []struct {
		TenantID string
		ID       string
		Version  int64
	}
	if err := s.db.WithContext(ctx).Raw(
		`SELECT tenant_id, id, version FROM knowledge_sources WHERE status = 'published'`).Scan(&srcs).Error; err != nil {
		return n, err
	}
	for _, src := range srcs {
		if err := EnqueueJob(ctx, s.db, src.TenantID, SourceRunbook, src.ID, src.Version, OpUpsert); err != nil {
			return n, err
		}
		n++
	}
	slog.Info("rag: backfill", "enqueued", n)
	return n, nil
}

// Reconcile 定期对账：修复钩子漏单与源/索引失步（幂等入队）。
func (s *Service) Reconcile(ctx context.Context) (int, error) {
	n := 0
	// 1) 剧本失步：启用无文档 / 文档陈旧 / 停用仍有文档（版本在 Go 侧取 UnixNano，避免 SQL 精度损失）
	var pbs []struct {
		TenantID  string
		ID        string
		Enabled   bool
		UpdatedAt time.Time
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT p.tenant_id, p.id, p.enabled, p.updated_at
		FROM playbooks p
		LEFT JOIN knowledge_documents d ON d.tenant_id = p.tenant_id AND d.source_type = 'playbook'
			AND d.source_id = p.id AND d.status = 'active'
		WHERE (p.enabled AND (d.id IS NULL OR d.updated_at < p.updated_at))
		   OR (NOT p.enabled AND d.id IS NOT NULL)`).Scan(&pbs).Error; err != nil {
		return n, err
	}
	for _, p := range pbs {
		op, version := OpUpsert, p.UpdatedAt.UnixNano()
		if !p.Enabled {
			op, version = OpDelete, 0
		}
		if err := EnqueueJob(ctx, s.db, p.TenantID, SourcePlaybook, p.ID, version, op); err != nil {
			return n, err
		}
		n++
	}
	// 2) 事故失步：已解决但无 active 文档，或结案版本高于文档版本
	type incidentSync struct {
		TenantID string
		ID       string
		Version  int64
	}
	var incs []incidentSync
	if err := s.db.WithContext(ctx).Raw(`
		SELECT i.tenant_id, i.id, COALESCE(r.version, 0) AS version
		FROM incidents i
		LEFT JOIN incident_resolutions r ON r.tenant_id = i.tenant_id AND r.incident_id = i.id
		LEFT JOIN knowledge_documents d ON d.tenant_id = i.tenant_id AND d.source_type = 'incident'
			AND d.source_id = i.id AND d.status = 'active'
		WHERE i.status = 'resolved' AND (d.id IS NULL OR d.source_version < COALESCE(r.version, 0))`).
		Scan(&incs).Error; err != nil {
		return n, err
	}
	for _, in := range incs {
		if err := EnqueueJob(ctx, s.db, in.TenantID, SourceIncident, in.ID, in.Version, OpUpsert); err != nil {
			return n, err
		}
		n++
	}
	// 3) 孤儿文档：active 文档的源已不存在（钩子漏发 delete）
	var orphs []struct {
		TenantID   string
		SourceType string
		SourceID   string
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT d.tenant_id, d.source_type, d.source_id
		FROM knowledge_documents d
		LEFT JOIN playbooks p ON p.tenant_id = d.tenant_id AND p.id = d.source_id
		WHERE d.source_type = 'playbook' AND d.status = 'active' AND p.id IS NULL
		UNION ALL
		SELECT d.tenant_id, d.source_type, d.source_id
		FROM knowledge_documents d
		LEFT JOIN incidents i ON i.tenant_id = d.tenant_id AND i.id = d.source_id
		WHERE d.source_type = 'incident' AND d.status = 'active' AND i.id IS NULL
		UNION ALL
		SELECT d.tenant_id, d.source_type, d.source_id
		FROM knowledge_documents d
		LEFT JOIN knowledge_sources s ON s.tenant_id = d.tenant_id AND s.id = d.source_id
		WHERE d.source_type = 'runbook' AND d.status = 'active' AND s.id IS NULL`).
		Scan(&orphs).Error; err != nil {
		return n, err
	}
	for _, o := range orphs {
		if err := EnqueueJob(ctx, s.db, o.TenantID, o.SourceType, o.SourceID, 0, OpDelete); err != nil {
			return n, err
		}
		n++
	}
	// 4) 多 active 版本并存（旧版本未正确 superseded，如钩子漏发升级任务）→ 重排最新版本
	var dupes []struct {
		TenantID   string
		SourceType string
		SourceID   string
		MaxVersion int64
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT tenant_id, source_type, source_id, MAX(source_version) AS max_version
		FROM knowledge_documents
		WHERE status = 'active'
		GROUP BY tenant_id, source_type, source_id
		HAVING COUNT(*) > 1`).Scan(&dupes).Error; err != nil {
		return n, err
	}
	for _, d := range dupes {
		if err := EnqueueJob(ctx, s.db, d.TenantID, d.SourceType, d.SourceID, d.MaxVersion, OpUpsert); err != nil {
			return n, err
		}
		n++
	}
	// 5) 知识源失步（KEEPON M3）：published 无 active 文档 / 文档版本落后 / 已删除仍有 active 文档
	var srcs []struct {
		TenantID string
		ID       string
		Status   string
		Version  int64
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT s.tenant_id, s.id, s.status, s.version
		FROM knowledge_sources s
		LEFT JOIN knowledge_documents d ON d.tenant_id = s.tenant_id AND d.source_type = 'runbook'
			AND d.source_id = s.id AND d.status = 'active'
		WHERE (s.status = 'published' AND (d.id IS NULL OR d.source_version < s.version))
		   OR (s.status = 'deleted' AND d.id IS NOT NULL)`).Scan(&srcs).Error; err != nil {
		return n, err
	}
	for _, src := range srcs {
		op, version := OpUpsert, src.Version
		if src.Status == SrcDeleted {
			op, version = OpDelete, 0
		}
		if err := EnqueueJob(ctx, s.db, src.TenantID, SourceRunbook, src.ID, version, op); err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		slog.Info("rag: reconcile out-of-sync", "jobs", n)
	}
	return n, nil
}

// ReindexAll 全量重建：对知识库已有全部源入队 rebuild（换 index_version 后旧向量自然退出，
// 新版本向量重算；同版本重建利用 checksum 幂等跳过）。
func (s *Service) ReindexAll(ctx context.Context) (int, error) {
	var srcs []struct {
		TenantID      string
		SourceType    string
		SourceID      string
		SourceVersion int64
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT DISTINCT tenant_id, source_type, source_id, source_version
		FROM knowledge_documents`).Scan(&srcs).Error; err != nil {
		return 0, err
	}
	n := 0
	for _, s2 := range srcs {
		if err := EnqueueJob(ctx, s.db, s2.TenantID, s2.SourceType, s2.SourceID, s2.SourceVersion, OpRebuild); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// incIndexJob 索引任务指标（status 按成功/失败结算，operation 带任务类型）。
func (s *Service) incIndexJob(status, op string) {
	if s.m != nil {
		s.m.Inc("aiops_rag_index_jobs_total", status, op)
	}
}

// Backlog 当前积压（pending+running）。观测路径带 5s 超时，DB 挂起时不阻塞 /metrics。
func (s *Service) Backlog() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	counts := jobCounts(ctx, s.db)
	return counts[JobPending] + counts[JobRunning]
}

// LagSeconds 索引滞后（无成功任务返回 0）。
func (s *Service) LagSeconds() float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lag := indexLagSeconds(ctx, s.db)
	if lag < 0 {
		return 0
	}
	return lag
}
