// Command rag-eval 检索评测工具（KEEPON M3 / RAG_PLAN §17.1）：
// 对 golden set（rag_golden_queries）跑与线上完全同链路的检索，输出 Recall@K 等指标。
// 调参对比：两次运行分别 -json 留痕，第二次带 -baseline 输出差异（可 -fail-on-regression 接 CI）。
//
// 用法示例：
//
//	go run ./cmd/rag-eval -list                                  # 冒烟：列出评测集规模
//	go run ./cmd/rag-eval -json .bench/eval-baseline.json        # 基线（默认参数）
//	go run ./cmd/rag-eval -topk-final 10 -json .bench/eval-new.json -baseline .bench/eval-baseline.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"aiops/internal/config"
	"aiops/internal/embedding"
	"aiops/internal/modelcfg"
	"aiops/internal/rag"
	"aiops/internal/store"
)

func main() {
	fs := flag.NewFlagSet("rag-eval", flag.ExitOnError)
	dsn := fs.String("dsn", store.DefaultDSN, "PostgreSQL DSN（默认 store.DefaultDSN）")
	tenant := fs.String("tenant", "", "限定评测租户（空=全部）")
	embMode := fs.String("embedding", "auto", "embedding 装配：auto（model_configs/env 生效值）| off（keyword-only）")
	enabledOnly := fs.Bool("enabled-only", true, "只评测 enabled=true 的查询")
	topkKeyword := fs.Int("topk-keyword", 0, "关键词召回条数（0=取 AIOPS_RAGTOPKKEYWORD 默认 40）")
	topkVector := fs.Int("topk-vector", 0, "向量召回条数（0=默认 40）")
	topkFinal := fs.Int("topk-final", 0, "融合后最终证据条数（0=默认 6）")
	maxTokens := fs.Int("max-tokens", 0, "上下文 token 预算（0=默认 5000）")
	jsonOut := fs.String("json", "", "指标汇总输出文件（调参对比留痕）")
	baseline := fs.String("baseline", "", "基线汇总 JSON（输出指标差异，可选 -fail-on-regression）")
	failOnRegression := fs.Bool("fail-on-regression", false, "AnyHitRate/FirstHit 低于基线时非零退出（接 CI）")
	listOnly := fs.Bool("list", false, "只列出评测集规模与租户分布（冒烟，不跑检索）")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "rag-eval：golden set 检索评测（KEEPON M3）")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load: %v\n", err)
		os.Exit(1)
	}
	db, err := store.Open(*dsn, store.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "store open: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()

	// embedding 装配（与 server 同源：model_configs DB 优先，env 回退；off 时 keyword-only）
	opts := rag.Options{
		IndexVersion:     cfg.RAGIndexVersion,
		RetrievalTimeout: time.Duration(cfg.RAGRetrievalTimeoutMS) * time.Millisecond,
		TopKKeyword:      orDefault(*topkKeyword, cfg.RAGTopKKeyword),
		TopKVector:       orDefault(*topkVector, cfg.RAGTopKVector),
		TopKFinal:        orDefault(*topkFinal, cfg.RAGTopKFinal),
		MaxContextTokens: orDefault(*maxTokens, cfg.RAGMaxContextTokens),
		Mode:             cfg.RAGMode,
	}
	if *embMode != "off" {
		// embedding 装配（与 server 同源：仅 AIOPS_EMBEDDING* env，不走 DB）
		embEnv := modelcfg.EmbeddingEnv{
			Driver: cfg.EmbeddingDriver, BaseURL: cfg.EmbeddingBaseURL, APIKey: cfg.EmbeddingAPIKey,
			Model: cfg.EmbeddingModel, Dimension: cfg.EmbeddingDimension,
			TimeoutSec: cfg.EmbeddingTimeoutSec, BatchSize: cfg.EmbeddingBatchSize,
		}
		embCfg := modelcfg.EffectiveEmbedding(embEnv)
		switch embCfg.Driver {
		case "openai-compat":
			if embCfg.Enabled && embCfg.APIKey != "" && embCfg.Dimension > 0 {
				opts.Emb = embedding.NewOpenAICompat(embCfg.BaseURL, embCfg.APIKey, embCfg.Model,
					time.Duration(embCfg.TimeoutSec)*time.Second, embCfg.BatchSize)
				opts.BatchSize = embCfg.BatchSize
			} else {
				fmt.Fprintln(os.Stderr, "提示：embedding 生效配置不可用（key/维度），降级 keyword-only")
			}
		case "local":
			opts.Emb = embedding.NewMock(embCfg.Dimension)
		default:
			// env/DB 均未配置：从在线索引反推——mock 向量是确定性 n-gram 哈希，
			// 跨进程一致（server 与 CLI 各自生成同维度 mock 向量即可命中）
			meta := rag.LoadIndexMeta(db)
			if meta.ModelID == "mock-local" && meta.Dimension > 0 {
				opts.Emb = embedding.NewMock(meta.Dimension)
				fmt.Fprintf(os.Stderr, "提示：从在线索引反推 local mock 向量（%d 维）\n", meta.Dimension)
			} else {
				fmt.Fprintln(os.Stderr, "提示：embedding 未配置且在线索引非 mock，降级 keyword-only")
			}
		}
	} else {
		fmt.Fprintln(os.Stderr, "提示：-embedding off，keyword-only 评测")
	}

	svc := rag.NewService(db, opts)
	tenants := []string{}
	if *tenant != "" {
		tenants = []string{*tenant}
	}
	if *listOnly {
		var queries []rag.GoldenQuery
		q := db.Model(&rag.GoldenQuery{})
		if *tenant != "" {
			q = q.Where("tenant_id = ?", *tenant)
		}
		if *enabledOnly {
			q = q.Where("enabled = true")
		}
		if err := q.Find(&queries).Error; err != nil {
			fmt.Fprintf(os.Stderr, "list golden: %v\n", err)
			os.Exit(1)
		}
		byTenant := map[string]int{}
		for _, g := range queries {
			byTenant[g.TenantID]++
		}
		fmt.Printf("评测集：%d 条查询，%d 个租户：", len(queries), len(byTenant))
		parts := make([]string, 0, len(byTenant))
		for t, n := range byTenant {
			parts = append(parts, fmt.Sprintf("%s=%d", t, n))
		}
		fmt.Println(strings.Join(parts, " "))
		return
	}

	metrics, err := svc.RunEval(ctx, rag.EvalOptions{
		Tenants: tenants, EnabledOnly: *enabledOnly,
		TopKKeyword: opts.TopKKeyword, TopKVector: opts.TopKVector,
		TopKFinal: opts.TopKFinal, MaxContextTokens: opts.MaxContextTokens,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval: %v\n", err)
		os.Exit(1)
	}

	printSummary(metrics)
	if *jsonOut != "" {
		writeJSON(*jsonOut, metrics)
		fmt.Printf("\n汇总已写入 %s（调参对比：再次运行带 -baseline %s）\n", *jsonOut, *jsonOut)
	}
	if *baseline != "" {
		regressed := compareBaseline(metrics, *baseline, *failOnRegression)
		if regressed && *failOnRegression {
			fmt.Fprintln(os.Stderr, "存在指标退化（--fail-on-regression）")
			os.Exit(1)
		}
	}
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func printSummary(m *rag.EvalMetrics) {
	fmt.Printf("\n=== 检索评测汇总（topk keyword=%d vector=%d final=%d, maxTokens=%d）===\n",
		m.Params.TopKKeyword, m.Params.TopKVector, m.Params.TopKFinal, m.Params.MaxContextTokens)
	fmt.Printf("查询数: %d（租户 %d）\n", m.Total, m.Tenants)
	fmt.Printf("AnyHitRate@K(候选):   %.1f%%\n", m.AnyHitRate*100)
	fmt.Printf("MeanRecall@K:         %.1f%%\n", m.MeanRecall*100)
	fmt.Printf("Selected 命中率:      %.1f%%\n", m.SelectedAnyHitRate*100)
	fmt.Printf("首条命中率(全部):     %.1f%%\n", m.SelectedFirstHit*100)
	fmt.Printf("首条命中率(非空):     %.1f%%\n", m.FirstHitNonEmpty*100)
	fmt.Printf("空结果率:             %.1f%%\n", m.EmptyRate*100)
	fmt.Printf("平均延迟:             %.0fms  P95: %.0fms\n", m.AvgLatencyMS, m.P95LatencyMS)
	fmt.Printf("useful 反馈覆盖:      %.1f%%\n", m.UsefulCoverage*100)
	if len(m.Degraded) > 0 {
		fmt.Printf("降级分布: %v\n", m.Degraded)
	}
	for _, r := range m.PerQuery {
		status := "✗"
		if len(r.HitDocs) > 0 {
			status = "✓"
		}
		fmt.Printf("  %s %-60s expect=%d hit=%d%s latency=%dms\n",
			status, truncateRune(r.Query, 60), len(r.Expected), len(r.HitDocs),
			firstOf(r.Degraded), r.LatencyMS)
	}
}

func truncateRune(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstOf(s string) string {
	if s == "" {
		return ""
	}
	return " degraded=" + s
}

func writeJSON(path string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "写汇总失败: %v\n", err)
	}
}

// compareBaseline 与基线 diff（AnyHitRate / FirstHit / Empty 三主指标；返回是否退化）。
func compareBaseline(m *rag.EvalMetrics, path string, fail bool) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读基线失败: %v\n", err)
		return false
	}
	var base rag.EvalMetrics
	if err := json.Unmarshal(b, &base); err != nil {
		fmt.Fprintf(os.Stderr, "基线 JSON 非法: %v\n", err)
		return false
	}
	delta := func(cur, old float64) string {
		d := (cur - old) * 100
		sign := "+"
		if d < 0 {
			sign = ""
		}
		return fmt.Sprintf("%s%.1f%%", sign, d)
	}
	fmt.Printf("\n=== 与基线对比（%s）===\n", path)
	fmt.Printf("AnyHitRate: %.1f%% → %.1f%%（%s）\n", base.AnyHitRate*100, m.AnyHitRate*100,
		delta(m.AnyHitRate, base.AnyHitRate))
	fmt.Printf("首条命中:   %.1f%% → %.1f%%（%s）\n", base.SelectedFirstHit*100, m.SelectedFirstHit*100,
		delta(m.SelectedFirstHit, base.SelectedFirstHit))
	fmt.Printf("空结果率:   %.1f%% → %.1f%%（%s）\n", base.EmptyRate*100, m.EmptyRate*100,
		delta(base.EmptyRate, m.EmptyRate))
	if fail {
		return m.AnyHitRate < base.AnyHitRate || m.SelectedFirstHit < base.SelectedFirstHit
	}
	return false
}
