// Package config 负责加载应用配置（env + yaml + 默认值）。
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Config 应用全局配置。
type Config struct {
	// 服务
	Addr string // HTTP 监听地址，默认 :8080

	// HTTP 服务器超时（生产加固）：防慢响应/慢 body/僵尸 keep-alive 连接占住资源。
	// ReadHeaderTimeout 固定 5s 防 slowloris；WS 长连接不受影响——hub.ServeWS 在
	// Accept 前显式清除 net/http 落在底层连接上的读/写 deadline（coder/websocket
	// hijack 后不清除残留，不清理则超时秒后 WS 读写被强制断开）。
	HTTPReadTimeoutSec  int // 读完整请求（含 body）超时秒（默认 15）
	HTTPWriteTimeoutSec int // 写响应超时秒（默认 30；须 > LLMTimeoutSec）
	HTTPIdleTimeoutSec  int // keep-alive 空闲超时秒（默认 120；对 WS 连接不生效，安全）

	// 存储
	DBDriver string // sqlite（默认）| postgres
	DBDSN    string // sqlite 文件路径或 postgres DSN

	// 存储加固（Phase 3 规模化）：连接池调优 + 启动重试 + 保留策略
	DBMaxOpenConns       int // 连接池最大打开连接数（默认 20）
	DBMaxIdleConns       int // 最大空闲连接数（默认 20，= MaxOpen 防连接抖动；Windows 回环 TIME_WAIT 冲突会引发 dial 错误）
	DBConnMaxLifetimeSec int // 连接最长存活秒（默认 1800；0 表示不限）
	DBConnMaxIdleTimeSec int // 空闲连接回收秒（默认 300；0 表示不限）
	DBConnectRetries     int // 启动连接重试次数（默认 5，postgres 未就绪时等待拉起）

	// 数据保留策略（worker 定时分批清理，防止无限膨胀；天数 <= 0 关闭该表清理）
	RetentionEnabled          bool // 保留策略 worker 开关（默认 true）
	RetentionAlertsDays       int  // alerts 明细保留天数（默认 30；事故/当前状态不受影响）
	RetentionAuditDays        int  // audit_logs 保留天数（默认 180）
	RetentionWorkflowRunsDays int  // workflow_runs 保留天数（默认 90）
	RetentionIntervalMin      int  // 清理执行间隔分钟（默认 60）

	// 分区化（规模化延伸）：alerts 按 created_at 月分区，保留策略整分区 DROP 清理
	AlertPartitions bool // alerts 月分区开关（默认 true；关闭则保留策略退回行级 DELETE，分区表降级可用）

	// 队列
	QueueDriver string // memory（默认）| asynq

	// 事件总线
	EventBusDriver string // memory（默认）| kafka

	// 认证
	AuthType string // noauth（默认）| apikey

	// 通知（钉钉/飞书/企微/通用 webhook；未配置 webhook 的渠道自动跳过）
	NotifierChannels string // 逗号分隔启用渠道：dingtalk,feishu,wecom,http
	NotifierDingTalk string // 钉钉机器人 webhook
	NotifierFeishu   string // 飞书机器人 webhook
	NotifierWecom    string // 企微机器人 webhook
	NotifierHTTP     string // 通用 webhook（POST JSON Message）
	NotifierBaseURL  string // 前端地址，用于 incident 详情链接

	// 通知节流（KEEPON 5.4：同 fingerprint 首条立即通知；冷却期重复合并；严重度升级/恢复突破；周期汇总）
	NotifyEnabled     bool // 告警级通知总开关（默认 true 开箱可用；无渠道 fail-open 不打扰）
	NotifyCooldownSec int  // 冷却时间秒（默认 300；0=每事件都发，语义=关闭节流）
	NotifySummaryMin  int  // 汇总周期分钟（默认 30；0=关闭汇总）

	// Agent
	AgentDryRun           bool    // 自愈动作默认 dry-run（默认 true，安全优先）
	AgentEnabled          bool    // 是否启用 Agent 流水线
	RCAConfidence         float64 // RCA 置信度门槛
	AgentMaxDepth         int     // 依赖链遍历深度
	AgentBlastRadiusLimit float64 // 爆炸半径超限强制降级 L2（默认 0.2）
	AgentAutoHealLevel    string  // 允许自动执行的最高级别（默认 L0，即仅 L0 自动）

	// AI
	AIEnabled     bool
	LLMDriver     string // openai-compat | off
	LLMBaseURL    string
	LLMAPIKey     string
	LLMModel      string
	LLMTimeoutSec int  // LLM 调用整体超时秒（默认 15；流式累计期间同样受控）
	LLMStream     bool // SSE 流式响应开关（默认 true；端点不支持流式时置 false）

	// AI 聚类（确定性事件关联，不依赖 LLM）
	AIClusterEnabled     bool // 定时 worker 开关（默认 true）
	AIClusterThreshold   int  // 达到该告警数才确认生成 Incident（默认 3）
	AIClusterWindowMin   int  // 聚类时间窗口/回溯分钟（默认 10）
	AIClusterIntervalSec int  // worker 执行间隔秒（默认 60）

	// 工作流（YAML 工作流引擎）
	WorkflowEnabled          bool // 是否投递 workflow.trigger 任务并消费（默认 true）
	WorkflowExecWorkers      int  // 工作流执行并发上限（慢步骤不阻塞队列 worker，默认 8）
	WorkflowSchedIntervalSec int  // interval 触发器扫描间隔秒（默认 10）

	// RAG（历史知识检索层，RAG_PLAN.md Phase 0~2；默认关闭，off 模式完全保持现有行为）
	RAGEnabled            bool   // RAG 总开关（默认 false）
	RAGMode               string // off|readonly（默认 off；shadow/assist 未实现，配置即报错）
	RAGRetrievalTimeoutMS int    // 检索总超时毫秒（默认 800，超时立即降级关键词/空证据，不阻塞）
	RAGTopKKeyword        int    // 关键词召回条数（默认 40）
	RAGTopKVector         int    // 向量召回条数（默认 40）
	RAGTopKFinal          int    // 融合后最终证据条数（默认 6，4~8 区间）
	RAGMaxContextTokens   int    // RAG 上下文 token 预算（默认 5000）
	RAGIndexWorkers       int    // 索引 worker 数（默认 2）
	RAGIndexVersion       string // 在线索引版本（默认 v1；换模型/维度必须递增并重建）
	RAGIndexIntervalSec   int    // 索引 worker 轮询间隔秒（默认 5）
	RAGIndexHNSW          bool   // 向量 HNSW 索引开关（默认 false：精确扫描基线，数据量大后再启用）
	RAGBackfillOnBoot     bool   // 启动时回填历史已解决事故与已启用剧本（默认 true，幂等）
	RAGAdminAPIs          bool   // 开放管理端点（rag/status、reindex、rag-trace；默认 false 一律 403）
	RAGAdminTenants       string // apikey 模式下允许管理端点的租户（逗号分隔）

	// 模型配置管理（model_configs 表：Chat/Embedding API 接入；携带密钥，独立管理开关）
	ModelCfgAdminAPIs    bool   // 开放模型配置管理端点（默认 false 一律 403）
	ModelCfgAdminTenants string // apikey 模式下允许管理模型配置的租户（逗号分隔）

	// 数据备份（KEEPON 4.1：定时 pg_dump + 轮转 + 可选异地镜像加密 + 恢复校验；默认关闭）
	BackupEnabled        bool   // 备份 worker 开关（默认 false）
	BackupIntervalHours  int    // 执行间隔小时（默认 24；距上次成功超间隔才补跑）
	BackupDir            string // 备份目录（默认 ./backups，0700 创建、文件 0600）
	BackupKeep           int    // 本机保留份数（默认 10；数量限）
	BackupMaxGB          int    // 本机保留总容量上限 GB（默认 5；与 Keep 双限，任一达限即清理）
	BackupMirrorDir      string // 异地镜像目录（默认 "" 不镜像；可挂载 NAS/另一磁盘）
	BackupMirrorEncrypt  bool   // 镜像 AES-256-GCM 加密（默认 false）
	BackupPassphraseFile string // 加密口令文件路径（镜像加密必填，建议权限 0600）
	BackupPgContainer    string // "aiops-postgres"（docker exec，默认）| "host"（本机 pg 客户端）
	BackupVerify         bool   // 备份后恢复到临时库校验（默认 true）
	BackupTimeoutMin     int    // 单次备份超时分钟（默认 60）
	BackupOnBoot         bool   // 启动补跑开关（默认 true）

	// Embedding（独立于 Chat LLM：分开配置、超时、限流；维度与模型是索引契约）
	EmbeddingDriver     string // off（默认，仅关键词检索）| openai-compat | local（确定性 mock，仅演示/测试）
	EmbeddingBaseURL    string // OpenAI 兼容 /embeddings 端点
	EmbeddingAPIKey     string
	EmbeddingModel      string
	EmbeddingDimension  int // 向量维度（索引契约；与在线索引不一致时禁用向量路降级关键词）
	EmbeddingTimeoutSec int // 单次 embedding 请求超时秒（默认 10）
	EmbeddingBatchSize  int // 文档批量 embedding 每批条数（默认 32）

	// 对话式 Agent（AGENT_CHAT_PLAN.md §10.3；默认关闭，不改变现有告警热路径）
	AssistantEnabled        bool   // AIOPS_ASSISTANTENABLED（默认 false）
	AssistantReadOnly       bool   // 本期恒只读（动作工具 Phase 3；字段先立）
	AssistantMaxConcurrency int    // 并发 run worker 数（默认 4）
	AssistantMaxModelRounds int    // 单 run 最大模型回合数（默认 8）
	AssistantMaxToolCalls   int    // 单 run 最大工具调用数（默认 12）
	AssistantRunTimeoutSec  int    // 单 run 总超时秒（默认 60）
	AssistantToolTimeoutSec int    // 单工具执行超时秒（默认 3）
	AssistantRetentionDays  int    // 会话/消息保留天数（默认 90；本期仅存配置，清理 Phase 4）
	AssistantPromptVersion  string // 系统提示词版本（默认 v1）

	// assistant_chat 模型端点 env（仅首启种子，DB 为准；与 Chat/Embedding 密钥逻辑分离，§9.3）
	AssistantChatDriver     string // openai-compat（默认）
	AssistantChatBaseURL    string // OpenAI 兼容 /chat/completions 端点（默认 ""，mock 先行）
	AssistantChatAPIKey     string
	AssistantChatModel      string
	AssistantChatTimeoutSec int // 默认 15
	AssistantChatStream     bool // 默认 true（SSE 流式）

	// 数据源证据（Prometheus 指标 / Loki 日志；往「采集→检测→RAG→LLM 归因」全链路靠拢）
	EvidenceEnabled     bool   // 数据源证据总开关（默认 false；开启后 AI 助手多 query_metrics/search_logs 工具 + RCA 自动注入日志/指标证据）
	PrometheusURL       string // Prometheus HTTP API 根地址（如 http://localhost:9090）
	PrometheusUser      string // Prometheus Basic Auth（可选，空=无认证）
	PrometheusPass      string
	LokiURL             string // Loki HTTP API 根地址（如 http://localhost:3100）
	EvidenceTimeoutSec  int    // 单次数据源查询超时秒（默认 3）
	PrometheusQueries   string // RCA 自动指标模板（分号分隔 PromQL，%s 替换 service；空=自动注入只查日志）

	// 日志
	LogLevel      string // debug|info|warn|error
	LogFile       string // 日志文件路径（默认 "" 仅 stdout；设置后 stdout+文件双写，Docker 不变）
	LogMaxMB      int    // 单文件轮转大小 MB（默认 100）
	LogMaxBackups int    // 保留轮转文件数（默认 10；100MB×10 ≈ 1GB 总上限）
	LogMaxAgeDays int    // 保留天数（默认 30）
	LogRequests   bool   // HTTP 访问日志开关（默认 true）
}

