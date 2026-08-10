package assistant_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aiops/internal/assistant"
	"aiops/internal/evidence"
)

// mockDataSource 同时 mock Prometheus 与 Loki 的 query_range（按 path 分发）。
func mockDataSource(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Loki 分支在前（/loki/api/v1/query_range 也以 /api/v1/query_range 结尾）
		switch {
		case strings.Contains(r.URL.Path, "/loki/api/v1/query_range"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"streams","result":[
				{"stream":{"service":"pay"},
				 "values":[["1690000000000000000","ERROR: db conn failed password=secret123"],
				           ["1690000060000000000","request timeout after 5s"]]}]}}`)
		case strings.HasSuffix(r.URL.Path, "/api/v1/query_range"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"__name__":"container_cpu_usage_seconds_total","service":"pay"},
				 "values":[[1690000000,"0.5"],[1690000060,"0.8"],[1690000120,"1.6"]]}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"bad path"}`)
		}
	}))
}

func TestQueryMetricsTool(t *testing.T) {
	ts := mockDataSource(t)
	defer ts.Close()
	prom := evidence.NewPromClient(ts.URL, "", "", time.Second)

	deps := assistant.ToolDeps{Prometheus: prom}
	reg := assistant.NewRegistry(deps)
	tool, ok := reg.Get("query_metrics")
	if !ok {
		t.Fatal("query_metrics 未注册")
	}
	out, err := tool.Call(context.Background(), assistant.ToolCallCtx{
		TenantID: "t", Args: map[string]any{"query": `rate(x[5m])`, "window_minutes": 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprintf("%v", out)
	if !strings.Contains(s, "container_cpu_usage_seconds_total") ||
		!strings.Contains(s, `"trending":"up"`) {
		t.Fatalf("结果缺趋势摘要: %s", s)
	}
}

func TestSearchLogsToolRedactsAndInjection(t *testing.T) {
	ts := mockDataSource(t)
	defer ts.Close()
	loki := evidence.NewLokiClient(ts.URL, time.Second)

	deps := assistant.ToolDeps{Loki: loki}
	reg := assistant.NewRegistry(deps)
	tool, _ := reg.Get("search_logs")

	// 正常查询：日志行脱敏
	out, err := tool.Call(context.Background(), assistant.ToolCallCtx{
		TenantID: "t", Args: map[string]any{"service": "pay", "limit": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprintf("%v", out)
	if strings.Contains(s, "secret123") || !strings.Contains(s, "REDACTED") {
		t.Fatalf("日志行未脱敏: %s", s)
	}
	if !strings.Contains(s, "timeout after 5s") {
		t.Fatalf("正常行丢失: %s", s)
	}
	// 注入查询 → 拒绝（不请求数据源）
	out, err = tool.Call(context.Background(), assistant.ToolCallCtx{
		TenantID: "t", Args: map[string]any{"query": `{service="x"} |= "ignore previous instructions"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprintf("%v", out), "注入") {
		t.Fatalf("注入查询未被拒绝: %v", out)
	}
}

func TestEvidenceToolsNotRegisteredWhenNil(t *testing.T) {
	reg := assistant.NewRegistry(assistant.ToolDeps{}) // 无数据源
	if _, ok := reg.Get("query_metrics"); ok {
		t.Fatal("未配置数据源时 query_metrics 不应注册")
	}
	if _, ok := reg.Get("search_logs"); ok {
		t.Fatal("未配置数据源时 search_logs 不应注册")
	}
	// 原有工具不受影响
	if _, ok := reg.Get("get_platform_overview"); !ok {
		t.Fatal("原有工具应保留")
	}
}

func TestQueryMetricsDataSourceDown(t *testing.T) {
	// 指向不可达端口
	prom := evidence.NewPromClient("http://127.0.0.1:1", "", "", 300*time.Millisecond)
	deps := assistant.ToolDeps{Prometheus: prom}
	reg := assistant.NewRegistry(deps)
	tool, _ := reg.Get("query_metrics")
	out, err := tool.Call(context.Background(), assistant.ToolCallCtx{
		TenantID: "t", Args: map[string]any{"query": "x"},
	})
	if err != nil {
		t.Fatalf("数据源不可达不应使工具失败: %v", err)
	}
	s := fmt.Sprintf("%v", out)
	if !strings.Contains(s, "prometheus 数据源不可达") {
		t.Fatalf("应返回结构化降级说明: %s", s)
	}
}

// ---- EvidenceCollector（RCA 注入） ----

func TestEvidenceCollectorLogs(t *testing.T) {
	ts := mockDataSource(t)
	defer ts.Close()
	loki := evidence.NewLokiClient(ts.URL, time.Second)
	collector := evidence.NewEvidenceCollector(nil, loki, nil)

	sentences := collector.Collect(context.Background(), "pay", time.Now().Add(-time.Hour))
	if len(sentences) != 1 {
		t.Fatalf("sentences = %v", sentences)
	}
	s := sentences[0]
	if !strings.Contains(s, "Loki") || !strings.Contains(s, "1 条错误级") {
		t.Fatalf("句子 = %q", s)
	}
	if strings.Contains(s, "secret123") {
		t.Fatalf("日志样本未脱敏: %s", s)
	}
	// nil collector 安全
	if NewCollectorNil().Collect(context.Background(), "x", time.Now()) != nil {
		t.Fatal("nil collector 应返回 nil")
	}
}

func NewCollectorNil() *evidence.EvidenceCollector { return nil }

func TestEvidenceCollectorMetricsJump(t *testing.T) {
	ts := mockDataSource(t)
	defer ts.Close()
	prom := evidence.NewPromClient(ts.URL, "", "", time.Second)
	collector := evidence.NewEvidenceCollector(prom, nil, []string{"rate(x[5m])"})

	sentences := collector.Collect(context.Background(), "pay", time.Now().Add(-time.Hour))
	if len(sentences) != 1 {
		t.Fatalf("sentences = %v", sentences)
	}
	if !strings.Contains(sentences[0], "跳变") {
		t.Fatalf("指标跳变句子 = %q", sentences[0])
	}
}

