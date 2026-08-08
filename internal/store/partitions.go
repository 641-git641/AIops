package store

import (
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
)

// alerts 月分区（规模化延伸）：alerts 按 created_at RANGE 分区，
// 保留策略从「逐行 DELETE」变为「整分区 DROP」（零 WAL、无锁表），
// 是万级/分钟以上写入的长期杠杆。分区命名约定 alerts_pYYYYMM（retention 侧按同名解析）：
//   - alerts_p202608：2026-08 分区（左闭右开 [2026-08-01, 2026-09-01)）
//   - alerts_default：兜底分区（时钟漂移/跨多月运行时插入永不失败）
const (
	alertsPartitionPrefix  = "alerts_p"
	alertsDefaultPartition = "alerts_default"
	alertsLegacyTable      = "alerts_legacy"
)

// partitionNameFor 返回 t 所在月的分区名（2026-08-15 → alerts_p202608）。
func partitionNameFor(t time.Time) string {
	return fmt.Sprintf("%s%04d%02d", alertsPartitionPrefix, t.Year(), t.Month())
}

// partitionBounds 返回 (year, month) 分区的左闭右开边界（UTC 字符串，12 月跨年处理）。
func partitionBounds(y int, m time.Month) (from, to string) {
	from = fmt.Sprintf("%04d-%02d-01 00:00:00+00", y, m)
	ny, nm := y, m+1
	if m == 12 {
		ny, nm = y+1, 1
	}
	to = fmt.Sprintf("%04d-%02d-01 00:00:00+00", ny, nm)
	return from, to
}

// alertTableExists 检查 alerts 表是否存在（regclass cast 在表不存在时会报错，必须先判存在）。
func alertTableExists(db *gorm.DB) (bool, error) {
	var n int64
	err := db.Raw(
		"SELECT count(*) FROM pg_class WHERE relname = 'alerts' AND relkind IN ('r','p')").Scan(&n).Error
	return n > 0, err
}

// alertPartitioned 检查 alerts 是否为分区表。
func alertPartitioned(db *gorm.DB) (bool, error) {
	var n int64
	err := db.Raw(
		"SELECT count(*) FROM pg_partitioned_table WHERE partrelid = 'alerts'::regclass").Scan(&n).Error
	return n > 0, err
}

// ensureAlertPartitions 确保 alerts 为月分区表：
//   - 未分区 → 事务内迁移（RENAME 旧表 → 建分区父表 → 逐月建分区 → 搬迁 → DROP legacy）；
//   - 已分区 → 维护「当月/次月 + default 兜底」分区（跨月运行自动补建）。
//
// 必须在 AutoMigrate 之后调用（保证 alerts 表已存在；首次启动空库时
// AutoMigrate 先建普通表，随后本函数原地迁移为分区表）。
func ensureAlertPartitions(db *gorm.DB) error {
	exists, err := alertTableExists(db)
	if err != nil {
		return fmt.Errorf("store: check alerts table: %w", err)
	}
	if !exists {
		slog.Warn("store: alerts table missing, skip partitioning")
		return nil
	}
	partitioned, err := alertPartitioned(db)
	if err != nil {
		return fmt.Errorf("store: check alerts partitioned: %w", err)
	}
	if !partitioned {
		if err := migrateToPartitioned(db); err != nil {
			return err
		}
		slog.Info("store: alerts migrated to monthly partitions")
	}
	if err := ensureUpcomingPartitions(db); err != nil {
		return err
	}
	// 索引兜底：AutoMigrate 在迁移前把索引建在普通表上，RENAME/DROP legacy 时被带走，
	// 新分区父表需幂等补建（CREATE INDEX 父表 → PG 自动传播到所有分区）。
	return ensureAlertIndexes(db)
}

// ensureAlertIndexes 幂等补齐分区父表上的功能索引（与模型标签一致，AutoMigrate 后续
// 检测到同名索引存在即跳过，不重复建）。
func ensureAlertIndexes(db *gorm.DB) error {
	for _, ddl := range []string{
		"CREATE INDEX IF NOT EXISTS idx_alerts_created ON alerts (created_at)",
		"CREATE INDEX IF NOT EXISTS idx_alerts_status ON alerts (status)",
		"CREATE INDEX IF NOT EXISTS idx_alerts_tenant_created ON alerts (tenant_id, severity, fingerprint)",
	} {
		if err := db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("store: ensure alert index: %w", err)
		}
	}
	return nil
}

