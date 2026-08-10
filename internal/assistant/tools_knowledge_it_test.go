package assistant_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"aiops/internal/alert"
	"aiops/internal/assistant"
	"aiops/internal/embedding"
	"aiops/internal/incident"
	"aiops/internal/playbook"
	"aiops/internal/rag"

	"gorm.io/gorm"
)

// tools_knowledge_it_test.go：知识工具 DB 门控集成测试。
// 走真实 SQL（索引建成 → search_knowledge / get_similar_incidents / list_playbooks 调用
// → citation 收集）+ thread 绑定上下文 + 消息反馈。PG 未运行时跳过。

const knowItTenant = "asst-know-it"

func openKnowDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openDB(t)
	models := []any{
		&assistant.AssistantThread{}, &assistant.AssistantMessage{}, &assistant.AssistantRun{},
		&assistant.AssistantFeedback{},
		&rag.KnowledgeDocument{}, &rag.KnowledgeChunk{}, &rag.KnowledgeEmbedding{},
		&rag.KnowledgeJob{}, &rag.RAGTrace{}, &rag.RAGFeedback{},
		&incident.Incident{}, &incident.Resolution{}, &incident.AlertToIncident{},
		&alert.Alert{}, &playbook.Playbook{},
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dim := rag.LoadIndexMeta(db).Dimension
	if dim == 0 {
		dim = 64
	}
	if err := rag.EnsureSchema(db, rag.SchemaOptions{Dimension: dim}); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	cleanupKnow(t, db)
	return db
}

func cleanupKnow(t *testing.T, db *gorm.DB) {
	t.Helper()
	t.Cleanup(func() {
		for _, tbl := range []string{"knowledge_embeddings", "knowledge_chunks", "knowledge_documents",
			"knowledge_jobs", "rag_traces", "rag_feedback", "alert_to_incidents", "incident_resolutions",
			"incidents", "alerts", "playbooks", "assistant_feedback", "assistant_messages",
			"assistant_threads", "assistant_runs"} {
			_ = db.Exec("DELETE FROM "+tbl+" WHERE tenant_id = ?", knowItTenant).Error
		}
	})
}

// seedResolvedIncident 已解决事故（含结案数据与告警）——RAG 索引源。
func seedResolvedIncident(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := db.Exec(sql, args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO incidents (id, tenant_id, name, summary, severity, status, source, created_at)
		VALUES (?, ?, 'order 服务连接池耗尽', 'order api latency spike db_pool_exhausted ECONNREFUSED',
			'critical', 'resolved', 'agent', now())`, id, knowItTenant)
	exec(`INSERT INTO incident_resolutions (id, tenant_id, incident_id, confirmed_root_cause, resolution,
		outcome, verification, verification_status, resolved_by, resolved_at, updated_at, version)
		VALUES (?, ?, ?, '连接池上限与并发配置不匹配', '回滚配置并重启连接池', 'success',
			'错误率恢复', 'verified', 'oncall', now(), now(), 1)`, "res-"+id, knowItTenant, id)
	exec(`INSERT INTO alerts (id, tenant_id, provider, provider_type, fingerprint, alert_hash, event,
		name, severity, status, labels, started_at, last_seen_at, created_at, updated_at)
		VALUES (?, ?, 'prometheus', 'prometheus', ?, 'h-1', '{}'::jsonb,
			'db_pool_exhausted', 'critical', 'resolved', ?::jsonb, now(), now(), now(), now())`,
		"alert-"+id, knowItTenant, "fp-"+id, `{"service":"order","error_code":"ECONNREFUSED"}`)
	exec(`INSERT INTO alert_to_incidents (alert_id, incident_id, tenant_id) VALUES (?, ?, ?)`,
		"alert-"+id, id, knowItTenant)
}

// seedOpenIncident 当前事故（get_similar_incidents 的查询源，应被排除出结果）。
func seedOpenIncident(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO incidents (id, tenant_id, name, summary, severity, status, source, created_at)
		VALUES (?, ?, 'order 接口超时', 'order api slow ECONNREFUSED', 'high', 'open', 'webhook', now())`,
		id, knowItTenant).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO alerts (id, tenant_id, provider, provider_type, fingerprint, alert_hash, event,
		name, severity, status, labels, started_at, last_seen_at, created_at, updated_at)
		VALUES (?, ?, 'webhook', 'generic', ?, 'h-2', '{}'::jsonb,
			'order-api-slow', 'high', 'firing', ?::jsonb, now(), now(), now(), now())`,
		"alert-cur", knowItTenant, "fp-cur", `{"service":"order"}`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO alert_to_incidents (alert_id, incident_id, tenant_id) VALUES (?, ?, ?)`,
		"alert-cur", id, knowItTenant).Error; err != nil {
		t.Fatal(err)
	}
}

