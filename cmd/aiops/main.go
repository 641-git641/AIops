// Command aiops 是 AIOps 告警平台单体服务入口。
// 装配顺序：config → store(PostgreSQL) → 领域服务 → 事件管线 → 编排器 → API → worker。
// 双轨架构：确定性热路径（pipeline）与 Agent 智能路径（orchestrator）共享同一进程。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aiops/internal/agent"
	"aiops/internal/ai"
	"aiops/internal/alert"
	"aiops/internal/api"
	"aiops/internal/auth"
	"aiops/internal/cel"
	"aiops/internal/circuit"
	"aiops/internal/config"
	"aiops/internal/eventbus"
	"aiops/internal/incident"
	"aiops/internal/maintenance"
	"aiops/internal/metrics"
	"aiops/internal/notifier"
	"aiops/internal/orchestrator"
	"aiops/internal/pipeline"
	"aiops/internal/playbook"
	"aiops/internal/provider/builtin" // 触发 init() 注册内置 provider
	"aiops/internal/pusher"
	"aiops/internal/queue"
	"aiops/internal/retention"
	"aiops/internal/store"
	"aiops/internal/topology"
	"aiops/internal/workflow"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load", "err", err)
		os.Exit(1)
	}
	setupLogger(cfg.LogLevel)
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

	// 3. 基础设施
	bus := eventbus.NewMemoryBus()
	defer bus.Close()
	hub := pusher.NewHub()
	q := queue.NewMemoryQueue(4)
	defer q.Close()

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

	// 4.5 LLM 增强（RCA 根因补充 + AI 聚类摘要）：配置 apiKey 后启用（openai-compat，默认 DeepSeek）；否则纯确定性
	var llmClient *ai.Client
	if cfg.LLMDriver == "openai-compat" && cfg.LLMAPIKey != "" {
		llmClient = ai.NewClient(cfg.LLMBaseURL, cfg.LLMAPIKey, cfg.LLMModel)
		llmClient.SetTimeout(time.Duration(cfg.LLMTimeoutSec) * time.Second)
		llmClient.SetStream(cfg.LLMStream)
		aiSvc.SetClusterLLM(llmClient) // 聚类摘要也接 LLM（非确定性模板，失败 fallback 确定性）
		slog.Info("llm enhancer enabled", "model", cfg.LLMModel, "stream", cfg.LLMStream, "timeoutSec", cfg.LLMTimeoutSec)
	} else {
		slog.Info("llm enhancer disabled (set AIOPS_LLMAPIKEY to enable)")
	}
	var rcaLLM agent.RCALLM = llmClient

	// 5. 编排器（Agent 智能路径）
	orch := buildOrchestrator(cfg, repo, svc, topoSvc, pbSvc, breaker, bus, hub, auditSvc, ntf, rcaLLM)

	// 5.5 工作流引擎（keep 式 YAML 工作流，provider: agent 可调编排器）
	// execWorkers 为执行并发上限：落库/防重在队列 worker 同步完成，执行异步有界并发，慢步骤不阻塞 agent.run
	wfSvc := workflow.NewService(db, svc, orch, cfg.WorkflowExecWorkers)
	_ = wfSvc.EnsureDefaults(context.Background(), "default") // 零配置补种演示工作流

	// 6. 队列消费者：按任务类型分发（agent.run → 编排器；workflow.trigger → 工作流引擎）
	consumeCtx, stopConsume := context.WithCancel(context.Background())
	defer stopConsume()
	go func() {
		handler := func(ctx context.Context, t queue.Task) error {
			switch t.Type {
			case queue.TaskAgentRun:
				return orch.HandleAgentTask(ctx, t)
			case queue.TaskWorkflowTrigger:
				return wfSvc.HandleTriggerTask(ctx, t)
			default:
				slog.Warn("queue: unknown task type", "type", t.Type)
				return nil
			}
		}
		if err := q.Consume(consumeCtx, handler); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("queue consumer exited", "err", err)
		}
	}()

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

	// 7. HTTP API + WS
	server := api.NewServer(cfg, repo, svc, rules, builtin.Registry, topoSvc, authSvc, hub, pipe,
		pbSvc, auditSvc, orch, ntf, ntfCfg, mwSvc, aiSvc, wfSvc)
	server.SetMetrics(reg)
	router := gin.New()
	router.Use(gin.Recovery())
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
func buildOrchestrator(cfg *config.Config, repo *alert.Repository, svc *incident.Service,
	topoSvc *topology.Service, pbSvc *playbook.Service, breaker *circuit.Breaker,
	bus eventbus.Bus, hub *pusher.Hub, auditSvc *eventbus.AuditService,
	ntf *notifier.Service, rcaLLM agent.RCALLM) *orchestrator.Orchestrator {
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
		{Name: "rca", Agent: agent.NewRCAAgent(topoSvc, rcaLLM), MaxRetries: 2, Condition: cond},
		// heal 制定自愈动作：dry-run → 爆炸半径 → 熔断器 → L0/L1/L2 审批门控
		{Name: "heal", Agent: agent.NewHealAgent(pbSvc, topoSvc, breaker), MaxRetries: 2, Condition: cond},
		// change 对自愈动作做 5 维风险评分，辅助审批决策
		{Name: "change", Agent: agent.NewChangeAgent(), MaxRetries: 2, Condition: cond},
	}
	return orchestrator.New(repo, svc, nodes, agentCfg, bus, hub, auditSvc, ntf)
}

// setupLogger 按配置设置日志级别。
func setupLogger(level string) {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv})))
}
