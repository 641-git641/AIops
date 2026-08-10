package backup_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"aiops/internal/backup"
	"aiops/internal/store"

	"gorm.io/gorm"
)

// fakeExecutor 模拟 pg 工具：pg_dump 输出可配置；可阻塞模拟慢备份。
type fakeExecutor struct {
	pgdumpOutput []byte
	pgdumpErr    error
	pgdumpStderr []byte
	block        chan struct{} // 非 nil 时 pg_dump 阻塞至关闭（模拟运行中）
	entered      chan struct{} // 进入 pg_dump 时关闭（测试同步点）
	calls        []string
}

func (f *fakeExecutor) Exec(_ context.Context, tool string, _ []string, _ io.Reader, stdout io.Writer) ([]byte, error) {
	f.calls = append(f.calls, tool)
	switch tool {
	case "pg_dump":
		if f.entered != nil {
			close(f.entered)
			f.entered = nil
		}
		if f.block != nil {
			<-f.block
		}
		if f.pgdumpErr != nil {
			return f.pgdumpStderr, f.pgdumpErr
		}
		_, _ = stdout.Write(f.pgdumpOutput)
		return nil, nil
	case "createdb", "dropdb", "pg_restore":
		return nil, nil
	case "psql":
		_, _ = stdout.Write([]byte("3|5|2|1|1"))
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected tool %s", tool)
}

func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(store.DefaultDSN, store.Options{})
	if err != nil {
		t.Skipf("postgres 未运行（docker compose），跳过: %v", err)
	}
	if err := db.AutoMigrate(&backup.BackupRun{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = db.Exec("DELETE FROM backup_runs").Error })
	return db
}

func testConfig(dir string) backup.Config {
	return backup.Config{
		DSN:      store.DefaultDSN,
		Dir:      dir,
		Keep:     3,
		MaxBytes: 1 << 30,
		Timeout:  30 * time.Second,
		Interval: time.Hour,
		Verify:   false,
	}
}

func TestRunOnceSuccess(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	fake := &fakeExecutor{pgdumpOutput: []byte("PGDUMP-DATA-123")}
	svc := backup.New(db, testConfig(dir), fake)

	run, err := svc.RunManual(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "success" || run.FileSize != int64(len("PGDUMP-DATA-123")) {
		t.Fatalf("run wrong: %+v", run)
	}
	if run.SHA256 == "" || run.VerifyStatus != "skipped" {
		t.Fatalf("run fields wrong: %+v", run)
	}
	// 文件存在且内容一致
	b, err := os.ReadFile(run.FilePath)
	if err != nil || string(b) != "PGDUMP-DATA-123" {
		t.Fatalf("dump file wrong: %v, %q", err, b)
	}
	// List/LastRun
	runs, total, err := svc.List(context.Background(), 10, 0)
	if err != nil || total != 1 || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("list wrong: %d %d %v", len(runs), total, err)
	}
	last, _ := svc.LastRun(context.Background())
	if last == nil || last.ID != run.ID {
		t.Fatal("last run wrong")
	}
	if svc.BackupState() != 1 {
		t.Fatal("state should be 1 after success")
	}
}

func TestRunOnceFailure(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	fake := &fakeExecutor{pgdumpErr: errors.New("pg_dump: connection refused"), pgdumpStderr: []byte("fatal: could not connect")}
	svc := backup.New(db, testConfig(dir), fake)

	run, err := svc.RunManual(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if run.Status != "failed" || run.ErrorDetail == "" {
		t.Fatalf("run wrong: %+v", run)
	}
	// 失败文件被清理
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("failed dump left behind: %v", entries)
	}
	if svc.BackupState() != 0 {
		t.Fatal("state must be 0 after failure")
	}
}

func TestRunIfDueSkipsWithinInterval(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	fake := &fakeExecutor{pgdumpOutput: []byte("x")}
	svc := backup.New(db, testConfig(dir), fake)

	if err := svc.RunIfDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	n := len(fake.calls)
	if err := svc.RunIfDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != n {
		t.Fatal("RunIfDue must skip within interval")
	}
	// 间隔 0（立即过期）→ 补跑
	cfg := testConfig(dir)
	cfg.Interval = 0
	svc2 := backup.New(db, cfg, fake)
	if err := svc2.RunIfDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) <= n {
		t.Fatal("RunIfDue must re-run after interval")
	}
}

func TestRunManualWhileRunning(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	block := make(chan struct{})
	fake := &fakeExecutor{pgdumpOutput: []byte("x"), block: block, entered: make(chan struct{})}
	svc := backup.New(db, testConfig(dir), fake)

	done := make(chan error, 1)
	go func() { done <- svc.RunOnce(context.Background()) }()
	<-fake.entered // 确保已进入 pg_dump（持锁中）

	if _, err := svc.RunManual(context.Background()); !errors.Is(err, backup.ErrRunning) {
		t.Fatalf("second run = %v, want ErrRunning", err)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMirrorAndVerify(t *testing.T) {
	db := openDB(t)
	dir := t.TempDir()
	mirror := t.TempDir()
	fake := &fakeExecutor{pgdumpOutput: []byte("DATA")}
	cfg := testConfig(dir)
	cfg.MirrorDir = mirror
	cfg.Verify = true // psql 计数 3|5|2|1|1
	svc := backup.New(db, cfg, fake)

	run, err := svc.RunManual(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.MirrorPath == "" || run.VerifyStatus != "passed" {
		t.Fatalf("run wrong: %+v", run)
	}
	if _, err := os.Stat(run.MirrorPath); err != nil {
		t.Fatalf("mirror missing: %v", err)
	}
	if run.VerifyDetail == "" {
		t.Fatal("verify detail empty")
	}
}
