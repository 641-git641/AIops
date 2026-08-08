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

// ---------- buildSaveAlertsSQL（批量写，Phase 3g） ----------

func TestBuildSaveAlertsSQLPlaceholdersMatchArgs(t *testing.T) {
	a1, a2 := sampleAlert(), sampleAlert()
	a2.ID, a2.AlertHash = "a-2", "h-2"
	sql, args := buildSaveAlertsSQL([]*Alert{a1, a2}, []DedupKind{DedupNew, DedupUpdate})
	if got, want := strings.Count(sql, "?"), len(args); got != want {
		t.Fatalf("placeholder count %d != args %d\n---\n%s", got, want, sql)
	}
	if strings.Count(sql, "INSERT INTO") != 2 {
		t.Errorf("want 2 INSERT INTO (alerts + last_alerts), got SQL:\n%s", sql)
	}
	for _, anchor := range []string{
		"?::jsonb",                             // JSON 显式 cast
		"ON CONFLICT (tenant_id, fingerprint)", // last_alerts 冲突仲裁
		"RETURNING id",                         // CTE 数据修改子句
	} {
		if !strings.Contains(sql, anchor) {
			t.Errorf("SQL missing anchor %q\n---\n%s", anchor, sql)
		}
	}
}

func TestBuildSaveAlertsSQLJSONAsString(t *testing.T) {
	a1, a2 := sampleAlert(), sampleAlert()
	a2.ID = "a-2"
	_, args := buildSaveAlertsSQL([]*Alert{a1, a2}, []DedupKind{DedupNew, DedupUpdate})
	// 每行 alerts 15 参数：event=args[6]、labels=args[10]；第二条行偏移 15
	for _, v := range []int{6, 10, 21, 25} {
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

func TestBuildSaveAlertsSQLZeroTimeFilled(t *testing.T) {
	a1, a2 := sampleAlert(), sampleAlert()
	a1.CreatedAt, a1.UpdatedAt = time.Time{}, time.Time{}
	a2.ID = "a-2"
	a2.CreatedAt, a2.UpdatedAt = time.Time{}, time.Time{}
	buildSaveAlertsSQL([]*Alert{a1, a2}, []DedupKind{DedupNew, DedupUpdate})
	if a1.CreatedAt.IsZero() || a1.UpdatedAt.IsZero() {
		t.Error("a1 times not filled")
	}
	if a2.CreatedAt.IsZero() || a2.UpdatedAt.IsZero() {
		t.Error("a2 times not filled")
	}
}

func TestBuildSaveAlertsSQLCollapseLastAlert(t *testing.T) {
	// 同批同 fingerprint：alerts 历史全量 2 行，last_alerts 折叠 1 行（批内最后一条）
	a1, a2 := sampleAlert(), sampleAlert() // 同 fp f-1
	a2.ID, a2.AlertHash = "a-2", "h-2"
	_, args := buildSaveAlertsSQL([]*Alert{a1, a2}, []DedupKind{DedupNew, DedupUpdate})
	if got, want := len(args), 15*2+9; got != want {
		t.Fatalf("args = %d, want %d (2 alerts × 15 + 1 collapsed × 9)", got, want)
	}
	// 折叠行参数（alerts 2×15 之后）：id / tenant / fp / alert_id / hash / status ...
	if got := args[15*2+3]; got != "a-2" {
		t.Errorf("collapsed alert_id = %v, want a-2（批内最后一条）", got)
	}
	if got := args[15*2+4]; got != "h-2" {
		t.Errorf("collapsed alert_hash = %v, want h-2", got)
	}
}

func TestBuildSaveAlertsSQLFiltersDuplicates(t *testing.T) {
	a1, a2 := sampleAlert(), sampleAlert()
	a2.ID = "a-2"
	sql, args := buildSaveAlertsSQL([]*Alert{a1, a2}, []DedupKind{DedupNew, DedupDuplicate})
	if got, want := len(args), 15+9; got != want {
		t.Fatalf("args = %d, want %d（duplicate 被过滤：1 行 alerts + 1 行折叠 last_alerts）", got, want)
	}
	if got := strings.Count(sql, "?::jsonb"); got != 2 {
		t.Errorf("?::jsonb 出现 %d 次, want 2（仅 1 行 alerts）\n---\n%s", got, sql)
	}
}

func TestBuildSaveAlertsSQLAllDuplicatesEmpty(t *testing.T) {
	a := sampleAlert()
	sql, args := buildSaveAlertsSQL([]*Alert{a}, []DedupKind{DedupDuplicate})
	if sql != "" || args != nil {
		t.Fatalf("全部 duplicate 应返回空，got sql=%q args=%v", sql, args)
	}
}