// migrateToPartitioned 将现有普通表原地迁移为分区表（单事务，失败整体回滚）。
// 列定义与 GORM 模型自动迁移结果逐列对齐（varchar(n)/jsonb/timestamptz，无 FK）。
func migrateToPartitioned(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// 1. 旧表改名（残留 legacy 视为人工干预，由启动失败暴露）
		if err := tx.Exec(fmt.Sprintf("ALTER TABLE IF EXISTS alerts RENAME TO %s", alertsLegacyTable)).Error; err != nil {
			return fmt.Errorf("store: rename alerts: %w", err)
		}
		// 2. 分区父表：PK 必须含分区键 created_at（PG 分区表唯一约束要求）；
		//    B-tree 左前缀使 WHERE id=? 仍走 (id, created_at) 索引
		ddl := `CREATE TABLE alerts (
    id           varchar(36)  NOT NULL,
    tenant_id    varchar(64),
    provider     varchar(64),
    provider_type varchar(64),
    fingerprint  varchar(64),
    alert_hash   varchar(64),
    event        jsonb,
    name         varchar(255),
    severity     varchar(16),
    status       varchar(16),
    labels       jsonb,
    started_at   timestamptz,
    last_seen_at timestamptz,
    created_at   timestamptz,
    updated_at   timestamptz,
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at)`
		if err := tx.Exec(ddl).Error; err != nil {
			return fmt.Errorf("store: create partitioned alerts: %w", err)
		}
		// 3. 存量覆盖分区：从旧表 min(created_at) 首月 → 当前月+1 逐月建
		var minTS time.Time
		if err := tx.Raw(fmt.Sprintf(
			"SELECT COALESCE(min(created_at), now()) FROM %s", alertsLegacyTable)).Scan(&minTS).Error; err != nil {
			return fmt.Errorf("store: read legacy min: %w", err)
		}
		now := time.Now().UTC()
		lastY, lastM := now.AddDate(0, 1, 0).Year(), now.AddDate(0, 1, 0).Month()
		for y, m := minTS.Year(), minTS.Month(); ; {
			if err := createPartition(tx, y, m); err != nil {
				return err
			}
			if y == lastY && m == lastM {
				break
			}
			y, m = nextMonth(y, m)
		}
		// 4. 兜底分区（不属任何显式分区的新数据）
		if err := tx.Exec(fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF alerts DEFAULT", alertsDefaultPartition)).Error; err != nil {
			return fmt.Errorf("store: create default partition: %w", err)
		}
		// 5. 数据搬迁（分批 5000，防长事务；created_at 同值超批量的极端情况会死循环——
		//    告警 created_at 为写入时刻秒级精度，实际不会发生）
		cols := "id, tenant_id, provider, provider_type, fingerprint, alert_hash, event, " +
			"name, severity, status, labels, started_at, last_seen_at, created_at, updated_at"
		const batchSize = 5000
		var lastMax time.Time
		for {
			res := tx.Exec(fmt.Sprintf(
				"INSERT INTO alerts (%s) SELECT %s FROM %s WHERE created_at > ? ORDER BY created_at LIMIT %d",
				cols, cols, alertsLegacyTable, batchSize), lastMax)
			if res.Error != nil {
				return fmt.Errorf("store: migrate legacy rows: %w", res.Error)
			}
			if res.RowsAffected < batchSize {
				break
			}
			// 下轮起点 = 本轮最后一行 created_at
			if err := tx.Raw(fmt.Sprintf(
				"SELECT max(created_at) FROM (SELECT created_at FROM %s ORDER BY created_at LIMIT %d) t",
				alertsLegacyTable, batchSize)).Scan(&lastMax).Error; err != nil {
				return fmt.Errorf("store: read legacy max: %w", err)
			}
		}
		// 6. 删除旧表（分区化不可逆；迁移失败整体回滚，legacy 保留）
		if err := tx.Exec(fmt.Sprintf("DROP TABLE %s", alertsLegacyTable)).Error; err != nil {
			return fmt.Errorf("store: drop legacy alerts: %w", err)
		}
		return nil
	})
}

// ensureUpcomingPartitions 维护当月 + 次月分区与 default 兜底（幂等，跨月运行自动补建）。
func ensureUpcomingPartitions(db *gorm.DB) error {
	now := time.Now().UTC()
	for _, t := range []time.Time{now, now.AddDate(0, 1, 0)} {
		if err := createPartition(db, t.Year(), t.Month()); err != nil {
			return err
		}
	}
	var n int64
	if err := db.Raw("SELECT count(*) FROM pg_class WHERE relname = ? AND relkind = 'r'",
		alertsDefaultPartition).Scan(&n).Error; err != nil {
		return fmt.Errorf("store: check default partition: %w", err)
	}
	if n == 0 {
		if err := db.Exec(fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF alerts DEFAULT", alertsDefaultPartition)).Error; err != nil {
			return fmt.Errorf("store: create default partition: %w", err)
		}
	}
	return nil
}

// createPartition 建 (year, month) 的分区（幂等；名字由自身生成，无注入面）。
func createPartition(tx *gorm.DB, y int, m time.Month) error {
	name := partitionNameFor(time.Date(y, m, 1, 0, 0, 0, 0, time.UTC))
	from, to := partitionBounds(y, m)
	return tx.Exec(fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF alerts FOR VALUES FROM ('%s') TO ('%s')",
		name, from, to)).Error
}

// nextMonth 返回下一个月（12 月跨年）。
func nextMonth(y int, m time.Month) (int, time.Month) {
	if m == 12 {
		return y + 1, 1
	}
	return y, m + 1
}
