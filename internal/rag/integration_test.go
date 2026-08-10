package rag_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"aiops/internal/alert"
	"aiops/internal/embedding"
	"aiops/internal/incident"
	"aiops/internal/playbook"
	"aiops/internal/rag"
	"aiops/internal/store"

	"gorm.io/gorm"
)

// DB 门控集成测试：覆盖真实 SQL 路径（SKIP LOCKED 领取 / vector / tsvector / 删除传播 /
// 跨租户隔离 / unsafe 反馈 quarantine）。PostgreSQL 未运行（docker compose）时跳过。
// 表结构用 AutoMigrate 幂等补齐；向量列维度取在线列（无则建 64）。

const itTenant = "rag-it-tenant"

func openRagTestDB(t *testing.T) (*gorm.DB, int) {
	t.Helper()
	db, err := store.Open(store.DefaultDSN, store.Options{})
	if err != nil {
		t.Skipf("postgres 未运行（docker compose），跳过集成断言: %v", err)
	}
	if err := db.AutoMigrate(
		&rag.KnowledgeDocument{}, &rag.KnowledgeChunk{}, &rag.KnowledgeEmbedding{},
		&rag.KnowledgeJob{}, &rag.RAGTrace{}, &rag.RAGFeedback{},
		&incident.Incident{}, &incident.Resolution{}, &incident.AlertToIncident{},
		&alert.Alert{}, &playbook.Playbook{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dim := rag.LoadIndexMeta(db).Dimension
	if dim == 0 {
		dim = 64
	}
	if err := rag.EnsureSchema(db, rag.SchemaOptions{Dimension: dim}); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return db, dim
}

func cleanupIT(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, tbl := range []string{"knowledge_embeddings", "knowledge_chunks", "knowledge_documents",
		"knowledge_jobs", "rag_traces", "rag_feedback", "alert_to_incidents", "incident_resolutions",
		"incidents", "alerts", "playbooks"} {
		if err := db.Exec("DELETE FROM "+tbl+" WHERE tenant_id = ?", itTenant).Error; err != nil {
			t.Logf("cleanup %s: %v", tbl, err)
		}
	}
}

func seedIncident(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	if err := db.Exec(`
		INSERT INTO incidents (id, tenant_id, name, summary, severity, status, source, created_at)
		VALUES (?, ?, 'order 服务连接池耗尽', 'order api latency spike db_pool_exhausted ECONNREFUSED',
			'critical', 'resolved', 'agent', now())`, id, itTenant).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO incident_resolutions (id, tenant_id, incident_id, confirmed_root_cause, resolution,
			outcome, verification, verification_status, resolved_by, resolved_at, updated_at, version)
		VALUES (?, ?, ?, '连接池上限与并发配置不匹配', '回滚配置并重启连接池', 'success',
			'错误率恢复', 'verified', 'oncall', now(), now(), 1)`,
		"res-"+id, itTenant, id).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO alerts (id, tenant_id, provider, provider_type, fingerprint, alert_hash, event,
			name, severity, status, labels, started_at, last_seen_at, created_at, updated_at)
		VALUES (?, ?, 'prometheus', 'prometheus', 'fp-1', 'hash-1', '{}'::jsonb,
			'db_pool_exhausted', 'critical', 'resolved', ?::jsonb, now(), now(), now(), now())`,
		"alert-"+id, itTenant, `{"service":"order","error_code":"ECONNREFUSED"}`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO alert_to_incidents (alert_id, incident_id, tenant_id) VALUES (?, ?, ?)`,
		"alert-"+id, id, itTenant).Error; err != nil {
		t.Fatal(err)
	}
}

func seedPlaybook(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	if err := db.Exec(`
		INSERT INTO playbooks (id, tenant_id, name, level, command_template, blast_radius_factor,
			rollback_command, dry_run_first, enabled, created_at, updated_at)
		VALUES (?, ?, 'rollback', 'L1', 'kubectl rollout undo deployment/{{.service}}', 0.35,
			'kubectl rollout status deployment/{{.service}}', true, true, now(), now())`,
		id, itTenant).Error; err != nil {
		t.Fatal(err)
	}
}

// waitJobs 轮询直到全部任务进入目标状态（默认 succeeded），带超时。
func waitJobs(t *testing.T, db *gorm.DB, status string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := db.Raw(`SELECT count(*) FROM knowledge_jobs WHERE tenant_id = ? AND status = ?`,
			itTenant, status).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n >= 2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	var rows []map[string]any
	db.Raw(`SELECT operation, source_type, status, attempts, last_error FROM knowledge_jobs
		WHERE tenant_id = ? ORDER BY created_at`, itTenant).Scan(&rows)
	t.Fatalf("jobs did not reach %s: %v", status, rows)
}

// waitCond 轮询条件成立（带超时）。
func waitCond(t *testing.T, db *gorm.DB, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", what)
}

// TestVersionSupersedeAndQuarantineSticky 版本演进与隔离粘性（review M1/M2 修复验证）。
func TestVersionSupersedeAndQuarantineSticky(t *testing.T) {
	db, dim := openRagTestDB(t)
	cleanupIT(t, db)
	defer cleanupIT(t, db)

	incID := "it-inc-2"
	seedIncident(t, db, incID)

	svc := rag.NewService(db, rag.Options{
		Emb:              embedding.NewMock(dim),
		IndexVersion:     "v1",
		Interval:         100 * time.Millisecond,
		ReconcileEvery:   time.Hour,
		RetrievalTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go svc.RunIndexer(ctx)

	// v1 入队 → 建成 active
	if err := rag.EnqueueJob(ctx, db, itTenant, rag.SourceIncident, incID, 1, rag.OpUpsert); err != nil {
		t.Fatal(err)
	}
	waitCond(t, db, "v1 active", func() bool {
		var n int64
		db.Raw(`SELECT count(*) FROM knowledge_documents WHERE tenant_id=? AND source_id=? AND source_version=1 AND status='active'`,
			itTenant, incID).Scan(&n)
		return n == 1
	})
	// v2 入队（结案更新）→ v1 必须 superseded、v2 active
	if err := rag.EnqueueJob(ctx, db, itTenant, rag.SourceIncident, incID, 2, rag.OpUpsert); err != nil {
		t.Fatal(err)
	}
	waitCond(t, db, "v1 superseded & v2 active", func() bool {
		var sup, act int64
		db.Raw(`SELECT count(*) FROM knowledge_documents WHERE tenant_id=? AND source_id=? AND source_version=1 AND status='superseded'`,
			itTenant, incID).Scan(&sup)
		db.Raw(`SELECT count(*) FROM knowledge_documents WHERE tenant_id=? AND source_id=? AND source_version=2 AND status='active'`,
			itTenant, incID).Scan(&act)
		return sup == 1 && act == 1
	})
	// 检索只应命中 v2（版本退出在线检索）
	q := rag.BuildQuery(itTenant, "irrelevant-self", "critical", "order api latency spike", "order",
		[]string{"db_pool_exhausted"}, map[string]string{"service": "order"}, time.Now())
	pack := svc.Retrieve(ctx, q)
	for _, e := range pack.Selected {
		if e.SourceID == incID {
			var v int64
			if err := db.Raw(`SELECT source_version FROM knowledge_documents WHERE id = ?`,
				e.DocumentID).Scan(&v).Error; err != nil {
				t.Fatal(err)
			}
			if v != 2 {
				t.Fatalf("retrieval hit non-latest version (v%d): %+v", v, e)
			}
		}
	}

	// quarantine 粘性：unsafe 反馈隔离 v2 → 重入队 upsert 不得复活
	var v2Doc string
	db.Raw(`SELECT id FROM knowledge_documents WHERE tenant_id=? AND source_id=? AND source_version=2`,
		itTenant, incID).Scan(&v2Doc)
	if _, err := svc.AddFeedback(ctx, itTenant, incID, &rag.RAGFeedback{
		DocumentID: v2Doc, Rating: rag.RatingUnsafe,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rag.EnqueueJob(ctx, db, itTenant, rag.SourceIncident, incID, 2, rag.OpUpsert); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // 等 worker 处理
	var st string
	db.Raw(`SELECT status FROM knowledge_documents WHERE id=?`, v2Doc).Scan(&st)
	if st != rag.DocQuarantined {
		t.Fatalf("quarantined doc resurrected by re-enqueue: status=%s", st)
	}
}

func TestIndexAndRetrieveIntegration(t *testing.T) {
	db, dim := openRagTestDB(t)
	cleanupIT(t, db)
	defer cleanupIT(t, db)

	incID := "it-inc-1"
	pbID := "it-pb-1"
	seedIncident(t, db, incID)
	seedPlaybook(t, db, pbID)

	svc := rag.NewService(db, rag.Options{
		Emb:              embedding.NewMock(dim),
		IndexVersion:     "v1",
		Interval:         100 * time.Millisecond,
		ReconcileEvery:   time.Hour,
		RetrievalTimeout: 2 * time.Second,
		TopKFinal:        6,
		MaxContextTokens: 5000,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go svc.RunIndexer(ctx)

	// 入队事故（结案版本 1）与剧本
	if err := rag.EnqueueJob(ctx, db, itTenant, rag.SourceIncident, incID, 1, rag.OpUpsert); err != nil {
		t.Fatal(err)
	}
	if err := rag.EnqueueJob(ctx, db, itTenant, rag.SourcePlaybook, pbID, time.Now().UnixNano(), rag.OpUpsert); err != nil {
		t.Fatal(err)
	}
	waitJobs(t, db, rag.JobSucceeded)

	// 检索：同服务事故 + 剧本应同时命中（来源多样性）
	q := rag.BuildQuery(itTenant, "irrelevant-self", "critical", "order api latency spike ECONNREFUSED",
		"order", []string{"db_pool_exhausted"}, map[string]string{"service": "order"}, time.Now())
	pack := svc.Retrieve(ctx, q)
	if len(pack.Selected) == 0 {
		t.Fatalf("no evidence: degraded=%s latency=%v", pack.DegradedReason, pack.LatencyMS)
	}
	var sawIncident, sawPlaybook bool
	for _, e := range pack.Selected {
		if e.SourceType == rag.SourceIncident && strings.Contains(e.Content, "连接池上限与并发配置不匹配") {
			sawIncident = true
		}
		if e.SourceType == rag.SourcePlaybook {
			sawPlaybook = true
		}
	}
	if !sawIncident {
		t.Fatalf("incident evidence missing: %+v", pack.Selected)
	}
	if !sawPlaybook {
		t.Fatalf("playbook evidence missing（多样性）: %+v", pack.Selected)
	}

	// 排除自身：查询主体为事故本身时不得召回自身文档
	qSelf := q
	qSelf.IncidentID = incID
	packSelf := svc.Retrieve(ctx, qSelf)
	for _, e := range packSelf.Selected {
		if e.SourceID == incID && e.SourceType == rag.SourceIncident {
			t.Fatalf("self incident leaked into results")
		}
	}

	// 跨租户隔离：其他租户查询零召回
	qOther := q
	qOther.TenantID = "other-tenant"
	packOther := svc.Retrieve(ctx, qOther)
	if len(packOther.Selected) != 0 {
		t.Fatalf("cross-tenant leak: %+v", packOther.Selected)
	}

	// unsafe 反馈 → 文档 quarantine 退出召回
	if _, err := svc.AddFeedback(ctx, itTenant, incID, &rag.RAGFeedback{
		DocumentID: pack.Selected[0].DocumentID, Rating: rag.RatingUnsafe,
	}); err != nil {
		t.Fatal(err)
	}
	var qd int64
	if err := db.Raw(`SELECT count(*) FROM knowledge_documents WHERE tenant_id = ? AND status = 'quarantined'`,
		itTenant).Scan(&qd).Error; err != nil {
		t.Fatal(err)
	}
	if qd == 0 {
		t.Fatal("unsafe feedback did not quarantine document")
	}

	// 剧本删除传播
	if err := rag.EnqueueJob(ctx, db, itTenant, rag.SourcePlaybook, pbID, 0, rag.OpDelete); err != nil {
		t.Fatal(err)
	}
	waitJobs(t, db, rag.JobSucceeded) // 总任务数 ≥2 已有满足，需等 delete 完成
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var d int64
		db.Raw(`SELECT count(*) FROM knowledge_documents WHERE tenant_id = ? AND source_type='playbook' AND status='deleted'`,
			itTenant).Scan(&d)
		if d > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	var deleted int64
	db.Raw(`SELECT count(*) FROM knowledge_documents WHERE tenant_id = ? AND source_type = 'playbook' AND status = 'deleted'`,
		itTenant).Scan(&deleted)
	if deleted == 0 {
		t.Fatal("playbook delete propagation failed")
	}
}
