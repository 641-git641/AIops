// Package config 负责加载应用配置（env + yaml + 默认值）。
package config

import (
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

	// 工作流（keep 式 YAML 工作流引擎）
	WorkflowEnabled          bool // 是否投递 workflow.trigger 任务并消费（默认 true）
	WorkflowExecWorkers      int  // 工作流执行并发上限（慢步骤不阻塞队列 worker，默认 8）
	WorkflowSchedIntervalSec int  // interval 触发器扫描间隔秒（默认 10）

	// 日志
	LogLevel string // debug|info|warn|error
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

	return &Config{
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
	}, nil
}
