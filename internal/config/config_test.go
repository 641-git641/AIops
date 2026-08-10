package config

import (
	"testing"
)

func TestLoadLLMDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if cfg.LLMDriver != "openai-compat" {
		t.Errorf("LLMDriver = %q, want openai-compat", cfg.LLMDriver)
	}
	if cfg.LLMBaseURL != "https://api.deepseek.com" {
		t.Errorf("LLMBaseURL = %q, want deepseek default", cfg.LLMBaseURL)
	}
	if cfg.LLMModel != "deepseek-chat" {
		t.Errorf("LLMModel = %q, want deepseek-chat", cfg.LLMModel)
	}
	if cfg.LLMAPIKey != "" {
		t.Errorf("LLMAPIKey = %q, want empty（未配置时 fail-open）", cfg.LLMAPIKey)
	}
	if cfg.LLMTimeoutSec != 15 {
		t.Errorf("LLMTimeoutSec = %d, want 15（默认超时可控）", cfg.LLMTimeoutSec)
	}
	if !cfg.LLMStream {
		t.Error("LLMStream = false, want true（默认 SSE 流式）")
	}
}

func TestLoadLLMEnv(t *testing.T) {
	t.Setenv("AIOPS_LLMAPIKEY", "sk-test")
	t.Setenv("AIOPS_LLMBASEURL", "http://localhost:9998")
	t.Setenv("AIOPS_LLMMODEL", "mock-chat")
	t.Setenv("AIOPS_LLMDRIVER", "openai-compat")
	t.Setenv("AIOPS_LLMTIMEOUTSEC", "45")
	t.Setenv("AIOPS_LLMSTREAM", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if cfg.LLMAPIKey != "sk-test" {
		t.Errorf("LLMAPIKey = %q, want sk-test（AIOPS_LLMAPIKEY 应生效）", cfg.LLMAPIKey)
	}
	if cfg.LLMBaseURL != "http://localhost:9998" {
		t.Errorf("LLMBaseURL = %q", cfg.LLMBaseURL)
	}
	if cfg.LLMModel != "mock-chat" {
		t.Errorf("LLMModel = %q", cfg.LLMModel)
	}
	if cfg.LLMTimeoutSec != 45 {
		t.Errorf("LLMTimeoutSec = %d, want 45（AIOPS_LLMTIMEOUTSEC 应生效）", cfg.LLMTimeoutSec)
	}
	if cfg.LLMStream {
		t.Error("LLMStream = true, want false（AIOPS_LLMSTREAM=false 应生效）")
	}
}

func TestLoadStorageHardeningDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if cfg.DBMaxOpenConns != 20 {
		t.Errorf("DBMaxOpenConns = %d, want 20", cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != 20 {
		t.Errorf("DBMaxIdleConns = %d, want 20（= MaxOpen 防连接抖动）", cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLifetimeSec != 1800 {
		t.Errorf("DBConnMaxLifetimeSec = %d, want 1800", cfg.DBConnMaxLifetimeSec)
	}
	if cfg.DBConnectRetries != 5 {
		t.Errorf("DBConnectRetries = %d, want 5", cfg.DBConnectRetries)
	}
	if !cfg.RetentionEnabled {
		t.Error("RetentionEnabled = false, want true（保留策略 worker 默认开启）")
	}
	if cfg.RetentionAlertsDays != 30 {
		t.Errorf("RetentionAlertsDays = %d, want 30", cfg.RetentionAlertsDays)
	}
	if cfg.RetentionAuditDays != 180 {
		t.Errorf("RetentionAuditDays = %d, want 180", cfg.RetentionAuditDays)
	}
	if cfg.RetentionWorkflowRunsDays != 90 {
		t.Errorf("RetentionWorkflowRunsDays = %d, want 90", cfg.RetentionWorkflowRunsDays)
	}
	if cfg.RetentionIntervalMin != 60 {
		t.Errorf("RetentionIntervalMin = %d, want 60", cfg.RetentionIntervalMin)
	}
}

func TestLoadStorageHardeningEnv(t *testing.T) {
	t.Setenv("AIOPS_DBMAXOPENCONNS", "50")
	t.Setenv("AIOPS_DBCONNECTRETRIES", "10")
	t.Setenv("AIOPS_RETENTIONENABLED", "false")
	t.Setenv("AIOPS_RETENTIONALERTSDAYS", "7")
	t.Setenv("AIOPS_RETENTIONINTERVALMIN", "5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if cfg.DBMaxOpenConns != 50 {
		t.Errorf("DBMaxOpenConns = %d, want 50（AIOPS_DBMAXOPENCONNS 应生效）", cfg.DBMaxOpenConns)
	}
	if cfg.DBConnectRetries != 10 {
		t.Errorf("DBConnectRetries = %d, want 10", cfg.DBConnectRetries)
	}
	if cfg.RetentionEnabled {
		t.Error("RetentionEnabled = true, want false（AIOPS_RETENTIONENABLED=false 应生效）")
	}
	if cfg.RetentionAlertsDays != 7 {
		t.Errorf("RetentionAlertsDays = %d, want 7", cfg.RetentionAlertsDays)
	}
	if cfg.RetentionIntervalMin != 5 {
		t.Errorf("RetentionIntervalMin = %d, want 5", cfg.RetentionIntervalMin)
	}
}

func TestLoadAssistantDefaultsAndEnv(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if cfg.AssistantEnabled {
		t.Error("AssistantEnabled = true, want false（默认关闭）")
	}
	if !cfg.AssistantReadOnly {
		t.Error("AssistantReadOnly = false, want true")
	}
	if cfg.AssistantMaxConcurrency != 4 || cfg.AssistantMaxModelRounds != 8 ||
		cfg.AssistantMaxToolCalls != 12 || cfg.AssistantRunTimeoutSec != 60 ||
		cfg.AssistantToolTimeoutSec != 3 || cfg.AssistantRetentionDays != 90 {
		t.Errorf("assistant limits = %d/%d/%d/%d/%d/%d，want 4/8/12/60/3/90",
			cfg.AssistantMaxConcurrency, cfg.AssistantMaxModelRounds, cfg.AssistantMaxToolCalls,
			cfg.AssistantRunTimeoutSec, cfg.AssistantToolTimeoutSec, cfg.AssistantRetentionDays)
	}
	if cfg.AssistantPromptVersion != "v1" {
		t.Errorf("AssistantPromptVersion = %q, want v1", cfg.AssistantPromptVersion)
	}
	if cfg.AssistantChatDriver != "openai-compat" || cfg.AssistantChatTimeoutSec != 15 || !cfg.AssistantChatStream {
		t.Errorf("assistant chat defaults = %q/%d/%v", cfg.AssistantChatDriver, cfg.AssistantChatTimeoutSec, cfg.AssistantChatStream)
	}

	t.Setenv("AIOPS_ASSISTANTENABLED", "true")
	t.Setenv("AIOPS_ASSISTANTCHATBASEURL", "http://localhost:9998")
	t.Setenv("AIOPS_ASSISTANTCHATAPIKEY", "sk-test")
	t.Setenv("AIOPS_ASSISTANTCHATMODEL", "mock-chat")
	t.Setenv("AIOPS_ASSISTANTMAXMODELROUNDS", "20")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if !cfg.AssistantEnabled || cfg.AssistantChatBaseURL != "http://localhost:9998" ||
		cfg.AssistantChatAPIKey != "sk-test" || cfg.AssistantChatModel != "mock-chat" {
		t.Errorf("assistant env 未生效: %+v", cfg.AssistantChatBaseURL)
	}
	if cfg.AssistantMaxModelRounds != 20 {
		t.Errorf("AssistantMaxModelRounds = %d, want 20", cfg.AssistantMaxModelRounds)
	}
	// 越界回填
	t.Setenv("AIOPS_ASSISTANTMAXTOOLCALLS", "9999")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if cfg.AssistantMaxToolCalls != 12 {
		t.Errorf("AssistantMaxToolCalls 越界未回填 = %d, want 12", cfg.AssistantMaxToolCalls)
	}
}

func TestLoadEvidenceDefaultsAndEnv(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if cfg.EvidenceEnabled {
		t.Error("EvidenceEnabled = true, want false（默认关闭）")
	}
	if cfg.EvidenceTimeoutSec != 3 {
		t.Errorf("EvidenceTimeoutSec = %d, want 3", cfg.EvidenceTimeoutSec)
	}
	if cfg.PrometheusURL != "" || cfg.LokiURL != "" {
		t.Error("数据源 URL 默认应为空")
	}

	t.Setenv("AIOPS_EVIDENCEENABLED", "true")
	t.Setenv("AIOPS_PROMETHEUSURL", "http://localhost:9090")
	t.Setenv("AIOPS_LOKIURL", "http://localhost:3100")
	t.Setenv("AIOPS_PROMETHEUSQUERIES", "rate(x[5m]); container_cpu_usage_seconds_total{service=\"%s\"}")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load error = %v", err)
	}
	if !cfg.EvidenceEnabled || cfg.PrometheusURL != "http://localhost:9090" ||
		cfg.LokiURL != "http://localhost:3100" {
		t.Errorf("evidence env 未生效: %+v", cfg.EvidenceEnabled)
	}
	if cfg.PrometheusQueries == "" {
		t.Error("PrometheusQueries 未生效")
	}
	// 契约错误：enabled 但无任何 URL → 报错
	t.Setenv("AIOPS_PROMETHEUSURL", "")
	t.Setenv("AIOPS_LOKIURL", "")
	if _, err := Load(); err == nil {
		t.Fatal("EvidenceEnabled 无 URL 应报错")
	}
}

func TestLoadModelCfgAdminDefaultsAndEnv(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelCfgAdminAPIs || cfg.ModelCfgAdminTenants != "" {
		t.Fatalf("modelcfg admin defaults: %v %q", cfg.ModelCfgAdminAPIs, cfg.ModelCfgAdminTenants)
	}
	t.Setenv("AIOPS_MODELCFGADMINAPIS", "true")
	t.Setenv("AIOPS_MODELCFGADMINTENANTS", "ops,dev")
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg2.ModelCfgAdminAPIs || cfg2.ModelCfgAdminTenants != "ops,dev" {
		t.Fatalf("modelcfg admin env: %v %q", cfg2.ModelCfgAdminAPIs, cfg2.ModelCfgAdminTenants)
	}
}