// waitDocs 轮询直到租户下 active 文档数达到 n。
func waitDocs(t *testing.T, db *gorm.DB, n int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var got int64
		if err := db.Raw(`SELECT count(*) FROM knowledge_documents WHERE tenant_id=? AND status='active'`,
			knowItTenant).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("documents did not reach %d", n)
}

// newRagSvc RAG 服务（mock embedding + 索引 worker；cancel 时停）。
func newRagSvc(t *testing.T, db *gorm.DB, dim int) (*rag.Service, context.CancelFunc) {
	t.Helper()
	svc := rag.NewService(db, rag.Options{
		Emb:              embedding.NewMock(dim),
		IndexVersion:     "v1",
		Interval:         100 * time.Millisecond,
		ReconcileEvery:   time.Hour,
		RetrievalTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go svc.RunIndexer(ctx)
	t.Cleanup(cancel)
	return svc, cancel
}

func TestSearchKnowledgeToolIndexedDoc(t *testing.T) {
	db := openKnowDB(t)
	ctx := context.Background()

	// 已解决事故 → 索引 → 检索（关键词路，无向量依赖）
	seedResolvedIncident(t, db, "know-inc-1")
	ragSvc, _ := newRagSvc(t, db, rag.LoadIndexMeta(db).Dimension)
	if err := rag.EnqueueJob(ctx, db, knowItTenant, rag.SourceIncident, "know-inc-1", 1, rag.OpUpsert); err != nil {
		t.Fatal(err)
	}
	waitDocs(t, db, 1)

	reg := assistant.NewRegistry(assistant.ToolDeps{
		RAG: ragSvc, Playbooks: playbook.NewService(db),
	})
	tool, ok := reg.Get("search_knowledge")
	if !ok {
		t.Fatal("search_knowledge 未注册")
	}
	var citations []assistant.Citation
	out, err := tool.Call(ctx, assistant.ToolCallCtx{
		TenantID: knowItTenant, Args: map[string]any{"query": "order 连接池耗尽 ECONNREFUSED", "limit": 5},
		Citations: &citations,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := out.(string)
	if !strings.Contains(s, `"count":1`) {
		t.Fatalf("应命中 1 条已索引事故: %s", s)
	}
	if len(citations) == 0 || citations[0].SourceType != rag.SourceIncident {
		t.Fatalf("citation 应为 incident 来源: %+v", citations)
	}
	if citations[0].URL != "/incidents/know-inc-1" {
		t.Fatalf("citation URL 异常: %+v", citations[0])
	}
}

func TestSimilarIncidentsToolExcludesSelf(t *testing.T) {
	db := openKnowDB(t)
	ctx := context.Background()

	seedResolvedIncident(t, db, "know-inc-1") // 历史已解决（应命中）
	seedOpenIncident(t, db, "know-inc-2")     // 当前事故（应排除自身）
	ragSvc, _ := newRagSvc(t, db, rag.LoadIndexMeta(db).Dimension)
	if err := rag.EnqueueJob(ctx, db, knowItTenant, rag.SourceIncident, "know-inc-1", 1, rag.OpUpsert); err != nil {
		t.Fatal(err)
	}
	waitDocs(t, db, 1)

	reg := assistant.NewRegistry(assistant.ToolDeps{RAG: ragSvc, Incidents: incident.NewService(db)})
	tool, ok := reg.Get("get_similar_incidents")
	if !ok {
		t.Fatal("get_similar_incidents 未注册")
	}
	var citations []assistant.Citation
	out, err := tool.Call(ctx, assistant.ToolCallCtx{
		TenantID: knowItTenant, Args: map[string]any{"incident_id": "know-inc-2"},
		Citations: &citations,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := out.(string)
	if strings.Contains(s, `"sourceId":"know-inc-2"`) {
		t.Fatalf("不应检索到自身: %s", s)
	}
	if !strings.Contains(s, `"count":1`) {
		t.Fatalf("应命中历史已解决事故: %s", s)
	}
}

func TestListPlaybooksToolEnabledOnly(t *testing.T) {
	db := openKnowDB(t)
	ctx := context.Background()

	for _, pb := range []struct{ name, enabled string }{
		{"rollback", "true"},
		{"scale_up", "false"},
	} {
		if err := db.Exec(`INSERT INTO playbooks (id, tenant_id, name, level, command_template,
			blast_radius_factor, rollback_command, dry_run_first, enabled, created_at, updated_at)
			VALUES (?, ?, ?, 'L1', 'kubectl rollout undo deployment/{{.service}}', 0.35,
				'kubectl rollout status', true, ?, now(), now())`,
			"pb-"+pb.name, knowItTenant, pb.name, pb.enabled).Error; err != nil {
			t.Fatal(err)
		}
	}

	reg := assistant.NewRegistry(assistant.ToolDeps{Playbooks: playbook.NewService(db)})
	tool, ok := reg.Get("list_playbooks")
	if !ok {
		t.Fatal("list_playbooks 未注册")
	}
	out, err := tool.Call(ctx, assistant.ToolCallCtx{TenantID: knowItTenant, Args: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	s := out.(string)
	if !strings.Contains(s, `"count":1`) || !strings.Contains(s, "rollback") {
		t.Fatalf("应只列出 enabled 剧本: %s", s)
	}
	if strings.Contains(s, "scale_up") {
		t.Fatalf("disabled 剧本不应出现: %s", s)
	}
}

func TestThreadContextBindingAndFeedback(t *testing.T) {
	db := openKnowDB(t)
	ctx := context.Background()
	seedResolvedIncident(t, db, "know-inc-1")

	incSvc := incident.NewService(db)
	svc := assistant.NewService(db, "mock", nil, nil, nil, incSvc)

	// 绑定 incident 上下文 → 摘要注入
	th, err := svc.CreateThread(ctx, knowItTenant, assistant.CreateThreadInput{
		ContextType: "incident", ContextID: "know-inc-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if th.ContextType != "incident" || th.ContextID != "know-inc-1" {
		t.Fatalf("context 未落库: %+v", th)
	}
	if !strings.Contains(th.Title, "连接池耗尽") {
		t.Fatalf("标题应默认绑定事故名: %q", th.Title)
	}
	builder := assistant.NewContextBuilder(incSvc)
	summary, err := builder.Build(ctx, knowItTenant, th)
	if err != nil || summary == "" {
		t.Fatalf("context 摘要失败: %q %v", summary, err)
	}
	if !strings.Contains(summary, "order 服务连接池耗尽") || !strings.Contains(summary, "db_pool_exhausted") {
		t.Fatalf("摘要应含确定性事实: %s", summary)
	}

	// 非法绑定：不存在的事故 / 未知类型
	if _, err := svc.CreateThread(ctx, knowItTenant, assistant.CreateThreadInput{
		ContextType: "incident", ContextID: "no-such-inc",
	}); err == nil {
		t.Fatal("绑定不存在事故应报错")
	}
	if _, err := svc.CreateThread(ctx, knowItTenant, assistant.CreateThreadInput{
		ContextType: "alert", ContextID: "x",
	}); err == nil {
		t.Fatal("未知 contextType 应报错")
	}

	// 反馈：仅最终 assistant 消息可评
	msg := &assistant.AssistantMessage{
		ID: "asst-msg-1", TenantID: knowItTenant, ThreadID: th.ID,
		Role: "assistant", Content: "回答内容", ContentFormat: "text",
		Sequence: 1, Status: "complete", CreatedAt: time.Now().UTC(),
	}
	if err := db.Create(msg).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AddFeedback(ctx, knowItTenant, "asst-msg-1", "useful", "很准确"); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddFeedback(ctx, knowItTenant, "asst-msg-1", "bad-rating", ""); err == nil {
		t.Fatal("非法 rating 应报错")
	}
	userMsg := &assistant.AssistantMessage{
		ID: "asst-msg-2", TenantID: knowItTenant, ThreadID: th.ID,
		Role: "user", Content: "问题", ContentFormat: "text",
		Sequence: 0, Status: "complete", CreatedAt: time.Now().UTC(),
	}
	if err := db.Create(userMsg).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AddFeedback(ctx, knowItTenant, "asst-msg-2", "useful", ""); err == nil {
		t.Fatal("user 消息不可反馈")
	}
	var n int64
	if err := db.Raw(`SELECT count(*) FROM assistant_feedback WHERE tenant_id=?`, knowItTenant).Scan(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("feedback 落库数 = %d, want 1", n)
	}
}

