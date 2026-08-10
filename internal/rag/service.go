package rag

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"aiops/internal/embedding"
	"aiops/internal/metrics"
	"aiops/internal/util"

	"gorm.io/gorm"
)

// Options RAG 服务装配参数（cmd/aiops/main.go 提供）。
type Options struct {
	Emb              embedding.Provider // nil → keyword-only（无向量路）
	IndexWorkers     int                // 索引 worker 并发数（默认 2）
	BatchSize        int                // 批量 embedding 分片（默认 32）
	IndexVersion     string             // 在线索引版本（换模型/维度必须递增并重建）
	Interval         time.Duration      // 索引轮询间隔（默认 5s）
	ReconcileEvery   time.Duration      // 对账间隔（默认 10min）
	RetrievalTimeout time.Duration      // 检索总超时（默认 800ms，超时立即降级）
	TopKKeyword      int                // 关键词召回条数（默认 40）
	TopKVector       int                // 向量召回条数（默认 40）
	TopKFinal        int                // 融合后最终条数（默认 6）
	MaxContextTokens int                // 上下文 token 预算（默认 5000）
	Mode             string             // off|readonly|shadow|assist（本期只读）
}

// Service RAG 知识检索服务：索引 worker + 检索器 + 反馈 + 状态观测。
// 默认关闭（main.go 只在 RAGEnabled && mode!=off 时装配）；组件故障全部 fail-open。
type Service struct {
	db       *gorm.DB
	opts     Options
	m        *metrics.Registry
	workerID string

	metaMu    sync.Mutex
	metaCache IndexMeta // 在线索引元数据 30s TTL 缓存（检索热路径减少往返）
	metaAt    time.Time
}

// NewService 创建 RAG 服务。
func NewService(db *gorm.DB, opts Options) *Service {
	if opts.IndexVersion == "" {
		opts.IndexVersion = "v1"
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 32
	}
	if opts.RetrievalTimeout <= 0 {
		opts.RetrievalTimeout = 800 * time.Millisecond
	}
	if opts.TopKKeyword <= 0 {
		opts.TopKKeyword = 40
	}
	if opts.TopKVector <= 0 {
		opts.TopKVector = 40
	}
	if opts.TopKFinal <= 0 {
		opts.TopKFinal = 6
	}
	if opts.MaxContextTokens <= 0 {
		opts.MaxContextTokens = 5000
	}
	return &Service{
		db:       db,
		opts:     opts,
		workerID: "worker-" + util.NewID()[:8],
	}
}

// SetMetrics 装配指标注册表（nil 跳过埋点）。
func (s *Service) SetMetrics(m *metrics.Registry) { s.m = m }

// indexMeta 在线索引元数据（30s TTL 缓存；索引写入会更新，重建后 30s 内可观察到）。
func (s *Service) indexMeta() IndexMeta {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if time.Since(s.metaAt) < 30*time.Second {
		return s.metaCache
	}
	s.metaCache = LoadIndexMeta(s.db)
	s.metaAt = time.Now()
	return s.metaCache
}

// TraceHasDocument 反馈校验：documentId 必须属于该事故最近检索的候选/入选集合。
// trace 存的是 chunk_id 集合，反馈传的是 document_id——用 EXISTS 反查该文档任一
// chunk 是否出现在候选/入选集中（防同租户拿任意文档 ID 误 quarantine；§12.3）。
func (s *Service) TraceHasDocument(ctx context.Context, tenantID, incidentID, documentID string) bool {
	var n int64
	err := s.db.WithContext(ctx).Raw(`
		SELECT count(*) FROM rag_traces t
		WHERE t.tenant_id = ? AND t.incident_id = ?
		  AND (
			EXISTS (SELECT 1 FROM knowledge_chunks c
				WHERE c.document_id = ? AND c.id::text IN (SELECT jsonb_array_elements_text(t.candidate_ids)))
			OR EXISTS (SELECT 1 FROM knowledge_chunks c
				WHERE c.document_id = ? AND c.id::text IN (SELECT jsonb_array_elements_text(t.selected_ids)))
		  )`,
		tenantID, incidentID, documentID, documentID).Scan(&n).Error
	return err == nil && n > 0
}

// EnqueueIncident 事故索引入队（incident.IndexHook 签名：结案/解决时同事务调用）。
func (s *Service) EnqueueIncident(ctx context.Context, tx *gorm.DB, tenantID, sourceID string, version int64, op string) error {
	return EnqueueJob(ctx, tx, tenantID, SourceIncident, sourceID, version, op)
}

