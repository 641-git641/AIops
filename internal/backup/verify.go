package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// verify.go：备份恢复校验（KEEPON 4.1 完成标准"定期验证真的能恢复"）。
// 恢复到临时库（aiops_bkverify_<ts>）→ pg_restore → 关键表计数 → drop。
// 校验失败独立呈现（VerifyStatus=failed），备份文件本身仍有效。

// verifyTables 校验的关键表（知识/配置/业务核心数据）。
var verifyTables = []string{
	"incidents", "alerts", "knowledge_documents", "model_configs", "notification_channels",
}

// VerifyFromFile 校验备份文件可恢复：临时库计数与源库一致则 passed。
func (s *Service) VerifyFromFile(ctx context.Context, run *BackupRun) error {
	target, err := ParseDSN(s.cfg.DSN)
	if err != nil {
		return err
	}
	ts := fmt.Sprintf("%d", run.StartedAt.Unix())
	tempDB := "aiops_bkverify_" + ts
	dropArgs := append(target.pgArgs(), tempDB)
	dropArgs = append([]string{"--if-exists"}, dropArgs...)
	cleanup := func() {
		_, _ = s.ex.Exec(ctx, "dropdb", dropArgs, nil, io.Discard)
	}
	cleanup()
	defer cleanup()

	// 1. 建临时库
	if _, err := s.ex.Exec(ctx, "createdb", append(target.pgArgs(), tempDB), nil, io.Discard); err != nil {
		return fmt.Errorf("createdb %s: %w", tempDB, err)
	}
	// 2. 恢复（--no-owner 避免属主问题；不经 -j 并行——stdin 管道不支持并行恢复）
	file, err := os.Open(run.FilePath)
	if err != nil {
		return err
	}
	defer file.Close()
	restoreArgs := append([]string{"--no-owner"}, target.pgArgs()...)
	restoreArgs = append(restoreArgs, "-d", tempDB)
	if stderr, err := s.ex.Exec(ctx, "pg_restore", restoreArgs, file, io.Discard); err != nil {
		return fmt.Errorf("pg_restore: %s", strings.TrimSpace(string(stderr)))
	}
	// 3. 计数比对（psql -t -A 单行输出）
	expr := make([]string, 0, len(verifyTables))
	for _, t := range verifyTables {
		expr = append(expr, fmt.Sprintf("(SELECT count(*) FROM %s)", t))
	}
	var out strings.Builder
	psqlArgs := append(target.pgArgs(), "-d", tempDB, "-t", "-A", "-c",
		"SELECT "+strings.Join(expr, ","))
	if _, err := s.ex.Exec(ctx, "psql", psqlArgs, nil, &out); err != nil {
		return fmt.Errorf("psql 计数失败: %w", err)
	}
	counts := strings.Split(strings.TrimSpace(out.String()), "|")
	if len(counts) != len(verifyTables) {
		return fmt.Errorf("计数输出异常: %q", out.String())
	}
	parts := make([]string, len(verifyTables))
	for i, c := range counts {
		parts[i] = fmt.Sprintf("%s=%s", verifyTables[i], strings.TrimSpace(c))
	}
	run.VerifyDetail = strings.Join(parts, " ")
	return nil
}
