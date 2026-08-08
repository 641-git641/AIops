package alert

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// sampleAlert 构造测试用告警（含 JSON 字段与显式时间，验证参数与占位符对齐）。
func sampleAlert() *Alert {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	return &Alert{
		ID:           "a-1",
		TenantID:     "t1",
		Provider:     "prometheus",
		ProviderType: "prom",
		Fingerprint:  "f-1",
		AlertHash:    "h-1",
		Event:        mustJSON(map[string]any{"name": "cpu"}),
		Name:         "cpu-high",
		Severity:     SeverityHigh,
		Status:       StatusFiring,
		Labels:       mustJSON(map[string]any{"severity": "high"}),
		StartedAt:    now,
		LastSeenAt:   now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestBuildSaveSQLPlaceholdersMatchArgs(t *testing.T) {
	a := sampleAlert()
	sql, args := buildSaveSQL(a)
	if got, want := strings.Count(sql, "?"), len(args); got != want {
		t.Fatalf("placeholder count %d != args %d\n---\n%s", got, want, sql)
	}
}

func TestBuildSaveSQLJSONAsString(t *testing.T) {
	_, args := buildSaveSQL(sampleAlert())
	// args[6]=event、args[10]=labels：必须绑定为 string（text→jsonb cast），而非 []byte
	for _, v := range []int{6, 10} {
		s, ok := args[v].(string)
		if !ok {
			t.Fatalf("args[%d] is %T, want string", v, args[v])
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("args[%d] not valid JSON: %v", v, err)
		}
	}
}

func TestBuildSaveSQLZeroTimeFilled(t *testing.T) {
	a := sampleAlert()
	a.CreatedAt = time.Time{}
	a.UpdatedAt = time.Time{}
	buildSaveSQL(a)
	if a.CreatedAt.IsZero() {
		t.Error("CreatedAt not filled")
	}
	if a.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not filled")
	}
	// 显式时间不被覆盖
	explicit := sampleAlert()
	buildSaveSQL(explicit)
	if explicit.CreatedAt != sampleAlert().CreatedAt {
		t.Error("explicit CreatedAt overwritten")
	}
}

func TestBuildSaveSQLAnchors(t *testing.T) {
	sql, _ := buildSaveSQL(sampleAlert())
	for _, anchor := range []string{
		"?::jsonb",                             // JSON 显式 cast
		"ON CONFLICT (tenant_id, fingerprint)", // last_alerts 冲突仲裁
		"RETURNING id",                         // CTE 数据修改子句
		"RETURNING id, tenant_id, fingerprint, alert_hash, status, last_seen_at", // CTE 返回冗余列（dedup 单读依赖）
		"alert_hash = EXCLUDED.alert_hash",                                       // 冲突更新同步 hash
	} {
		if !strings.Contains(sql, anchor) {
			t.Errorf("SQL missing anchor %q\n---\n%s", anchor, sql)
		}
	}
	// 双表写入：alerts insert + last_alerts insert 同一条语句
	if strings.Count(sql, "INSERT INTO") != 2 {
		t.Errorf("want 2 INSERT INTO (alerts + last_alerts), got SQL:\n%s", sql)
	}
}
