package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiops/internal/alert"
	"aiops/internal/assistant"
	"aiops/internal/config"
	"aiops/internal/eventbus"
	"aiops/internal/incident"
	"aiops/internal/overview"
	"aiops/internal/store"
	"aiops/internal/topology"
	"aiops/internal/util"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 测试租户（固定值：cleanup 与请求中间件必须一致，否则残留数据破坏幂等断言）。
const testTenant = "asst-api-test"

// newAssistantServer 组装带 assistant 服务的测试 Server（DB-gated；PG 未运行跳过）。
func newAssistantServer(t *testing.T, cfg *config.Config) (*Server, *assistant.Runner, func()) {
	t.Helper()
	db := openAPIDB(t)
	tenant := testTenant
	cleanupAPI(t, db, tenant)

	repo := alert.NewRepository(db)
	svc := incident.NewService(db)
	topoSvc := topology.NewService(db)
	ovSvc := overview.New(db, nil, nil, nil, nil, time.Now())
	audit := eventbus.NewAuditService(db)

	deps := assistant.ToolDeps{Overview: ovSvc, Alerts: repo, Incidents: svc, Topology: topoSvc}
	reg := assistant.NewRegistry(deps)
	broker := assistant.NewBroker(db)
	limits := assistant.Limits{MaxConcurrency: 4, MaxModelRounds: 8, MaxToolCalls: 12,
		RunTimeout: 5 * time.Second, ToolTimeout: 3 * time.Second}
	runner := assistant.NewRunner(db, nil, "mock", reg, broker, audit, limits, nil) // nil model：run 快速失败
	asstSvc := assistant.NewService(db, "mock", broker, audit, runner, svc)

	s := &Server{cfg: cfg, assist: asstSvc, assistBr: broker, audit: audit}
	return s, runner, func() { cleanupAPI(t, db, tenant) }
}

// openAPIDB PostgreSQL 可用则连库，否则跳过。
func openAPIDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(store.DefaultDSN, store.Options{})
	if err != nil {
		t.Skipf("postgres 未运行（docker compose），跳过: %v", err)
	}
	return db
}

func cleanupAPI(t *testing.T, db *gorm.DB, tenant string) {
	t.Helper()
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM assistant_events WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM assistant_steps WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM assistant_messages WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM assistant_runs WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM assistant_threads WHERE tenant_id = ?", tenant).Error
		_ = db.Exec("DELETE FROM audit_logs WHERE tenant_id = ?", tenant).Error
	})
}

func doJSON(t *testing.T, s *Server, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 模拟 authMiddleware：固定测试租户
	r.Use(func(c *gin.Context) { c.Set(ctxTenant, testTenant); c.Next() })
	registerAssistantRoutes(r, s)
	var buf bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = *bytes.NewBuffer(b)
	}
	req := httptest.NewRequest(method, path, &buf)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// registerAssistantRoutes 只注册 assistant 路由（测试隔离）。
func registerAssistantRoutes(r *gin.Engine, s *Server) {
	api := r.Group("/api/v1")
	api.POST("/assistant/threads", s.handleCreateThread)
	api.GET("/assistant/threads", s.handleListThreads)
	api.GET("/assistant/threads/:id", s.handleGetThread)
	api.DELETE("/assistant/threads/:id", s.handleDeleteThread)
	api.POST("/assistant/threads/:id/messages", s.handleCreateMessage)
	api.GET("/assistant/runs/:id", s.handleGetRun)
	api.GET("/assistant/runs/:id/events", s.handleRunEvents)
	api.POST("/assistant/runs/:id/cancel", s.handleCancelRun)
}

// ---- 关闭态 ----

func TestAssistantDisabled404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(ctxTenant, testTenant); c.Next() })
	s := &Server{} // assist == nil
	registerAssistantRoutes(r, s)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v1/assistant/threads"},
		{"GET", "/api/v1/assistant/threads"},
		{"POST", "/api/v1/assistant/threads/x/messages"},
		{"GET", "/api/v1/assistant/runs/x/events"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, w.Code)
		}
	}
}

// ---- 会话与消息 ----

