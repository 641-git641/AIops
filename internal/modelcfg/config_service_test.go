package modelcfg_test

import (
	"aiops/internal/modelcfg"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aiops/internal/store"

	"gorm.io/gorm"
)

// ---- 生效值纯函数 ----

func TestEffectiveChat(t *testing.T) {
	env := modelcfg.ChatEnv{Driver: "openai-compat", BaseURL: "https://env.example", APIKey: "env-key",
		Model: "env-model", TimeoutSec: 30, Stream: true}
	// DB 行优先
	row := &modelcfg.ModelConfig{Enabled: true, BaseURL: "https://db.example", APIKey: "db-key",
		Model: "db-model", TimeoutSec: 60, Stream: false}
	got := modelcfg.EffectiveChat(row, true, env)
	if got.Source != "db" || got.BaseURL != "https://db.example" || got.APIKey != "db-key" || got.Stream {
		t.Fatalf("db row not preferred: %+v", got)
	}
	// 无行回退 env
	got = modelcfg.EffectiveChat(nil, false, env)
	if got.Source != "env" || !got.Enabled || got.APIKey != "env-key" || !got.Stream {
		t.Fatalf("env fallback wrong: %+v", got)
	}
	// disabled 行 → 停用=静默（不回退 env）
	got = modelcfg.EffectiveChat(&modelcfg.ModelConfig{Enabled: false}, true, env)
	if got.Enabled {
		t.Fatalf("disabled row must disable: %+v", got)
	}
	// env 无 key → Enabled=false
	got = modelcfg.EffectiveChat(nil, false, modelcfg.ChatEnv{Driver: "openai-compat"})
	if got.Enabled {
		t.Fatal("env without key must disable")
	}
}

func TestEffectiveEmbedding(t *testing.T) {
	// 仅 env 来源（embedding 不走 DB）：透传 driver，全字段透传
	env := modelcfg.EmbeddingEnv{Driver: "local", Dimension: 64, BaseURL: "u", APIKey: "k",
		Model: "m", TimeoutSec: 10, BatchSize: 16}
	got := modelcfg.EffectiveEmbedding(env)
	if got.Driver != "local" || got.Dimension != 64 || got.Enabled ||
		got.BaseURL != "u" || got.Model != "m" || got.BatchSize != 16 {
		t.Fatalf("local passthrough wrong: %+v", got)
	}
	// openai-compat + key → Enabled（与 chat 门控对齐）
	got = modelcfg.EffectiveEmbedding(modelcfg.EmbeddingEnv{Driver: "openai-compat", APIKey: "k", Dimension: 1024})
	if got.Driver != "openai-compat" || !got.Enabled || got.Dimension != 1024 {
		t.Fatalf("openai-compat enabled wrong: %+v", got)
	}
	// openai-compat 空 key → 不启用（防静默 401 降级）
	got = modelcfg.EffectiveEmbedding(modelcfg.EmbeddingEnv{Driver: "openai-compat", Dimension: 1024})
	if got.Enabled {
		t.Fatalf("openai-compat without key must not enable: %+v", got)
	}
	// off
	got = modelcfg.EffectiveEmbedding(modelcfg.EmbeddingEnv{Driver: "off"})
	if got.Driver != "off" || got.Enabled {
		t.Fatalf("env off wrong: %+v", got)
	}
}

// ---- DTO 脱敏 ----

func TestToDTOMasksAPIKey(t *testing.T) {
	m := &modelcfg.ModelConfig{Key: "chat", Kind: "chat", Enabled: true, BaseURL: "u", APIKey: "sk-super-secret", Model: "m"}
	raw, err := json.Marshal(modelcfg.ToDTO(m))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, "sk-super-secret") || strings.Contains(s, "apiKey\":\"") && strings.Contains(s, "secret") {
		t.Fatalf("apiKey leaked in DTO: %s", s)
	}
	if !strings.Contains(s, `"apiKeySet":true`) {
		t.Fatalf("apiKeySet missing: %s", s)
	}
	// 未配置 → apiKeySet=false
	raw2, _ := json.Marshal(modelcfg.ToDTO(&modelcfg.ModelConfig{Key: "chat"}))
	if !strings.Contains(string(raw2), `"apiKeySet":false`) {
		t.Fatalf("apiKeySet=false missing: %s", raw2)
	}
}

