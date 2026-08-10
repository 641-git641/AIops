package assistant

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
)

// repository.go：assistant 持久化（对齐 rag/repository.go 模式）。
// 所有查询在 SQL 层强制 tenant 过滤（禁止跨租户检索后在内存过滤）；
// JSONB 写 ?::jsonb、读 ::text 后反序列化；时间一律 now()/make_interval。

// runLease = worker 崩溃后 running run 复活阈值。
const runLease = 2 * time.Minute

// runMaxAttempts = 重试上限（超过进 dead 并告警，人工介入）。
const runMaxAttempts = 8

// maxClaimBatch 单轮领取上限。
const maxClaimBatch = 50

// ---------- Run（DB 租约） ----------

// EnqueueRun 幂等入队（(tenant, idempotency_key) 唯一——防重复提交/断线重放）。
// 冲突（已存在）时返回既有 run 的 id（DO NOTHING 后回读）。
func EnqueueRun(ctx context.Context, db *gorm.DB, run *AssistantRun) (string, error) {
	run.Status = RunQueued
	run.NextRunAt = time.Now().UTC().Add(-time.Second) // 立即可领取
	err := db.WithContext(ctx).Exec(`
		INSERT INTO assistant_runs (id, tenant_id, thread_id, input_message_id, status, model,
			prompt_version, idempotency_key, attempts, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, now(), now(), now())
		ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
		run.ID, run.TenantID, run.ThreadID, run.InputMessageID, run.Status, run.Model,
		run.PromptVersion, run.IdempotencyKey).Error
	if err != nil {
		return "", err
	}
	// 冲突 → 回读既有 run
	var existing string
	if err := db.WithContext(ctx).Raw(
		`SELECT id FROM assistant_runs WHERE tenant_id = ? AND idempotency_key = ?`,
		run.TenantID, run.IdempotencyKey).Scan(&existing).Error; err != nil {
		return "", err
	}
	if existing != "" && existing != run.ID {
		return existing, nil
	}
	return run.ID, nil
}

// ClaimRuns 领取到期 run（FOR UPDATE SKIP LOCKED；running 超过租约复活）。
// tenantFilter 非空时只领取该租户的 run（测试隔离与多租户 worker 分流；生产传空=全租户）。
func ClaimRuns(ctx context.Context, db *gorm.DB, workerID, tenantFilter string, limit int) ([]AssistantRun, error) {
	if limit <= 0 || limit > maxClaimBatch {
		limit = maxClaimBatch
	}
	args := []any{workerID, int(runLease.Seconds()), limit}
	tenantCond := ""
	if tenantFilter != "" {
		tenantCond = " AND tenant_id = ?"
		args = []any{workerID, int(runLease.Seconds()), tenantFilter, limit}
	}
	var runs []AssistantRun
	err := db.WithContext(ctx).Raw(`
		UPDATE assistant_runs SET status = 'running', locked_at = now(), locked_by = ?,
			attempts = attempts + 1, updated_at = now()
		WHERE id IN (
			SELECT id FROM assistant_runs
			WHERE ((status = 'queued' AND next_run_at <= now())
			   OR (status = 'running' AND locked_at < now() - make_interval(secs => ?)))`+tenantCond+`
			ORDER BY next_run_at ASC
			LIMIT ?
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, tenant_id, thread_id, input_message_id, status, model, prompt_version,
			idempotency_key, attempts, next_run_at, locked_at, locked_by, created_at, updated_at`,
		args...).Scan(&runs).Error
	return runs, err
}

// CompleteRun 标记成功。
func CompleteRun(ctx context.Context, db *gorm.DB, id string, latencyMS int64) error {
	return db.WithContext(ctx).Exec(
		`UPDATE assistant_runs SET status = 'completed', locked_at = NULL, locked_by = '',
			finished_at = now(), latency_ms = ?, error_code = '', updated_at = now() WHERE id = ?`,
		latencyMS, id).Error
}

// FailRun 标记失败：达到上限 → dead；否则指数退避（5s 起、30min 封顶）。
func FailRun(ctx context.Context, db *gorm.DB, id string, attempts int, err error) error {
	status := RunFailed
	backoff := time.Second * 5 << attempts
	if backoff > 30*time.Minute {
		backoff = 30 * time.Minute
	}
	if attempts >= runMaxAttempts {
		status = "dead"
		slog.Error("assistant: run dead", "run", id, "attempts", attempts, "err", err)
	} else {
		slog.Warn("assistant: run failed, will retry", "run", id, "attempts", attempts, "backoff", backoff, "err", err)
	}
	return db.WithContext(ctx).Exec(
		`UPDATE assistant_runs SET status = ?, next_run_at = now() + make_interval(secs => ?),
			error_code = ?, locked_at = NULL, locked_by = '', finished_at = now(), updated_at = now()
		WHERE id = ?`,
		status, int(backoff.Seconds()), truncateStr(err.Error(), 1000), id).Error
}

// CancelRun 取消（仅 queued/running 可取消；终态不可逆）。
func CancelRun(ctx context.Context, db *gorm.DB, tenantID, runID string) error {
	return db.WithContext(ctx).Exec(
		`UPDATE assistant_runs SET status = 'cancelled', locked_at = NULL, locked_by = '',
			finished_at = now(), updated_at = now()
		WHERE tenant_id = ? AND id = ? AND status IN ('queued', 'running')`,
		tenantID, runID).Error
}

// GetRun 按 id 查 run（tenant 强制；不存在返回 (nil, nil)）。
func GetRun(ctx context.Context, db *gorm.DB, tenantID, runID string) (*AssistantRun, error) {
	var run AssistantRun
	err := db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, runID).First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// ListRuns 列出线程的 run（新到旧）。
func ListRuns(ctx context.Context, db *gorm.DB, tenantID, threadID string, limit int) ([]AssistantRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var runs []AssistantRun
	err := db.WithContext(ctx).Where("tenant_id = ? AND thread_id = ?", tenantID, threadID).
		Order("created_at DESC").Limit(limit).Find(&runs).Error
	return runs, err
}

// ---------- Thread ----------

// CreateThread 创建会话。
func CreateThread(ctx context.Context, db *gorm.DB, th *AssistantThread) error {
	return db.WithContext(ctx).Create(th).Error
}

// GetThread 查会话（tenant 强制；软删行不返回）。
func GetThread(ctx context.Context, db *gorm.DB, tenantID, threadID string) (*AssistantThread, error) {
	var th AssistantThread
	err := db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, threadID, ThreadActive).First(&th).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &th, nil
}

// ListThreads 列出会话（新到旧，limit 钳 50）。
func ListThreads(ctx context.Context, db *gorm.DB, tenantID string, limit int) ([]AssistantThread, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var threads []AssistantThread
	err := db.WithContext(ctx).Where("tenant_id = ? AND status = ?", tenantID, ThreadActive).
		Order("updated_at DESC").Limit(limit).Find(&threads).Error
	return threads, err
}

// SoftDeleteThread 软删会话（仅属主租户）。
func SoftDeleteThread(ctx context.Context, db *gorm.DB, tenantID, threadID string) error {
	return db.WithContext(ctx).Exec(
		`UPDATE assistant_threads SET status = 'deleted', updated_at = now()
		WHERE tenant_id = ? AND id = ? AND status = 'active'`, tenantID, threadID).Error
}

// TouchThread 更新会话时间戳（新消息后列表排序）。
func TouchThread(ctx context.Context, db *gorm.DB, tenantID, threadID, title string) error {
	return db.WithContext(ctx).Exec(
		`UPDATE assistant_threads SET updated_at = now(), title = COALESCE(NULLIF(?, ''), title)
		WHERE tenant_id = ? AND id = ?`, title, tenantID, threadID).Error
}

// ---------- Message ----------

// NextMessageSequence 分配消息序号（线程内递增）。
func NextMessageSequence(ctx context.Context, db *gorm.DB, tenantID, threadID string) (int, error) {
	var seq int
	err := db.WithContext(ctx).Raw(
		`SELECT COALESCE(MAX(sequence), 0) + 1 FROM assistant_messages WHERE tenant_id = ? AND thread_id = ?`,
		tenantID, threadID).Scan(&seq).Error
	return seq, err
}

// CreateMessage 插入消息（(tenant, thread, sequence) 唯一——并发幂等）。
func CreateMessage(ctx context.Context, db *gorm.DB, m *AssistantMessage) error {
	return db.WithContext(ctx).Create(m).Error
}

// ListMessages 列出线程消息（升序）。
func ListMessages(ctx context.Context, db *gorm.DB, tenantID, threadID string) ([]AssistantMessage, error) {
	var msgs []AssistantMessage
	err := db.WithContext(ctx).Where("tenant_id = ? AND thread_id = ?", tenantID, threadID).
		Order("sequence ASC").Find(&msgs).Error
	return msgs, err
}

// GetMessage 查单条消息。
func GetMessage(ctx context.Context, db *gorm.DB, tenantID, msgID string) (*AssistantMessage, error) {
	var m AssistantMessage
	err := db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, msgID).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ---------- Feedback ----------

// CreateFeedback 落一条消息反馈（消息归属校验由 Service 层完成）。
func CreateFeedback(ctx context.Context, db *gorm.DB, fb *AssistantFeedback) error {
	return db.WithContext(ctx).Create(fb).Error
}

// ---------- Step ----------

// InsertStep 插入步骤（ordinal 由调用方维护）。
func InsertStep(ctx context.Context, db *gorm.DB, s *AssistantStep) error {
	return db.WithContext(ctx).Create(s).Error
}

// ListSteps 列出 run 步骤（升序）。
func ListSteps(ctx context.Context, db *gorm.DB, tenantID, runID string) ([]AssistantStep, error) {
	var steps []AssistantStep
	err := db.WithContext(ctx).Where("tenant_id = ? AND run_id = ?", tenantID, runID).
		Order("ordinal ASC").Find(&steps).Error
	return steps, err
}

// truncateStr 按 rune 截断（字节截断会切碎 UTF-8 写库失败）。
func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