func TestAssistantThreadAndMessageFlow(t *testing.T) {
	cfg := &config.Config{}
	s, _, cleanup := newAssistantServer(t, cfg)
	defer cleanup()

	// 建会话
	w := doJSON(t, s, http.MethodPost, "/api/v1/assistant/threads", map[string]any{"title": "测试会话"}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create thread = %d: %s", w.Code, w.Body.String())
	}
	var th map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &th)
	threadID := th["id"].(string)

	// 发送消息（无 Idempotency-Key → 400）
	w = doJSON(t, s, http.MethodPost, "/api/v1/assistant/threads/"+threadID+"/messages",
		map[string]any{"content": "你好"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no idem key = %d, want 400", w.Code)
	}
	// 带 key → 202 + runId
	hdr := map[string]string{"Idempotency-Key": "idem-1"}
	w = doJSON(t, s, http.MethodPost, "/api/v1/assistant/threads/"+threadID+"/messages",
		map[string]any{"content": "你好"}, hdr)
	if w.Code != http.StatusAccepted {
		t.Fatalf("send = %d: %s", w.Code, w.Body.String())
	}
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	runID := res["runId"].(string)
	if runID == "" {
		t.Fatal("runId 缺失")
	}
	// 同 key 重复 → 同 runId（幂等）
	w = doJSON(t, s, http.MethodPost, "/api/v1/assistant/threads/"+threadID+"/messages",
		map[string]any{"content": "你好"}, hdr)
	var res2 map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res2)
	if res2["runId"] != runID {
		t.Fatalf("幂等失效：%v != %v", res2["runId"], runID)
	}
	// 列表
	w = doJSON(t, s, http.MethodGet, "/api/v1/assistant/threads", nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), threadID) {
		t.Fatalf("list = %d: %s", w.Code, w.Body.String())
	}
	// run 查询
	w = doJSON(t, s, http.MethodGet, "/api/v1/assistant/runs/"+runID, nil, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"queued"`) {
		t.Fatalf("get run = %d: %s", w.Code, w.Body.String())
	}
	// 取消
	w = doJSON(t, s, http.MethodPost, "/api/v1/assistant/runs/"+runID+"/cancel", nil, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d: %s", w.Code, w.Body.String())
	}
	w = doJSON(t, s, http.MethodGet, "/api/v1/assistant/runs/"+runID, nil, nil)
	if !strings.Contains(w.Body.String(), `"cancelled"`) {
		t.Fatalf("cancel 后 run = %s", w.Body.String())
	}
	// 软删会话
	w = doJSON(t, s, http.MethodDelete, "/api/v1/assistant/threads/"+threadID, nil, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", w.Code)
	}
	w = doJSON(t, s, http.MethodGet, "/api/v1/assistant/threads/"+threadID, nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("软删后 = %d, want 404", w.Code)
	}
	// 不存在的会话发消息 → 404
	w = doJSON(t, s, http.MethodPost, "/api/v1/assistant/threads/nope/messages",
		map[string]any{"content": "x"}, map[string]string{"Idempotency-Key": "k2"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在会话 = %d, want 404", w.Code)
	}
}

// ---- SSE ----

func TestAssistantRunEventsSSE(t *testing.T) {
	cfg := &config.Config{}
	s, runner, cleanup := newAssistantServer(t, cfg)
	defer cleanup()

	// 直接造 run + 事件
	db := openAPIDB(t)
	th := &assistant.AssistantThread{ID: util.NewID(), TenantID: testTenant, Title: "t",
		Status: assistant.ThreadActive, CreatedAt: time.Now().UTC()}
	_ = db.Create(th).Error
	run := &assistant.AssistantRun{ID: util.NewID(), TenantID: testTenant, ThreadID: th.ID,
		Model: "mock", PromptVersion: "v1", IdempotencyKey: util.NewID()}
	runID, _ := assistant.EnqueueRun(context.Background(), db, run)
	_ = runner // run 不执行（nil model 快速失败即可）；直接发布事件
	_, _ = s.assistBr.Publish(context.Background(), runID, testTenant, assistant.EvRunStarted, map[string]any{"status": "running"})
	_, _ = s.assistBr.Publish(context.Background(), runID, testTenant, assistant.EvMessageDelta, map[string]any{"content": "你好"})

	// SSE 请求
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/assistant/runs/"+runID+"/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	// 直接调 handler（gin 需要 writer——用 gin 路由包裹，读 body 需要 flusher）
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(ctxTenant, testTenant); c.Next() })
	registerAssistantRoutes(r, s)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("sse = %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	// 解析事件行
	body := w.Body.String()
	if !strings.Contains(body, `"run.started"`) || !strings.Contains(body, `"message.delta"`) {
		t.Fatalf("事件缺失: %s", body)
	}
	_ = bufio.NewScanner(strings.NewReader(body))
}