// EnqueuePlaybook 剧本索引入队（playbook.IndexHook 签名：CRUD 同事务调用）。
func (s *Service) EnqueuePlaybook(ctx context.Context, tx *gorm.DB, tenantID, sourceID string, version int64, op string) error {
	return EnqueueJob(ctx, tx, tenantID, SourcePlaybook, sourceID, version, op)
}

// EmbeddingEnabled 向量路是否可用（配置了 Provider）。
func (s *Service) EmbeddingEnabled() bool { return s.opts.Emb != nil }

// ModelID 当前 embedding 模型（keyword-only 返回 ""）。
func (s *Service) ModelID() string {
	if s.opts.Emb == nil {
		return ""
	}
	return s.opts.Emb.ModelID()
}

// DocumentStats 知识库文档级统计（KEEPON M3 状态展示）。
type DocumentStats struct {
	Total        int            `json:"total"`
	Active       int            `json:"active"`
	Superseded   int            `json:"superseded"`
	Deleted      int            `json:"deleted"`
	Failed       int            `json:"failed"`
	Quarantined  int            `json:"quarantined"`
	Chunks       int            `json:"chunks"`
	Embeddings   int            `json:"embeddings"`
	BySourceType map[string]int `json:"bySourceType,omitempty"` // active 文档按来源分布
}

// StatusInfo 检索/索引状态（管理 API 与运维）。
type StatusInfo struct {
	Enabled            bool          `json:"enabled"`
	Mode               string        `json:"mode"`
	EmbeddingDriver    string        `json:"embeddingDriver"`
	EmbeddingModel     string        `json:"embeddingModel"`
	EmbeddingDimension int           `json:"embeddingDimension"`
	ConfiguredVersion  string        `json:"configuredVersion"`
	OnlineModel        string        `json:"onlineModel"` // 知识库现存向量模型（空表为空）
	OnlineVersion      string        `json:"onlineVersion"`
	OnlineDimension    int           `json:"onlineDimension"`
	VectorEnabled      bool          `json:"vectorEnabled"` // 模型/维度/版本一致才可用
	VectorReadyReason  string        `json:"vectorReadyReason,omitempty"`
	Backlog            int           `json:"backlog"`
	FailedJobs         int           `json:"failedJobs"`
	DeadJobs           int           `json:"deadJobs"`
	LastSuccessAt      *time.Time    `json:"lastSuccessAt,omitempty"`
	LagSeconds         float64       `json:"lagSeconds"`
	Documents          DocumentStats `json:"documents"`
}

// Status 汇总状态（向量一致性检查：模型/索引版本不匹配时禁用向量路，允许关键词降级）。
func (s *Service) Status(ctx context.Context) StatusInfo {
	info := StatusInfo{
		Enabled:           true,
		Mode:              s.opts.Mode,
		ConfiguredVersion: s.opts.IndexVersion,
		OnlineModel:       "",
		OnlineVersion:     "",
		OnlineDimension:   0,
		VectorEnabled:     false,
		VectorReadyReason: "embedding 未配置（keyword-only）",
		LagSeconds:        s.LagSeconds(),
	}
	if s.opts.Emb != nil {
		info.EmbeddingModel = s.opts.Emb.ModelID()
		info.EmbeddingDimension = s.opts.Emb.Dimension()
		meta := s.indexMeta()
		info.OnlineModel, info.OnlineVersion, info.OnlineDimension = meta.ModelID, meta.IndexVersion, meta.Dimension
		ready := true
		switch {
		case info.OnlineModel == "":
			ready, info.VectorReadyReason = false, "知识库尚无向量（索引 worker 在建）"
		case info.OnlineModel != info.EmbeddingModel:
			ready, info.VectorReadyReason = false, "模型不匹配（在线 "+info.OnlineModel+"，配置 "+info.EmbeddingModel+"），需重建索引版本"
		case info.OnlineVersion != info.ConfiguredVersion:
			ready, info.VectorReadyReason = false, "索引版本不匹配（在线 "+info.OnlineVersion+"，配置 "+info.ConfiguredVersion+"），需重建"
		case info.OnlineDimension > 0 && info.OnlineDimension != s.opts.Emb.Dimension():
			ready, info.VectorReadyReason = false, "向量维度不一致（在线 "+itoaN(info.OnlineDimension)+"，配置 "+itoaN(info.EmbeddingDimension)+"），需重建"
		}
		info.VectorEnabled = ready
		if ready {
			info.VectorReadyReason = ""
		}
	}
	counts := jobCounts(ctx, s.db)
	info.Backlog = counts[JobPending] + counts[JobRunning]
	info.FailedJobs = counts[JobFailed]
	info.DeadJobs = counts[JobDead]
	var last *time.Time
	if err := s.db.WithContext(ctx).Raw(
		`SELECT MAX(updated_at) FROM knowledge_jobs WHERE status = 'succeeded'`).Scan(&last).Error; err == nil {
		info.LastSuccessAt = last
	}
	if info.LagSeconds < 0 {
		info.LagSeconds = 0
	}
	info.Documents = s.docStats(ctx)
	return info
}

