// Command aiops 是 AIOps 告警平台单体服务入口。
// 装配顺序：config → store(PostgreSQL) → 领域服务 → 事件管线 → 编排器 → API → worker。
// 双轨架构：确定性热路径（pipeline）与 Agent 智能路径（orchestrator）共享同一进程。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"aiops/internal/agent"
	"aiops/internal/ai"
	"aiops/internal/alert"
	"aiops/internal/anomaly"
	"aiops/internal/api"
	"aiops/internal/assistant"
	"aiops/internal/auth"
	"aiops/internal/backup"
	"aiops/internal/cel"
	"aiops/internal/circuit"
	"aiops/internal/config"
	"aiops/internal/embedding"
	"aiops/internal/external"
	"aiops/internal/eventbus"
	"aiops/internal/evidence"
	"aiops/internal/incident"
	"aiops/internal/infra"
	"aiops/internal/llm"
	"aiops/internal/logutil"
	"aiops/internal/maintenance"
	"aiops/internal/metrics"
	"aiops/internal/modelcfg"
	"aiops/internal/notifier"
	"aiops/internal/notifythrottle"
	"aiops/internal/orchestrator"
	"aiops/internal/overview"
	"aiops/internal/pipeline"
	"aiops/internal/playbook"
	"aiops/internal/provider/builtin" // 触发 init() 注册内置 provider
	"aiops/internal/pusher"
	"aiops/internal/queue"
	"aiops/internal/rag"
	"aiops/internal/retention"
	"aiops/internal/store"
	"aiops/internal/topology"
	"aiops/internal/workflow"

	"github.com/gin-gonic/gin"
	"golang.org/x/sys/windows"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}
	startTime := time.Now() // 总览 uptime 基准（KEEPON 5.1）
	// Windows 控制台代码页默认 GBK(936)，程序输出 UTF-8 中文会乱码 → 切到 UTF-8（65001）
	// （仅影响本进程写控制台的显示；日志文件本就是 UTF-8，不受影响）
	if err := windows.SetConsoleOutputCP(65001); err != nil {
		slog.Debug("console: set output cp", "err", err)
	}
	setupLogger(cfg)
	slog.Info("aiops starting", "addr", cfg.Addr, "db", cfg.DBDriver, "auth", cfg.AuthType,
		"agent", cfg.AgentEnabled, "dryRun", cfg.AgentDryRun)

	// 1. 存储（PostgreSQL，docker-compose 提供；连接池调优 + 启动重试，Phase 3 加固）
	db, err := store.Open(cfg.DBDSN, store.Options{
		MaxOpenConns:    cfg.DBMaxOpenConns,
		MaxIdleConns:    cfg.DBMaxIdleConns,
		ConnMaxLifetime: time.Duration(cfg.DBConnMaxLifetimeSec) * time.Second,
		ConnMaxIdleTime: time.Duration(cfg.DBConnMaxIdleTimeSec) * time.Second,
		ConnectRetries:  cfg.DBConnectRetries,
		AlertPartitions: cfg.AlertPartitions,
	})
	if err != nil {
		slog.Error("store open", "err", err)
		os.Exit(1)
	}

	// 2. 领域组件
	repo := alert.NewRepository(db)
	celEngine, err := cel.New()
	if err != nil {
		slog.Error("cel init", "err", err)
		os.Exit(1)
	}
	svc := incident.NewService(db)
	rules := incident.NewRuleEngine(db, celEngine, svc, 5*time.Second)
	topoSvc := topology.NewService(db)
	// Phase 3：归一化依赖边回填（存量 JSONB 依赖投影到 topology_edges，影响面查询的事实来源）
	if err := topoSvc.SyncAllEdges(context.Background(), "default"); err != nil {
		slog.Warn("topology: sync edges", "err", err)
	}
	authSvc := auth.NewService(db)
	// 2.5 简单访问保护（KEEPON 4.3）：apikey 模式首次启动生成 owner key。
	// 明文直接打 stdout（不经 slog，防落日志文件）；已存在任何 key 时不重复生成。
	if cfg.AuthType == "apikey" {
		if owner, err := authSvc.EnsureOwnerKey(context.Background()); err != nil {
			slog.Warn("auth: ensure owner key", "err", err)
		} else if owner != "" {
			fmt.Fprintf(os.Stdout, "\n=== AIOPS owner key（仅显示一次，请立即保存）===\n%s\n============================================\n", owner)
		}
	}
	pbSvc := playbook.NewService(db)
	mwSvc := maintenance.NewService(db)
	aiSvc := ai.NewService(db, svc, cfg.AIClusterThreshold, time.Duration(cfg.AIClusterWindowMin)*time.Minute)
	auditSvc := eventbus.NewAuditService(db)
	breaker := circuit.NewBreaker(5, 60*time.Second) // 自愈执行熔断：5 次失败 → 开启 60s
	ntf := notifier.New(cfg.NotifierChannels, cfg.NotifierDingTalk, cfg.NotifierFeishu,
		cfg.NotifierWecom, cfg.NotifierHTTP, cfg.NotifierBaseURL)
	// 通知渠道配置 DB 化：env 仅作首启种子，之后 DB 为准（UI 可管理 + 热重载）
	ntfCfg := notifier.NewConfigService(db, ntf)
	envWebhooks := map[string]string{
		notifier.ChannelDingTalk: cfg.NotifierDingTalk,
		notifier.ChannelFeishu:   cfg.NotifierFeishu,
		notifier.ChannelWecom:    cfg.NotifierWecom,
		notifier.ChannelHTTP:     cfg.NotifierHTTP,
	}
	if err := ntfCfg.EnsureDefaults(context.Background(), cfg.NotifierChannels, envWebhooks); err != nil {
		slog.Warn("notifier: seed channel defaults", "err", err)
	}
	if err := ntfCfg.Reload(context.Background()); err != nil {
		slog.Warn("notifier: reload channels", "err", err)
	}
	if ntf.Len() > 0 {
		slog.Info("notifier enabled", "channels", ntf.Enabled())
	}

	// 事故生命周期通知：规则/AI 聚类/Agent 新建或解决事故也推送渠道（fail-open，未配置渠道跳过）
	svc.SetNotifyFunc(notifier.NewLifecycleNotifier(ntf, auditSvc).Notify)

	// 2.6 大模型 API 接入管理（DB 化，仿通知渠道：Chat LLM 与 Assistant Chat 入库，env 仅首启种子，之后 DB 为准）。
	// Embedding 不走 DB——由 AIOPS_EMBEDDING* env 直接配置（见下方 4.7 装配）。
	// 客户端在启动装配时构建 → 保存配置后需重启生效（不做热重载，避免装配重构风险）。
	mcCfg := modelcfg.NewConfigService(db)
	if err := mcCfg.EnsureDefaults(context.Background(),
		modelcfg.ChatEnv{Driver: cfg.LLMDriver, BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey,
			Model: cfg.LLMModel, TimeoutSec: cfg.LLMTimeoutSec, Stream: cfg.LLMStream},
		modelcfg.ChatEnv{Driver: cfg.AssistantChatDriver, BaseURL: cfg.AssistantChatBaseURL,
			APIKey: cfg.AssistantChatAPIKey, Model: cfg.AssistantChatModel,
			TimeoutSec: cfg.AssistantChatTimeoutSec, Stream: cfg.AssistantChatStream}); err != nil {
		slog.Warn("modelcfg: seed defaults", "err", err)
	}

	// 3. 基础设施
	bus := eventbus.NewMemoryBus()
	defer bus.Close()
	hub := pusher.NewHub()
	tickets := auth.NewTicketStore(30 * time.Second) // WS 握手短期 ticket（apikey 模式）
	q := queue.NewMemoryQueue(4)
	defer q.Close()
	// 告警通知节流器（KEEPON 5.4：内存态，首条立即/冷却合并/升级与恢复突破/周期汇总）
	th := notifythrottle.New(time.Duration(cfg.NotifyCooldownSec)*time.Second,
		time.Duration(cfg.NotifySummaryMin)*time.Minute)

	// 3.5 指标出口（Phase 3 压测观测）：管线耗时/去重构成/事故来源/队列深度 → GET /metrics
	reg := metrics.New()
	reg.Counter("aiops_alerts_total", "入库告警数（按去重分类）", "kind")
	reg.Counter("aiops_alerts_duplicated_total", "完全重复被丢弃的告警数")
	reg.Counter("aiops_alerts_suppressed_total", "维护窗口静默的告警数")
	reg.Counter("aiops_incidents_total", "新建事故数（按来源）", "source")
	reg.Histogram("aiops_pipeline_duration_seconds", "单条告警管线处理耗时（秒）", metrics.SecondsBuckets)
	reg.Gauge("aiops_queue_depth", "任务队列深度（agent.run + workflow.trigger）",
		func() float64 { return float64(q.Len()) })
	svc.SetMetrics(reg)

	// 4. 事件管线（确定性热路径）
	pipe := pipeline.New(repo, rules, svc, bus, hub, q)
	pipe.SetMetrics(reg)
	pipe.SetMaintenanceFilter(pipeline.NewWindowFilter(mwSvc, 5*time.Second))
	if !cfg.AgentEnabled {
		pipe.AgentEnabled = false
		slog.Warn("agent pipeline disabled by config (AIOPS_AGENTENABLED=false)")
	}

	// 4.5 LLM 增强（RCA 根因补充 + AI 聚类摘要）：生效配置来自 model_configs 表（DB 优先），
	// 无 DB 行回退 env；apiKey 非空才启用（openai-compat，默认 DeepSeek）；否则纯确定性
	chatCfg, err := mcCfg.ResolveChat(context.Background(), modelcfg.ChatEnv{
		Driver: cfg.LLMDriver, BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey,
		Model: cfg.LLMModel, TimeoutSec: cfg.LLMTimeoutSec, Stream: cfg.LLMStream,
	})
	if err != nil {
		slog.Warn("modelcfg: resolve chat, fallback env", "err", err)
		chatCfg = modelcfg.EffectiveChat(nil, false, modelcfg.ChatEnv{
			Driver: cfg.LLMDriver, BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey,
			Model: cfg.LLMModel, TimeoutSec: cfg.LLMTimeoutSec, Stream: cfg.LLMStream,
		})
	}
	var llmClient *ai.Client
	if chatCfg.Enabled && chatCfg.APIKey != "" {
		llmClient = ai.NewClient(chatCfg.BaseURL, chatCfg.APIKey, chatCfg.Model)
		llmClient.SetTimeout(time.Duration(chatCfg.TimeoutSec) * time.Second)
		llmClient.SetStream(chatCfg.Stream)
		aiSvc.SetClusterLLM(llmClient) // 聚类摘要也接 LLM（非确定性模板，失败 fallback 确定性）
		slog.Info("llm enhancer enabled", "source", chatCfg.Source, "model", chatCfg.Model,
			"stream", chatCfg.Stream, "timeoutSec", chatCfg.TimeoutSec)
	} else {
		slog.Info("llm enhancer disabled（model_configs 表或 env 未配置 apiKey）")
	}
	// 注意：未配置 key 时保持接口为 nil（不能把 typed-nil 的 *ai.Client 赋给接口——
	// 接口非 nil 会让 RCAAgent 的 nil 守卫失效，enhance 空指针 panic）
	var rcaLLM agent.RCALLM
	if llmClient != nil {
		rcaLLM = llmClient
	}

	// 4.7 RAG 知识检索层（RAG_PLAN.md Phase 0~2；默认关闭，off 模式完全保持现有行为）。
	// 本期实现 readonly：知识索引 + 相似事故只读 API + 反馈，不注入 RCA（Shadow/assist 后续阶段）。
	consumeCtx, stopConsume := context.WithCancel(context.Background())
	defer stopConsume()
	var ragSvc *rag.Service
	if cfg.RAGEnabled && cfg.RAGMode != "off" {
		// Embedding 生效配置（仅 AIOPS_EMBEDDING* env；不走 DB，无配置入口）
		embEnv := modelcfg.EmbeddingEnv{
			Driver: cfg.EmbeddingDriver, BaseURL: cfg.EmbeddingBaseURL, APIKey: cfg.EmbeddingAPIKey,
			Model: cfg.EmbeddingModel, Dimension: cfg.EmbeddingDimension,
			TimeoutSec: cfg.EmbeddingTimeoutSec, BatchSize: cfg.EmbeddingBatchSize,
		}
		embCfg := modelcfg.EffectiveEmbedding(embEnv)
		// 装配时二次校验（config.Load 已校验 env，此处按生效值兜底：
		// RAG 开启时契约错误显式退出，不静默降级）
		if embCfg.Enabled && embCfg.Driver == "openai-compat" {
			if embCfg.BaseURL == "" || embCfg.Model == "" {
				slog.Error("rag: embedding 生效配置缺 baseURL/model（AIOPS_EMBEDDINGBASEURL/AIOPS_EMBEDDINGMODEL）")
				os.Exit(1)
			}
			if embCfg.APIKey == "" {
				slog.Error("rag: embedding 生效配置缺 apiKey（AIOPS_EMBEDDINGAPIKEY）")
				os.Exit(1)
			}
			if embCfg.Dimension <= 0 {
				slog.Error("rag: embedding 生效配置维度 <= 0（向量维度是索引契约）")
				os.Exit(1)
			}
		}
		// 扩展/向量列是索引契约：安装失败显式退出（运维动作，不静默降级）
		if err := rag.EnsureSchema(db, rag.SchemaOptions{Dimension: embCfg.Dimension, HNSW: cfg.RAGIndexHNSW}); err != nil {
			slog.Error("rag: ensure schema", "err", err)
			os.Exit(1)
		}
		var emb embedding.Provider
		switch embCfg.Driver {
		case "openai-compat":
			if embCfg.Enabled { // 未配 key → 不建客户端（keyword-only）
				emb = embedding.NewOpenAICompat(embCfg.BaseURL, embCfg.APIKey, embCfg.Model,
					time.Duration(embCfg.TimeoutSec)*time.Second, embCfg.BatchSize)
			}
		case "local":
			emb = embedding.NewMock(embCfg.Dimension) // 确定性 mock（仅 env 回退路径；演示/CI）
		}
		ragSvc = rag.NewService(db, rag.Options{
			Emb:              emb,
			IndexWorkers:     cfg.RAGIndexWorkers,
			BatchSize:        embCfg.BatchSize,
			IndexVersion:     cfg.RAGIndexVersion,
			Interval:         time.Duration(cfg.RAGIndexIntervalSec) * time.Second,
			RetrievalTimeout: time.Duration(cfg.RAGRetrievalTimeoutMS) * time.Millisecond,
			TopKKeyword:      cfg.RAGTopKKeyword,
			TopKVector:       cfg.RAGTopKVector,
			TopKFinal:        cfg.RAGTopKFinal,
			MaxContextTokens: cfg.RAGMaxContextTokens,
			Mode:             cfg.RAGMode,
		})
		// RAG 指标（零依赖注册表；检索/索引/embedding/反馈）
		reg.Counter("aiops_rag_retrieval_total", "RAG 检索结果（result=success|empty|degraded）", "result", "degraded_reason")
		reg.Histogram("aiops_rag_retrieval_duration_seconds", "RAG 检索总耗时（秒）", metrics.SecondsBuckets)
		reg.Histogram("aiops_rag_lane_exact_duration_seconds", "精确路耗时（秒）", metrics.SecondsBuckets)
		reg.Histogram("aiops_rag_lane_keyword_duration_seconds", "关键词路耗时（秒）", metrics.SecondsBuckets)
		reg.Histogram("aiops_rag_lane_vector_duration_seconds", "向量路耗时（秒）", metrics.SecondsBuckets)
		reg.Counter("aiops_rag_index_jobs_total", "索引任务结算（status= succeeded|failed）", "status", "operation")
		reg.Histogram("aiops_rag_index_job_duration_seconds", "索引任务耗时（秒）", metrics.SecondsBuckets)
		reg.Counter("aiops_rag_embedding_requests_total", "embedding 请求", "model", "result")
		reg.Counter("aiops_rag_embedding_tokens_total", "embedding 调用次数（单条调用粒度）", "model")
		reg.Counter("aiops_rag_feedback_total", "人工反馈", "rating")
		reg.Gauge("aiops_rag_index_backlog", "索引积压（pending+running）", func() float64 { return float64(ragSvc.Backlog()) })
		reg.Gauge("aiops_rag_index_lag_seconds", "索引滞后秒（无成功任务为 0）", func() float64 { return ragSvc.LagSeconds() })
		ragSvc.SetMetrics(reg)
		// 源数据同事务入队（结案/解决/剧本 CRUD → knowledge_jobs），worker 异步消费
		svc.SetResIndexHook(ragSvc.EnqueueIncident)
		pbSvc.SetIndexHook(ragSvc.EnqueuePlaybook)
		go ragSvc.RunIndexer(consumeCtx)
		if cfg.RAGBackfillOnBoot {
			if n, err := ragSvc.Backfill(context.Background()); err != nil {
				slog.Warn("rag: backfill", "err", err)
			} else {
				slog.Info("rag: backfill done", "enqueued", n)
			}
		}
		embState := "keyword-only"
		if emb != nil {
			embState = embCfg.Driver + "/" + emb.ModelID()
		}
		slog.Info("rag enabled", "mode", cfg.RAGMode, "embedding", embState,
			"indexVersion", cfg.RAGIndexVersion, "retrievalTimeoutMS", cfg.RAGRetrievalTimeoutMS)
	} else {
		slog.Info("rag disabled (set AIOPS_RAGENABLED=true AIOPS_RAGMODE=readonly to enable)")
	}

	// 4.8b 数据源证据（Prometheus 指标 / Loki 日志；EvidenceEnabled 时装配）。
	// AI 助手多 query_metrics/search_logs 两个工具 + RCA 自动注入日志/指标证据进证据链。
	evTimeout := time.Duration(cfg.EvidenceTimeoutSec) * time.Second
	var promCli *evidence.PromClient
	var lokiCli *evidence.LokiClient
	var evCollector *evidence.EvidenceCollector
	if cfg.PrometheusURL != "" {
		promCli = evidence.NewPromClient(cfg.PrometheusURL, cfg.PrometheusUser, cfg.PrometheusPass, evTimeout)
	}
	if cfg.LokiURL != "" {
		lokiCli = evidence.NewLokiClient(cfg.LokiURL, evTimeout)
	}
	if cfg.EvidenceEnabled {
		var promQueries []string
		if cfg.PrometheusQueries != "" {
			for _, q := range strings.Split(cfg.PrometheusQueries, ";") {
				if q = strings.TrimSpace(q); q != "" {
					promQueries = append(promQueries, q)
				}
			}
		}
		evCollector = evidence.NewEvidenceCollector(promCli, lokiCli, promQueries)
		slog.Info("evidence enabled", "prometheus", promCli != nil, "loki", lokiCli != nil)
	}

	// 5. 编排器（Agent 智能路径）
	orch := buildOrchestrator(cfg, repo, svc, topoSvc, pbSvc, breaker, bus, hub, auditSvc, ntf, rcaLLM, evCollector)

	// 5.5 工作流引擎（YAML 工作流，provider: agent 可调编排器）
	// execWorkers 为执行并发上限：落库/防重在队列 worker 同步完成，执行异步有界并发，慢步骤不阻塞 agent.run
	wfSvc := workflow.NewService(db, svc, orch, cfg.WorkflowExecWorkers)
	_ = wfSvc.EnsureDefaults(context.Background(), "default") // 零配置补种演示工作流

	// 6. 队列消费者：按任务类型分发（agent.run → 编排器；workflow.trigger → 工作流引擎；notify.send → 通知节流）
	go func() {
		handler := func(ctx context.Context, t queue.Task) error {
			switch t.Type {
			case queue.TaskAgentRun:
				return orch.HandleAgentTask(ctx, t)
			case queue.TaskWorkflowTrigger:
				return wfSvc.HandleTriggerTask(ctx, t)
			case queue.TaskNotifySend:
				return notifythrottle.HandleNotifyTask(ctx, t, th, ntf, auditSvc, cfg.NotifierBaseURL)
			default:
				slog.Warn("queue: unknown task type", "type", t.Type)
				return nil
			}
		}
		if err := q.Consume(consumeCtx, handler); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("queue consumer exited", "err", err)
		}
	}()

	// 6.1 告警级通知（KEEPON 5.4 节流）：pipeline 命中 alert.new/update → 异步 notify.send。
	// 节流器内存态：首条立即 / 冷却合并 / 升级与恢复突破 / 周期汇总；无渠道 fail-open 不打扰。
	if cfg.NotifyEnabled {
		pipe.SetNotifyTrigger(func(ctx context.Context, a *alert.Alert) {
			payload, err := notifythrottle.MarshalNotifyTask(a)
			if err != nil {
				slog.Warn("pipeline: marshal notify.send", "alert", a.ID, "err", err)
				return
			}
			if err := q.Enqueue(ctx, queue.Task{Type: queue.TaskNotifySend, TenantID: a.TenantID, Payload: payload}); err != nil {
				slog.Warn("pipeline: enqueue notify.send", "alert", a.ID, "err", err)
			}
		})
		slog.Info("alert notify enabled", "cooldownSec", cfg.NotifyCooldownSec,
			"summaryMin", cfg.NotifySummaryMin, "channels", ntf.Enabled())
	}

	// 6.2 工作流触发投递（pipeline 命中 → 异步 workflow.trigger；AIOPS_WORKFLOWENABLED=false 关闭）
	if cfg.WorkflowEnabled {
		pipe.SetWorkflowTrigger(func(ctx context.Context, a *alert.Alert) {
			payload, _ := json.Marshal(map[string]any{"alert_id": a.ID})
			if err := q.Enqueue(ctx, queue.Task{Type: queue.TaskWorkflowTrigger, TenantID: a.TenantID, Payload: payload}); err != nil {
				slog.Warn("pipeline: enqueue workflow.trigger", "alert", a.ID, "err", err)
			}
		})
		slog.Info("workflow engine enabled")
	}

	// 6.3 interval 触发器调度器（type: interval 的工作流按 with.interval 周期执行）
	wfSvc.StartIntervalScheduler(consumeCtx, time.Duration(cfg.WorkflowSchedIntervalSec)*time.Second)

	// 6.5 AI 聚类 worker：定时扫描未归属告警 → 达到阈值确认生成 Incident（可 AIOPS_AICLUSTERENABLED=false 关闭）
	if cfg.AIClusterEnabled {
		interval := time.Duration(cfg.AIClusterIntervalSec) * time.Second
		if interval <= 0 {
			interval = 60 * time.Second
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			aiSvc.RunAll(consumeCtx) // 启动即跑一次
			for {
				select {
				case <-ticker.C:
					aiSvc.RunAll(consumeCtx)
				case <-consumeCtx.Done():
					return
				}
			}
		}()
		slog.Info("ai cluster worker enabled", "threshold", cfg.AIClusterThreshold, "windowMin", cfg.AIClusterWindowMin, "intervalSec", interval)
	}

	// 6.6 数据保留策略 worker：alerts/audit_logs/workflow_runs 按天分级保留、分批删除（可 AIOPS_RETENTIONENABLED=false 关闭）
	// alerts 分区表模式：整月过期分区 DROP + 当月残余行级 DELETE（AIOPS_ALERTSPARTITIONS=false 退回纯行级删除）
	if cfg.RetentionEnabled {
		retSvc := retention.New(db, []retention.Rule{
			{Table: "alerts", TimeColumn: "created_at", Days: cfg.RetentionAlertsDays, DropPartitions: cfg.AlertPartitions},
			{Table: "audit_logs", TimeColumn: "created_at", Days: cfg.RetentionAuditDays},
			{Table: "workflow_runs", TimeColumn: "started_at", Days: cfg.RetentionWorkflowRunsDays},
		})
		interval := time.Duration(cfg.RetentionIntervalMin) * time.Minute
		if interval <= 0 {
			interval = 60 * time.Minute
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			_ = retSvc.RunOnce(consumeCtx) // 启动即清理一次
			for {
				select {
				case <-ticker.C:
					_ = retSvc.RunOnce(consumeCtx)
				case <-consumeCtx.Done():
					return
				}
			}
		}()
		slog.Info("retention worker enabled",
			"alertsDays", cfg.RetentionAlertsDays, "auditDays", cfg.RetentionAuditDays,
			"workflowRunsDays", cfg.RetentionWorkflowRunsDays, "intervalMin", cfg.RetentionIntervalMin)
	}

	// 6.7 数据备份 worker（KEEPON 4.1）：定时 pg_dump + 轮转 + 可选镜像加密/恢复校验。
	// 启动补跑（距上次成功超间隔才跑）；docker exec 执行 pg_dump（本机无客户端）。
	var bkSvc *backup.Service
	if cfg.BackupEnabled {
		bkDSN := cfg.DBDSN
		if bkDSN == "" {
			bkDSN = store.DefaultDSN // 与 store.Open 空 DSN 回退一致
		}
		bkSvc = backup.New(db, backup.Config{
			DSN:            bkDSN,
			Dir:            cfg.BackupDir,
			Keep:           cfg.BackupKeep,
			MaxBytes:       int64(cfg.BackupMaxGB) << 30,
			MirrorDir:      cfg.BackupMirrorDir,
			MirrorEncrypt:  cfg.BackupMirrorEncrypt,
			PassphraseFile: cfg.BackupPassphraseFile,
			Verify:         cfg.BackupVerify,
			Timeout:        time.Duration(cfg.BackupTimeoutMin) * time.Minute,
			Interval:       time.Duration(cfg.BackupIntervalHours) * time.Hour,
		}, backup.NewExecutor(cfg.BackupPgContainer, "aiops")) // PGPASSWORD 与 compose 环境一致
		bkSvc.SetMetrics(reg)
		interval := time.Duration(cfg.BackupIntervalHours) * time.Hour
		go func() {
			if cfg.BackupOnBoot {
				_ = bkSvc.RunIfDue(consumeCtx) // 启动补跑（距上次成功超间隔才执行）
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					_ = bkSvc.RunIfDue(consumeCtx)
				case <-consumeCtx.Done():
					return
				}
			}
		}()
		slog.Info("backup worker enabled",
			"dir", cfg.BackupDir, "intervalHours", cfg.BackupIntervalHours,
			"keep", cfg.BackupKeep, "maxGB", cfg.BackupMaxGB,
			"mirror", cfg.BackupMirrorDir != "", "verify", cfg.BackupVerify)
	}

	// 6.8 通知汇总 worker（KEEPON 5.4）：冷却期累计的重复按周期发汇总（"过去 30 分钟重复 47 次"）。
	// NotifySummaryMin=0 时 DueSummaries 恒空（关闭汇总）；无渠道时 Notify fail-open。
	if cfg.NotifyEnabled && cfg.NotifySummaryMin > 0 {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					for _, task := range th.DueSummaries() {
						notifythrottle.HandleSummaryTask(context.Background(), task, ntf, auditSvc, cfg.NotifierBaseURL,
							time.Duration(cfg.NotifySummaryMin)*time.Minute)
					}
				case <-consumeCtx.Done():
					return
				}
			}
		}()
		slog.Info("notify summary worker enabled", "summaryMin", cfg.NotifySummaryMin)
	}

	// 4.8 总览聚合服务（KEEPON 5.1；assistant 工具与 /overview 共用同一实例）
	// typed-nil 防护：nil 具体指针显式转 nil 接口（否则接口判空失效，RAG/备份关闭时 panic）。
	var ovSvc *overview.Service
	if sqlDB, err := db.DB(); err == nil {
		var ragI overview.RAGStatuser
		if ragSvc != nil {
			ragI = ragSvc
		}
		var bkI overview.BackupLister
		if bkSvc != nil {
			bkI = bkSvc
		}
		ovSvc = overview.New(db, sqlDB, q, ragI, bkI, startTime)
	} else {
		slog.Warn("overview: get sql.DB", "err", err)
	}

	// 4.9 对话式 Agent（AGENT_CHAT_PLAN Phase 0+1 只读 MVP；默认关闭）
	// 与告警热路径隔离：独立 DB 租约 worker + 信号量；生成模型 mock 先行（未配置时
	// 服务照常启动，run 快速失败 model_not_configured）。
	var assistSvc *assistant.Service
	var assistBroker *assistant.Broker
	if cfg.AssistantEnabled {
		asstEnv := modelcfg.ChatEnv{Driver: cfg.AssistantChatDriver, BaseURL: cfg.AssistantChatBaseURL,
			APIKey: cfg.AssistantChatAPIKey, Model: cfg.AssistantChatModel,
			TimeoutSec: cfg.AssistantChatTimeoutSec, Stream: cfg.AssistantChatStream}
		asstCfg, err := mcCfg.ResolveAssistant(context.Background(), asstEnv)
		if err != nil {
			slog.Warn("modelcfg: resolve assistant, fallback env", "err", err)
			asstCfg = modelcfg.EffectiveAssistant(nil, false, asstEnv)
		}
		var asstModel assistant.ChatModel // typed-nil 防护：未配置时接口保持 nil（run 快速失败）
		if asstCfg.Enabled && asstCfg.APIKey != "" {
			lc := llm.NewClient(asstCfg.BaseURL, asstCfg.APIKey, asstCfg.Model)
			lc.SetTimeout(time.Duration(asstCfg.TimeoutSec) * time.Second)
			lc.SetStream(asstCfg.Stream)
			asstModel = lc
		}
		assistBroker = assistant.NewBroker(db)
		assistReg := assistant.NewRegistry(assistant.ToolDeps{
			Overview: ovSvc, Alerts: repo, Incidents: svc, Topology: topoSvc,
			RAG: ragSvc, Playbooks: pbSvc, // Phase 2：RAG nil 时知识工具不注册；剧本元数据工具
			Prometheus: promCli, Loki: lokiCli,
		})
		limits := assistant.Limits{
			MaxConcurrency: cfg.AssistantMaxConcurrency,
			MaxModelRounds: cfg.AssistantMaxModelRounds,
			MaxToolCalls:   cfg.AssistantMaxToolCalls,
			RunTimeout:     time.Duration(cfg.AssistantRunTimeoutSec) * time.Second,
			ToolTimeout:    time.Duration(cfg.AssistantToolTimeoutSec) * time.Second,
		}
		asstRunner := assistant.NewRunner(db, asstModel, asstCfg.Model, assistReg, assistBroker, auditSvc,
			limits, assistant.NewContextBuilder(svc)) // Phase 2：thread 绑定事故上下文摘要
		asstRunner.SetMetrics(reg)
		assistant.RegisterMetrics(reg, func() float64 { return float64(asstRunner.ActiveRuns()) })
		assistSvc = assistant.NewService(db, asstCfg.Model, assistBroker, auditSvc, asstRunner, svc)
		assistSvc.SetMetrics(reg)
		// Phase 3：动作执行器（确认路径复用领域 Service 与 Orchestrator 审批语义）
		assistSvc.SetExecutor(&assistant.ActionExecutor{
			Alerts: repo, Incidents: svc, Orchestrator: orch,
		})
		go asstRunner.RunWorker(consumeCtx)
		slog.Info("assistant enabled", "model", asstCfg.Model, "source", asstCfg.Source)
	} else {
		slog.Info("assistant disabled (AIOPS_ASSISTANTENABLED=false)")
	}

	// 7. HTTP API + WS
	server := api.NewServer(cfg, repo, svc, rules, builtin.Registry, topoSvc, authSvc, hub, pipe,
		pbSvc, auditSvc, orch, ntf, ntfCfg, mwSvc, aiSvc, wfSvc, ragSvc, mcCfg, tickets, bkSvc,
		assistSvc, assistBroker)
	server.SetMetrics(reg)

	// 4.10 外部可观测系统入口（KEEPON 6.2）：env 首启种子，之后 DB 为准（保存后热生效）
	extSvc := external.NewService(db)
	if cfg.ExternalSystems != "" {
		var seeds []external.SeedItem
		if err := json.Unmarshal([]byte(cfg.ExternalSystems), &seeds); err != nil {
			slog.Warn("external: AIOPS_EXTERNALSYSTEMS 不是合法 JSON 数组，跳过种子", "err", err)
		} else if err := extSvc.Seed(context.Background(), "default", seeds); err != nil {
			slog.Warn("external: seed", "err", err)
		}
	}
	server.SetExternal(extSvc)

	// 4.11 首版异常检测（KEEPON 七，P3）：告警数量季节性基线，只读提示
	server.SetAnomaly(anomaly.NewService(db))
	if ovSvc != nil {
		server.SetOverview(ovSvc)
	}
	// 基础设施指标（监控平板：主机 CPU/内存 + PostgreSQL 状态 + docker 容器占用）
	server.SetInfra(infra.New(db))
	router := gin.New()
	router.Use(gin.Recovery())
	if cfg.LogRequests {
		router.Use(api.RequestIDMiddleware(), api.AccessLogMiddleware())
	}
	server.Register(router)

	// 生产加固：ReadHeaderTimeout 固定 5s 防 slowloris；ReadTimeout/WriteTimeout 防慢
	// 请求体/慢响应客户端占住 goroutine；IdleTimeout 回收僵尸 keep-alive 连接。WS 端点
	// （hub.ServeWS）在 Accept 前显式清除底层连接的读/写 deadline，长连接不受误伤。
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       time.Duration(cfg.HTTPReadTimeoutSec) * time.Second,
		WriteTimeout:      time.Duration(cfg.HTTPWriteTimeoutSec) * time.Second,
		IdleTimeout:       time.Duration(cfg.HTTPIdleTimeoutSec) * time.Second,
	}
	go func() {
		slog.Info("http listening", "addr", cfg.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http serve", "err", err)
			os.Exit(1)
		}
	}()

	// 8. 优雅退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	slog.Info("shutting down")
	stopConsume()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

