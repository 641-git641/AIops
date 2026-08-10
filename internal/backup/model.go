// Package backup 实现 PostgreSQL 定时备份（KEEPON 4.1 数据备份与恢复）：
// pg_dump -Fc → 本机轮转（数量+容量双限）→ 可选异地镜像（AES-256-GCM 加密）→
// 可选恢复校验（临时库 + 关键表计数）。恢复是高风险操作，仅通过 scripts/restore.sh
// 执行（交互二次确认），不提供网页按钮。
package backup

import "time"

// BackupRun 备份执行记录（backup_runs 表；校验失败独立呈现，备份本身仍视为 success）。
type BackupRun struct {
	ID              string     `gorm:"primaryKey;size:36" json:"id"`
	Trigger         string     `gorm:"size:16" json:"trigger"` // schedule | manual
	StartedAt       time.Time  `json:"startedAt"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	Status          string     `gorm:"size:16" json:"status"` // running | success | failed
	FilePath        string     `gorm:"size:512" json:"filePath,omitempty"`
	FileSize        int64      `json:"fileSize,omitempty"`
	SHA256          string     `gorm:"size:64" json:"sha256,omitempty"`
	MirrorPath      string     `gorm:"size:512" json:"mirrorPath,omitempty"`
	MirrorEncrypted bool       `json:"mirrorEncrypted"`
	VerifyStatus    string     `gorm:"size:16" json:"verifyStatus,omitempty"` // skipped | passed | failed
	VerifyDetail    string     `gorm:"size:2048" json:"verifyDetail,omitempty"`
	ErrorDetail     string     `gorm:"size:2048" json:"errorDetail,omitempty"`
}

// TableName 表名。
func (BackupRun) TableName() string { return "backup_runs" }