// docStats 文档级统计（KEEPON M3：状态分布 + chunk/embedding 计数 + active 来源分布）。
func (s *Service) docStats(ctx context.Context) DocumentStats {
	var st DocumentStats
	type row struct {
		Status string
		N      int
	}
	var rows []row
	if err := s.db.WithContext(ctx).Raw(
		`SELECT status, count(*) AS n FROM knowledge_documents GROUP BY status`).Scan(&rows).Error; err != nil {
		return st
	}
	for _, r := range rows {
		st.Total += r.N
		switch r.Status {
		case DocActive:
			st.Active = r.N
		case DocSuperseded:
			st.Superseded = r.N
		case DocDeleted:
			st.Deleted = r.N
		case DocFailed:
			st.Failed = r.N
		case DocQuarantined:
			st.Quarantined = r.N
		}
	}
	_ = s.db.WithContext(ctx).Raw(`SELECT count(*) FROM knowledge_chunks`).Scan(&st.Chunks).Error
	_ = s.db.WithContext(ctx).Raw(`SELECT count(*) FROM knowledge_embeddings`).Scan(&st.Embeddings).Error
	type srcRow struct {
		SourceType string
		N          int
	}
	var srcs []srcRow
	if err := s.db.WithContext(ctx).Raw(
		`SELECT source_type, count(*) AS n FROM knowledge_documents WHERE status = 'active' GROUP BY source_type`).Scan(&srcs).Error; err == nil {
		st.BySourceType = map[string]int{}
		for _, r := range srcs {
			st.BySourceType[r.SourceType] = r.N
		}
	}
	return st
}

func itoaN(n int) string { return strconv.Itoa(n) }

