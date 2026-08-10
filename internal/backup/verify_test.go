package backup_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"aiops/internal/backup"
	"aiops/internal/store"
)

// TestVerifyFromFileReal 真实恢复演练：docker exec pg_dump 真备份 → VerifyFromFile
// 恢复临时库 → 断言计数（验证 psql/createdb/pg_restore 全链路）。
// 双重 gate：PostgreSQL 不可用或 docker CLI 不可用均跳过。
func TestVerifyFromFileReal(t *testing.T) {
	if err := exec.Command("docker", "ps").Run(); err != nil {
		t.Skipf("docker CLI 不可用，跳过真实恢复演练: %v", err)
	}
	db := openDB(t)
	dir := t.TempDir()
	cfg := backup.Config{
		DSN: store.DefaultDSN, Dir: dir, Keep: 3, MaxBytes: 1 << 30,
		Timeout: 60 * time.Second, Interval: time.Hour, Verify: true,
	}
	svc := backup.New(db, cfg, backup.NewExecutor("aiops-postgres", "aiops"))

	run, err := svc.RunManual(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "success" {
		t.Fatalf("run = %s, detail: %s", run.Status, run.ErrorDetail)
	}
	if run.VerifyStatus != "passed" {
		t.Fatalf("verify = %s: %s", run.VerifyStatus, run.VerifyDetail)
	}
	// 校验详情应包含 5 张关键表计数（如 incidents=N alerts=M ...）
	for _, tbl := range []string{"incidents", "alerts", "knowledge_documents", "model_configs", "notification_channels"} {
		if !strings.Contains(run.VerifyDetail, tbl) {
			t.Fatalf("verify detail missing %s: %q", tbl, run.VerifyDetail)
		}
	}
	// 临时库已清理
	out, err := exec.Command("docker", "exec", "aiops-postgres", "psql", "-U", "aiops", "-d", "aiops",
		"-t", "-A", "-c", "SELECT count(*) FROM pg_database WHERE datname LIKE 'aiops_bkverify_%'").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "0" {
		t.Fatalf("temp verify databases left behind: %q", out)
	}
	_ = os.Remove(run.FilePath) // 清理测试产物
}