// ---- 保存校验 ----

func TestValidateSave(t *testing.T) {
	for _, key := range []string{modelcfg.KeyChat, modelcfg.KeyAssistantChat} {
		ok := modelcfg.SaveInput{Enabled: true, BaseURL: "u", Model: "m", TimeoutSec: 0}
		if err := modelcfg.ValidateSave(key, &ok); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if ok.TimeoutSec != 15 {
			t.Fatalf("%s timeout default = %d, want 15", key, ok.TimeoutSec)
		}
	}
	cases := []struct {
		key string
		in  modelcfg.SaveInput
	}{
		{"badkey", modelcfg.SaveInput{Enabled: true, BaseURL: "u", Model: "m"}},
		{modelcfg.KeyChat, modelcfg.SaveInput{Enabled: true, BaseURL: "", Model: "m"}},
		{modelcfg.KeyChat, modelcfg.SaveInput{Enabled: true, BaseURL: "u", Model: ""}},
		{modelcfg.KeyAssistantChat, modelcfg.SaveInput{Enabled: true, BaseURL: "u", Model: ""}},
	}
	for i, c := range cases {
		if err := modelcfg.ValidateSave(c.key, &c.in); err == nil {
			t.Fatalf("case %d should fail: %+v", i, c.in)
		}
	}
	// 停用不校验字段
	for _, key := range []string{modelcfg.KeyChat, modelcfg.KeyAssistantChat} {
		if err := modelcfg.ValidateSave(key, &modelcfg.SaveInput{Enabled: false}); err != nil {
			t.Fatalf("%s disabled must pass: %v", key, err)
		}
	}
}

// ---- 连通性测试（DB 门控 + httptest；PostgreSQL 未运行自动跳过）----

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(store.DefaultDSN, store.Options{})
	if err != nil {
		t.Skipf("postgres 未运行（docker compose），跳过集成断言: %v", err)
	}
	if err := db.AutoMigrate(&modelcfg.ModelConfig{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestTestConnectivity(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	cleanup := func() { _ = db.Exec("DELETE FROM model_configs").Error }
	cleanup()
	defer cleanup()

	// mock chat/completions
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "bad path "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer srv.Close()

	c := modelcfg.NewConfigService(db)
	// chat 连通性（经 Save 落库后 Test 用 DB 兜底）
	if _, err := c.Save(ctx, modelcfg.KeyChat, modelcfg.SaveInput{Enabled: true, BaseURL: srv.URL, APIKey: "test-key", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	res, _ := c.Test(ctx, modelcfg.KeyChat, modelcfg.SaveInput{})
	if !res.OK {
		t.Fatalf("chat test failed: %+v", res)
	}
	// 坏 key → 500 → OK=false
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer bad.Close()
	res, _ = c.Test(ctx, modelcfg.KeyChat, modelcfg.SaveInput{BaseURL: bad.URL, Model: "m", APIKey: "k"})
	if res.OK {
		t.Fatalf("bad endpoint must fail: %+v", res)
	}
	// apiKey 空保存 → 保留旧值
	dto, err := c.Save(ctx, modelcfg.KeyChat, modelcfg.SaveInput{Enabled: true, BaseURL: srv.URL, Model: "m2"})
	if err != nil {
		t.Fatal(err)
	}
	if !dto.APIKeySet {
		t.Fatal("apiKey must be preserved on empty save")
	}
	// List 脱敏：chat + assistant_chat 两行固定、无 apiKey 字段
	items, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Key != "chat" || items[1].Key != "assistant_chat" {
		t.Fatalf("list = %+v, want 2 rows [chat, assistant_chat]", items)
	}
	// assistant_chat 缺行补零值（未配置时不 seed）
	if items[1].APIKeySet {
		t.Fatalf("assistant_chat 未配置却 apiKeySet=true: %+v", items[1])
	}
	raw, _ := json.Marshal(items)
	if strings.Contains(string(raw), "test-key") {
		t.Fatal("apiKey leaked in List response")
	}
}