// buildOrchestrator 组装编排表（monitor → rca → heal → change，安全护栏链驱动）。
// ev 为数据源证据收集器（EvidenceEnabled 时装配；nil 时 RCA 行为不变）。
func buildOrchestrator(cfg *config.Config, repo *alert.Repository, svc *incident.Service,
	topoSvc *topology.Service, pbSvc *playbook.Service, breaker *circuit.Breaker,
	bus eventbus.Bus, hub *pusher.Hub, auditSvc *eventbus.AuditService,
	ntf *notifier.Service, rcaLLM agent.RCALLM, ev *evidence.EvidenceCollector) *orchestrator.Orchestrator {
	agentCfg := agent.Cfg{
		DryRun:           cfg.AgentDryRun,
		RCAConfidence:    cfg.RCAConfidence,
		MaxDepth:         cfg.AgentMaxDepth,
		BlastRadiusLimit: cfg.AgentBlastRadiusLimit,
		AutoHealLevel:    cfg.AgentAutoHealLevel,
	}
	cond := func(st *incident.State) bool { return st.AlertID != "" }
	nodes := []orchestrator.Node{
		{Name: "monitor", Agent: agent.NewMonitorAgent(), MaxRetries: 2},
		{Name: "rca", Agent: agent.NewRCAAgent(topoSvc, rcaLLM, ev), MaxRetries: 2, Condition: cond},
		// heal 制定自愈动作：dry-run → 爆炸半径 → 熔断器 → L0/L1/L2 审批门控
		{Name: "heal", Agent: agent.NewHealAgent(pbSvc, topoSvc, breaker), MaxRetries: 2, Condition: cond},
		// change 对自愈动作做 5 维风险评分，辅助审批决策
		{Name: "change", Agent: agent.NewChangeAgent(), MaxRetries: 2, Condition: cond},
	}
	return orchestrator.New(repo, svc, nodes, agentCfg, bus, hub, auditSvc, ntf)
}