// DocStatus 某来源最新一版知识文档的状态（KEEPON M3 结案卡片展示）。
type DocStatus struct {
	DocumentID         string    `json:"documentId"`
	Title              string    `json:"title"`
	TrustLevel         string    `json:"trustLevel"`
	VerificationStatus string    `json:"verificationStatus"`
	Status             string    `json:"status"` // active|superseded|deleted|failed|quarantined
	SourceVersion      int64     `json:"sourceVersion"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// JobStatus 某来源最近索引任务（展示"索引中"/失败原因）。
type JobStatus struct {
	Operation     string    `json:"operation"`
	Status        string    `json:"status"`
	SourceVersion int64     `json:"sourceVersion"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"lastError,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// KnowledgeState 结案知识状态（三态：已解决 / 结案已填写 / 知识已索引）。
type KnowledgeState struct {
	Resolved            bool       `json:"resolved"`
	ResolutionFilled    bool       `json:"resolutionFilled"`
	Indexed             bool       `json:"indexed"`
	ResolutionVersion   int64      `json:"resolutionVersion,omitempty"`
	ResolutionUpdatedAt *time.Time `json:"resolutionUpdatedAt,omitempty"`
	Document            *DocStatus `json:"document,omitempty"`
	Job                 *JobStatus `json:"job,omitempty"`
}

// KnowledgeStatus 某事故的结案→发布→索引三态（KEEPON M3；4 条强制 tenant 小查询；
// 查询失败返回零值不阻塞事故详情——该信息只影响卡片展示）。
func (s *Service) KnowledgeStatus(ctx context.Context, tenantID, incidentID string) *KnowledgeState {
	st := &KnowledgeState{}
	// 1) 事故是否已解决
	var status string
	if err := s.db.WithContext(ctx).Raw(
		`SELECT status FROM incidents WHERE tenant_id = ? AND id = ?`, tenantID, incidentID).Scan(&status).Error; err == nil {
		st.Resolved = status == "resolved"
	}
	// 2) 结案是否已填写（版本摘要；count 判定——Raw Scan 无行时 err 为 nil）
	var filled int64
	if err := s.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM incident_resolutions WHERE tenant_id = ? AND incident_id = ?`,
		tenantID, incidentID).Scan(&filled).Error; err == nil {
		st.ResolutionFilled = filled > 0
	}
	if st.ResolutionFilled {
		var ver struct {
			Version   int64
			UpdatedAt time.Time
		}
		if err := s.db.WithContext(ctx).Raw(
			`SELECT version, updated_at FROM incident_resolutions WHERE tenant_id = ? AND incident_id = ?`,
			tenantID, incidentID).Scan(&ver).Error; err == nil {
			st.ResolutionVersion = ver.Version
			st.ResolutionUpdatedAt = &ver.UpdatedAt
		}
	}
	// 3) 最新一版文档（任意状态；active → indexed）。Raw Scan 无行时 err 为 nil——
	// 以主键非空判定是否有行（避免零值对象进响应）。
	var d DocStatus
	if err := s.db.WithContext(ctx).Raw(
		`SELECT id AS document_id, title, trust_level, verification_status, status, source_version, updated_at
		 FROM knowledge_documents
		 WHERE tenant_id = ? AND source_type = 'incident' AND source_id = ?
		 ORDER BY source_version DESC LIMIT 1`, tenantID, incidentID).Scan(&d).Error; err == nil && d.DocumentID != "" {
		st.Document = &d
		st.Indexed = d.Status == DocActive
	}
	// 4) 最近索引任务（展示"索引中"/失败原因）
	var j JobStatus
	if err := s.db.WithContext(ctx).Raw(
		`SELECT operation, status, source_version, attempts, COALESCE(last_error,'') AS last_error, updated_at
		 FROM knowledge_jobs
		 WHERE tenant_id = ? AND source_type = 'incident' AND source_id = ?
		 ORDER BY created_at DESC LIMIT 1`, tenantID, incidentID).Scan(&j).Error; err == nil && j.Operation != "" {
		st.Job = &j
	}
	return st
}

// AddFeedback 记录人工反馈；rating=unsafe 时对目标文档立即 quarantine（退出召回，数据保留待复核）。
func (s *Service) AddFeedback(ctx context.Context, tenantID, incidentID string, fb *RAGFeedback) (quarantined bool, err error) {
	switch fb.Rating {
	case RatingUseful, RatingNotUseful, RatingWrong, RatingUnsafe:
	default:
		return false, errors.New("rating 必须为 useful|not_useful|wrong|unsafe")
	}
	if fb.ID == "" {
		fb.ID = util.NewID()
	}
	fb.TenantID, fb.IncidentID, fb.CreatedAt = tenantID, incidentID, time.Now().UTC()
	if err := s.db.WithContext(ctx).Create(fb).Error; err != nil {
		return false, err
	}
	if s.m != nil {
		s.m.Inc("aiops_rag_feedback_total", fb.Rating)
	}
	if fb.Rating == RatingUnsafe && fb.DocumentID != "" {
		if err := quarantineDocument(ctx, s.db, tenantID, fb.DocumentID); err != nil {
			slog.Warn("rag: quarantine doc", "document", fb.DocumentID, "err", err)
		} else {
			quarantined = true
			slog.Warn("rag: document quarantined by unsafe feedback", "document", fb.DocumentID, "incident", incidentID)
			// KEEPON M3 联动：runbook 知识源同步置位（展示辅助；重新发布=新版本天然复活）
			if err := s.db.WithContext(ctx).Exec(
				`UPDATE knowledge_sources s SET status = 'quarantined', updated_at = now()
				 FROM knowledge_documents d
				 WHERE d.id = ? AND d.source_type = 'runbook' AND s.id = d.source_id`, fb.DocumentID).Error; err != nil {
				slog.Warn("rag: quarantine source sync", "document", fb.DocumentID, "err", err)
			}
		}
	}
	return quarantined, nil
}

// TracesOf 某事故的检索轨迹（管理 API；按 tenant 隔离，最新在前）。
func (s *Service) TracesOf(ctx context.Context, tenantID, incidentID string, limit int) ([]RAGTrace, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var items []RAGTrace
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND incident_id = ?", tenantID, incidentID).
		Order("created_at DESC").Limit(limit).Find(&items).Error
	return items, err
}

// ResolutionsOf 批量取候选文档的结案数据（相似事故展示用；按 source_id 一次查询）。
func (s *Service) ResolutionsOf(ctx context.Context, tenantID string, incidentIDs []string) (map[string]*ResolutionInfo, error) {
	out := map[string]*ResolutionInfo{}
	if len(incidentIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		IncidentID         string
		ConfirmedRootCause string
		Resolution         string
		Outcome            string
		VerificationStatus string
		ResolvedAt         time.Time
	}
	if err := s.db.WithContext(ctx).Raw(
		`SELECT incident_id, confirmed_root_cause, resolution, outcome, verification_status, resolved_at
		 FROM incident_resolutions WHERE tenant_id = ? AND incident_id IN ?`, tenantID, incidentIDs).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.IncidentID] = &ResolutionInfo{
			ConfirmedRootCause: r.ConfirmedRootCause,
			Resolution:         r.Resolution,
			Outcome:            r.Outcome,
			VerificationStatus: r.VerificationStatus,
			ResolvedAt:         r.ResolvedAt,
		}
	}
	return out, nil
}