// Load 从环境变量 + 默认值加载配置（前缀 AIOPS_）。
func Load() (*Config, error) {
	v := viper.New()
	v.SetEnvPrefix("AIOPS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	v.SetDefault("addr", ":8080")
	v.SetDefault("dbdriver", "postgres") // 唯一支持：docker-compose 提供
	v.SetDefault("dbdsn", "")
	v.SetDefault("queuedriver", "memory")
	v.SetDefault("eventbusdriver", "memory")
	v.SetDefault("authtype", "noauth")
	v.SetDefault("notifierchannels", "")
	v.SetDefault("notifierdingtalk", "")
	v.SetDefault("notifierfeishu", "")
	v.SetDefault("notifierwecom", "")
	v.SetDefault("notifierhttp", "")
	v.SetDefault("notifierbaseurl", "http://localhost:5173")
	v.SetDefault("notifyenabled", true)
	v.SetDefault("notifycooldownsec", 300)
	v.SetDefault("notifysummarymin", 30)
	v.SetDefault("agentdryrun", true)
	v.SetDefault("agentenabled", true)
	v.SetDefault("rcaconfidence", 0.3)
	v.SetDefault("agentmaxdepth", 5)
	v.SetDefault("agentblastradiuslimit", 0.2)
	v.SetDefault("agentautoheallevel", "L0")
	v.SetDefault("aienabled", false)
	v.SetDefault("llmdriver", "openai-compat")
	v.SetDefault("llmbaseurl", "https://api.deepseek.com")
	v.SetDefault("llmapikey", "")
	v.SetDefault("llmmodel", "deepseek-chat")
	v.SetDefault("llmtimeoutsec", 15)
	v.SetDefault("llmstream", true)
	v.SetDefault("aiclusterenabled", true)
	v.SetDefault("aiclusterthreshold", 3)
	v.SetDefault("aiclusterwindowmin", 10)
	v.SetDefault("aiclusterintervalsec", 60)
	v.SetDefault("workflowenabled", true)
	v.SetDefault("workflowexecworkers", 8)
	v.SetDefault("workflowschedintervalsec", 10)
	v.SetDefault("loglevel", "info")
	v.SetDefault("logfile", "")
	v.SetDefault("logmaxmb", 100)
	v.SetDefault("logmaxbackups", 10)
	v.SetDefault("logmaxagedays", 30)
	v.SetDefault("logrequests", true)
	// Phase 3 存储加固
	v.SetDefault("dbmaxopenconns", 20)
	v.SetDefault("dbmaxidleconns", 20) // = MaxOpen：池内连接不因空闲回收而关闭重连（防回环 TIME_WAIT 抖动）
	v.SetDefault("dbconnmaxlifetimesec", 1800)
	v.SetDefault("dbconnmaxidletimesec", 300)
	v.SetDefault("dbconnectretries", 5)
	v.SetDefault("retentionenabled", true)
	v.SetDefault("retentionalertsdays", 30)
	v.SetDefault("retentionauditdays", 180)
	v.SetDefault("retentionworkflowrunsdays", 90)
	v.SetDefault("retentionintervalmin", 60)
	v.SetDefault("alertpartitions", true)
	v.SetDefault("httpreadtimeoutsec", 15)
	v.SetDefault("httpwritetimeoutsec", 30)
	v.SetDefault("httpidletimeoutsec", 120)
	// RAG（默认关闭；开启即校验索引契约，错误显式暴露而非静默降级）
	v.SetDefault("ragenabled", false)
	v.SetDefault("ragmode", "off")
	v.SetDefault("ragretrievaltimeoutms", 800)
	v.SetDefault("ragtopkkeyword", 40)
	v.SetDefault("ragtopkvector", 40)
	v.SetDefault("ragtopkfinal", 6)
	v.SetDefault("ragmaxcontexttokens", 5000)
	v.SetDefault("ragindexworkers", 2)
	v.SetDefault("ragindexversion", "v1")
	v.SetDefault("ragindexintervalsec", 5)
	v.SetDefault("ragindexhnsw", false)
	v.SetDefault("ragbackfillonboot", true)
	v.SetDefault("ragadminapis", false)
	v.SetDefault("ragadmintenants", "")
	v.SetDefault("modelcfgadminapis", false)
	v.SetDefault("modelcfgadmintenants", "")
	v.SetDefault("backupenabled", false)
	v.SetDefault("backupintervalhours", 24)
	v.SetDefault("backupdir", "./backups")
	v.SetDefault("backupkeep", 10)
	v.SetDefault("backupmaxgb", 5)
	v.SetDefault("backupmirrordir", "")
	v.SetDefault("backupmirrorencrypt", false)
	v.SetDefault("backuppassphrasefile", "")
	v.SetDefault("backuppgcontainer", "aiops-postgres")
	v.SetDefault("backupverify", true)
	v.SetDefault("backuptimeoutmin", 60)
	v.SetDefault("backuponboot", true)
	v.SetDefault("embeddingdriver", "off")
	v.SetDefault("embeddingbaseurl", "")
	v.SetDefault("embeddingapikey", "")
	v.SetDefault("embeddingmodel", "")
	v.SetDefault("embeddingdimension", 0)
	v.SetDefault("embeddingtimeoutsec", 10)
	v.SetDefault("embeddingbatchsize", 32)
	v.SetDefault("assistantenabled", false)
	v.SetDefault("assistantreadonly", true)
	v.SetDefault("assistantmaxconcurrency", 4)
	v.SetDefault("assistantmaxmodelrounds", 8)
	v.SetDefault("assistantmaxtoolcalls", 12)
	v.SetDefault("assistantruntimesec", 60)
	v.SetDefault("assistanttooltimesec", 3)
	v.SetDefault("assistantretentiondays", 90)
	v.SetDefault("assistantpromptversion", "v1")
	v.SetDefault("assistantchatdriver", "openai-compat")
	v.SetDefault("assistantchatbaseurl", "")
	v.SetDefault("assistantchatapikey", "")
	v.SetDefault("assistantchatmodel", "")
	v.SetDefault("assistantchattimeoutsec", 15)
	v.SetDefault("assistantchatstream", true)
	v.SetDefault("evidenceenabled", false)
	v.SetDefault("prometheusurl", "")
	v.SetDefault("prometheususer", "")
	v.SetDefault("prometheuspass", "")
	v.SetDefault("lokiurl", "")
	v.SetDefault("evidencetimeoutsec", 3)
	v.SetDefault("prometheusqueries", "")

	cfg := &Config{
		Addr:                      v.GetString("addr"),
		DBDriver:                  v.GetString("dbdriver"),
		DBDSN:                     v.GetString("dbdsn"),
		QueueDriver:               v.GetString("queuedriver"),
		EventBusDriver:            v.GetString("eventbusdriver"),
		AuthType:                  v.GetString("authtype"),
		NotifierChannels:          v.GetString("notifierchannels"),
		NotifierDingTalk:          v.GetString("notifierdingtalk"),
		NotifierFeishu:            v.GetString("notifierfeishu"),
		NotifierWecom:             v.GetString("notifierwecom"),
		NotifierHTTP:              v.GetString("notifierhttp"),
		NotifierBaseURL:           v.GetString("notifierbaseurl"),
		NotifyEnabled:             v.GetBool("notifyenabled"),
		NotifyCooldownSec:         v.GetInt("notifycooldownsec"),
		NotifySummaryMin:          v.GetInt("notifysummarymin"),
		AgentDryRun:               v.GetBool("agentdryrun"),
		AgentEnabled:              v.GetBool("agentenabled"),
		RCAConfidence:             v.GetFloat64("rcaconfidence"),
		AgentMaxDepth:             v.GetInt("agentmaxdepth"),
		AgentBlastRadiusLimit:     v.GetFloat64("agentblastradiuslimit"),
		AgentAutoHealLevel:        v.GetString("agentautoheallevel"),
		AIEnabled:                 v.GetBool("aienabled"),
		LLMDriver:                 v.GetString("llmdriver"),
		LLMBaseURL:                v.GetString("llmbaseurl"),
		LLMAPIKey:                 v.GetString("llmapikey"),
		LLMModel:                  v.GetString("llmmodel"),
		LLMTimeoutSec:             v.GetInt("llmtimeoutsec"),
		LLMStream:                 v.GetBool("llmstream"),
		AIClusterEnabled:          v.GetBool("aiclusterenabled"),
		AIClusterThreshold:        v.GetInt("aiclusterthreshold"),
		AIClusterWindowMin:        v.GetInt("aiclusterwindowmin"),
		AIClusterIntervalSec:      v.GetInt("aiclusterintervalsec"),
		WorkflowEnabled:           v.GetBool("workflowenabled"),
		WorkflowExecWorkers:       v.GetInt("workflowexecworkers"),
		WorkflowSchedIntervalSec:  v.GetInt("workflowschedintervalsec"),
		LogLevel:                  v.GetString("loglevel"),
		LogFile:                   v.GetString("logfile"),
		LogMaxMB:                  v.GetInt("logmaxmb"),
		LogMaxBackups:             v.GetInt("logmaxbackups"),
		LogMaxAgeDays:             v.GetInt("logmaxagedays"),
		LogRequests:               v.GetBool("logrequests"),
		DBMaxOpenConns:            v.GetInt("dbmaxopenconns"),
		DBMaxIdleConns:            v.GetInt("dbmaxidleconns"),
		DBConnMaxLifetimeSec:      v.GetInt("dbconnmaxlifetimesec"),
		DBConnMaxIdleTimeSec:      v.GetInt("dbconnmaxidletimesec"),
		DBConnectRetries:          v.GetInt("dbconnectretries"),
		RetentionEnabled:          v.GetBool("retentionenabled"),
		RetentionAlertsDays:       v.GetInt("retentionalertsdays"),
		RetentionAuditDays:        v.GetInt("retentionauditdays"),
		RetentionWorkflowRunsDays: v.GetInt("retentionworkflowrunsdays"),
		RetentionIntervalMin:      v.GetInt("retentionintervalmin"),
		AlertPartitions:           v.GetBool("alertpartitions"),
		HTTPReadTimeoutSec:        v.GetInt("httpreadtimeoutsec"),
		HTTPWriteTimeoutSec:       v.GetInt("httpwritetimeoutsec"),
		HTTPIdleTimeoutSec:        v.GetInt("httpidletimeoutsec"),
		RAGEnabled:                v.GetBool("ragenabled"),
		RAGMode:                   v.GetString("ragmode"),
		RAGRetrievalTimeoutMS:     v.GetInt("ragretrievaltimeoutms"),
		RAGTopKKeyword:            v.GetInt("ragtopkkeyword"),
		RAGTopKVector:             v.GetInt("ragtopkvector"),
		RAGTopKFinal:              v.GetInt("ragtopkfinal"),
		RAGMaxContextTokens:       v.GetInt("ragmaxcontexttokens"),
		RAGIndexWorkers:           v.GetInt("ragindexworkers"),
		RAGIndexVersion:           v.GetString("ragindexversion"),
		RAGIndexIntervalSec:       v.GetInt("ragindexintervalsec"),
		RAGIndexHNSW:              v.GetBool("ragindexhnsw"),
		RAGBackfillOnBoot:         v.GetBool("ragbackfillonboot"),
		RAGAdminAPIs:              v.GetBool("ragadminapis"),
		RAGAdminTenants:           v.GetString("ragadmintenants"),
		ModelCfgAdminAPIs:         v.GetBool("modelcfgadminapis"),
		ModelCfgAdminTenants:      v.GetString("modelcfgadmintenants"),
		BackupEnabled:             v.GetBool("backupenabled"),
		BackupIntervalHours:       v.GetInt("backupintervalhours"),
		BackupDir:                 v.GetString("backupdir"),
		BackupKeep:                v.GetInt("backupkeep"),
		BackupMaxGB:               v.GetInt("backupmaxgb"),
		BackupMirrorDir:           v.GetString("backupmirrordir"),
		BackupMirrorEncrypt:       v.GetBool("backupmirrorencrypt"),
		BackupPassphraseFile:      v.GetString("backuppassphrasefile"),
		BackupPgContainer:         v.GetString("backuppgcontainer"),
		BackupVerify:              v.GetBool("backupverify"),
		BackupTimeoutMin:          v.GetInt("backuptimeoutmin"),
		BackupOnBoot:              v.GetBool("backuponboot"),
		EmbeddingDriver:           v.GetString("embeddingdriver"),
		EmbeddingBaseURL:          v.GetString("embeddingbaseurl"),
		EmbeddingAPIKey:           v.GetString("embeddingapikey"),
		EmbeddingModel:            v.GetString("embeddingmodel"),
		EmbeddingDimension:        v.GetInt("embeddingdimension"),
		EmbeddingTimeoutSec:       v.GetInt("embeddingtimeoutsec"),
		EmbeddingBatchSize:        v.GetInt("embeddingbatchsize"),
		AssistantEnabled:          v.GetBool("assistantenabled"),
		AssistantReadOnly:         v.GetBool("assistantreadonly"),
		AssistantMaxConcurrency:   v.GetInt("assistantmaxconcurrency"),
		AssistantMaxModelRounds:   v.GetInt("assistantmaxmodelrounds"),
		AssistantMaxToolCalls:     v.GetInt("assistantmaxtoolcalls"),
		AssistantRunTimeoutSec:    v.GetInt("assistantruntimesec"),
		AssistantToolTimeoutSec:   v.GetInt("assistanttooltimesec"),
		AssistantRetentionDays:    v.GetInt("assistantretentiondays"),
		AssistantPromptVersion:    v.GetString("assistantpromptversion"),
		AssistantChatDriver:       v.GetString("assistantchatdriver"),
		AssistantChatBaseURL:      v.GetString("assistantchatbaseurl"),
		AssistantChatAPIKey:       v.GetString("assistantchatapikey"),
		AssistantChatModel:        v.GetString("assistantchatmodel"),
		AssistantChatTimeoutSec:   v.GetInt("assistantchattimeoutsec"),
		AssistantChatStream:       v.GetBool("assistantchatstream"),
		EvidenceEnabled:           v.GetBool("evidenceenabled"),
		PrometheusURL:             v.GetString("prometheusurl"),
		PrometheusUser:            v.GetString("prometheususer"),
		PrometheusPass:            v.GetString("prometheuspass"),
		LokiURL:                   v.GetString("lokiurl"),
		EvidenceTimeoutSec:        v.GetInt("evidencetimeoutsec"),
		PrometheusQueries:         v.GetString("prometheusqueries"),
	}

	// 对话式 Agent 配置校验（AGENT_CHAT_PLAN §10.3）：越界值回填默认。
	// 不强制模型配置——mock 先行；未配置时服务照常启动，run 快速失败 model_not_configured。
	if cfg.AssistantMaxConcurrency < 1 {
		cfg.AssistantMaxConcurrency = 4
	}
	if cfg.AssistantMaxModelRounds < 1 || cfg.AssistantMaxModelRounds > 50 {
		cfg.AssistantMaxModelRounds = 8
	}
	if cfg.AssistantMaxToolCalls < 1 || cfg.AssistantMaxToolCalls > 100 {
		cfg.AssistantMaxToolCalls = 12
	}
	if cfg.AssistantRunTimeoutSec < 1 {
		cfg.AssistantRunTimeoutSec = 60
	}
	if cfg.AssistantToolTimeoutSec < 1 {
		cfg.AssistantToolTimeoutSec = 3
	}
	if cfg.AssistantRetentionDays < 1 {
		cfg.AssistantRetentionDays = 90
	}
	if cfg.AssistantPromptVersion == "" {
		cfg.AssistantPromptVersion = "v1"
	}
	if cfg.AssistantChatTimeoutSec < 1 {
		cfg.AssistantChatTimeoutSec = 15
	}
	// 数据源证据校验：开启时必须配置至少一个数据源 URL（契约错误显式暴露）
	if cfg.EvidenceTimeoutSec < 1 {
		cfg.EvidenceTimeoutSec = 3
	}
	if cfg.EvidenceEnabled && cfg.PrometheusURL == "" && cfg.LokiURL == "" {
		return nil, fmt.Errorf("config: EVIDENCEENABLED=true 必须配置 PROMETHEUSURL 或 LOKIURL 至少一个")
	}

	// RAG 配置校验（RAG_PLAN §13）：默认关闭保持现有行为；开启时契约错误显式暴露。
	if !cfg.RAGEnabled {
		cfg.RAGMode = "off"
	} else {
		switch cfg.RAGMode {
		case "off":
			cfg.RAGEnabled = false // off 视同关闭（兼容置 RAGENABLED=true 但模式未改的配置）
		case "readonly":
			// 本期实现范围：只读相似事故 + 知识索引，不注入 RCA
		default:
			return nil, fmt.Errorf("config: RAGMODE=%q 未实现（本期支持 off|readonly，shadow/assist 后续阶段）", cfg.RAGMode)
		}
	}
	if !cfg.RAGEnabled {
		return cfg, nil
	}
	switch cfg.EmbeddingDriver {
	case "openai-compat":
		if cfg.EmbeddingBaseURL == "" || cfg.EmbeddingModel == "" {
			return nil, fmt.Errorf("config: EMBEDDINGDRIVER=openai-compat 必须配置 EMBEDDINGBASEURL 与 EMBEDDINGMODEL")
		}
		if cfg.EmbeddingAPIKey == "" {
			return nil, fmt.Errorf("config: EMBEDDINGDRIVER=openai-compat 必须配置 EMBEDDINGAPIKEY（空 key 会静默 401 降级）")
		}
	case "local":
		// 确定性 mock 向量（仅演示/测试）
	case "off":
		// 仅关键词检索（keyword-only，装配层 warn）
	default:
		return nil, fmt.Errorf("config: EMBEDDINGDRIVER=%q 未支持（off|openai-compat|local）", cfg.EmbeddingDriver)
	}
	if cfg.EmbeddingDimension <= 0 && cfg.EmbeddingDriver != "off" {
		return nil, fmt.Errorf("config: EMBEDDINGDIMENSION 必须 > 0（向量维度是索引契约，与在线索引不一致会禁用向量路）")
	}
	if cfg.EmbeddingDriver == "off" {
		slog.Warn("config: EMBEDDINGDRIVER=off，RAG 以 keyword-only 运行（无向量路，不建向量列）")
	}
	if cfg.EmbeddingBatchSize <= 0 {
		cfg.EmbeddingBatchSize = 32
	}
	// 通知节流校验（KEEPON 5.4）
	if cfg.NotifyCooldownSec < 0 {
		cfg.NotifyCooldownSec = 300
	}
	if cfg.NotifySummaryMin < 0 {
		cfg.NotifySummaryMin = 0
	}
	// 备份配置校验（KEEPON 4.1）：启用时契约错误显式暴露
	if cfg.BackupEnabled {
		if cfg.BackupKeep < 1 {
			cfg.BackupKeep = 10
		}
		if cfg.BackupMaxGB < 1 {
			cfg.BackupMaxGB = 5
		}
		if cfg.BackupIntervalHours < 1 {
			cfg.BackupIntervalHours = 24
		}
		if cfg.BackupTimeoutMin < 1 {
			cfg.BackupTimeoutMin = 60
		}
		if cfg.BackupMirrorEncrypt {
			if cfg.BackupPassphraseFile == "" {
				return nil, fmt.Errorf("config: BACKUPMIRRORENCRYPT=true 必须配置 BACKUPPASSPHRASEFILE（口令文件，权限 0600）")
			}
			if _, err := os.Stat(cfg.BackupPassphraseFile); err != nil {
				return nil, fmt.Errorf("config: 备份口令文件不可读 %s: %w", cfg.BackupPassphraseFile, err)
			}
		}
	}
	return cfg, nil
}