// setupLogger 按配置设置日志：级别 + 可选文件持久化（lumberjack 大小/数量/天数三重轮转，
// KEEPON 4.2）+ 全局脱敏（敏感 key/URL 查询参数 → [REDACTED]）。
// 设置了 AIOPS_LOGFILE 时 stdout 与文件双写，Docker 运行时（不配 LOGFILE）继续纯 stdout。
func setupLogger(cfg *config.Config) {
	var lv slog.Level
	switch cfg.LogLevel {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	w := io.Writer(os.Stdout)
	if cfg.LogFile != "" {
		if err := os.MkdirAll(dirOf(cfg.LogFile), 0o700); err != nil {
			slog.Warn("logger: mkdir log dir", "err", err)
		}
		w = io.MultiWriter(os.Stdout, &lumberjack.Logger{
			Filename:   cfg.LogFile,
			MaxSize:    cfg.LogMaxMB,      // MB，默认 100
			MaxBackups: cfg.LogMaxBackups, // 默认 10
			MaxAge:     cfg.LogMaxAgeDays, // 默认 30
			Compress:   true,
		})
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv, ReplaceAttr: logutil.RedactAttr})
	slog.SetDefault(slog.New(h))
}

// dirOf 返回路径所在目录（"" 表示无目录部分）。
func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return ""
}
