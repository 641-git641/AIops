package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// executor.go：pg 工具执行抽象。本机（Windows 开发）PATH 无 pg_dump，PostgreSQL 在
// docker 容器内 → 默认 docker exec 模式；装有 pg 客户端的机器可设 =host 逃生门。
// 参数全部来自 DSN 解析器、无 shell 拼接（无注入面）；密码仅经 -e PGPASSWORD env 注入。

// PgExecutor 执行 pg 工具（pg_dump/pg_restore/createdb/dropdb/psql）。
// stdout 由调用方提供 writer（备份流式写盘 + 算哈希，不整块驻留内存）；stderr 返回收集。
type PgExecutor interface {
	Exec(ctx context.Context, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error)
}

// NewExecutor 按配置创建执行器："host" → 本机 pg 客户端；其他（默认容器名）→ docker exec。
func NewExecutor(container string, password string) PgExecutor {
	if container == "host" {
		return &hostExecutor{password: password}
	}
	return &dockerExecutor{container: container, password: password}
}

type dockerExecutor struct {
	container string
	password  string
}

// Exec docker exec -e PGPASSWORD=<pw> <container> <tool> <args...>。
// 不加 -t（避免 TTY 污染二进制流）；stdin 经 -i 传入。
func (e *dockerExecutor) Exec(ctx context.Context, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	cmdArgs := []string{"exec", "-i", "-e", "PGPASSWORD=" + e.password, e.container, tool}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "docker", cmdArgs...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Stdout = stdout
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stderr.String()), err
}

type hostExecutor struct {
	password string
}

// Exec 本机执行 <tool> <args...>（PGPASSWORD 进环境）。
func (e *hostExecutor) Exec(ctx context.Context, tool string, args []string, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	path, err := exec.LookPath(tool)
	if err != nil {
		return nil, fmt.Errorf("backup: 本机未找到 %s（安装 PostgreSQL 客户端或改用 docker exec 模式）", tool)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+e.password)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Stdout = stdout
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	return []byte(stderr.String()), err
}
