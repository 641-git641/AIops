package rag

import (
	"fmt"

	"gorm.io/gorm"
)

// SchemaOptions 显式 SQL migration 选项。
type SchemaOptions struct {
	Dimension int  // 向量维度（索引契约；与列不一致时报错，禁止混维）
	HNSW      bool // 向量 HNSW 索引开关（默认精确扫描基线；数据量/延迟超门槛后再启用）
}

// EnsureSchema 安装 pgvector/pg_trgm 扩展并补齐知识表的向量/全文列与索引。
// 表结构由 GORM AutoMigrate（store.migrate models 列表）创建，这里只做扩展类 DDL——
// 生产环境 CREATE EXTENSION 权限与索引构建由部署流程控制（RAG_PLAN §7）。
// Dimension <= 0（keyword-only 模式）时跳过向量扩展/列/HNSW，只维护 pg_trgm + tsvector。
func EnsureSchema(db *gorm.DB, opt SchemaOptions) error {
	for _, ext := range []string{"vector", "pg_trgm"} {
		if opt.Dimension <= 0 && ext == "vector" {
			continue // keyword-only：不装向量扩展
		}
		if err := db.Exec(fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s", ext)).Error; err != nil {
			return fmt.Errorf("rag: create extension %s: %w", ext, err)
		}
	}
	if opt.Dimension > 0 {
		// 向量列：先查现有维度（pgvector 0.8.x 的 atttypmod = 维度本身，已实测验证；
		// 升级 pgvector 大版本需复核该编码），不一致直接报错（索引契约，禁止混维重建）。
		var typmod *int
		if err := db.Raw(`SELECT atttypmod FROM pg_attribute
			WHERE attrelid = 'knowledge_embeddings'::regclass AND attname = 'embedding'`).Scan(&typmod).Error; err != nil {
			return fmt.Errorf("rag: inspect embedding column: %w", err)
		}
		if typmod == nil {
			if err := db.Exec(fmt.Sprintf(
				"ALTER TABLE knowledge_embeddings ADD COLUMN embedding vector(%d)", opt.Dimension)).Error; err != nil {
				return fmt.Errorf("rag: add embedding column: %w", err)
			}
		} else if *typmod != opt.Dimension {
			return fmt.Errorf("rag: embedding column dimension %d != configured %d（换维度必须重建新索引版本）",
				*typmod, opt.Dimension)
		}
	}
	// 全文列（tsvector 不进 GORM 模型）+ GIN 索引
	if err := db.Exec("ALTER TABLE knowledge_chunks ADD COLUMN IF NOT EXISTS content_tsv tsvector").Error; err != nil {
		return fmt.Errorf("rag: add content_tsv column: %w", err)
	}
	ddl := []string{
		"CREATE INDEX IF NOT EXISTS idx_kc_tsv ON knowledge_chunks USING GIN (content_tsv)",
		"CREATE INDEX IF NOT EXISTS idx_kc_trgm ON knowledge_chunks USING GIN (content gin_trgm_ops)",
		"CREATE INDEX IF NOT EXISTS idx_kd_metadata ON knowledge_documents USING GIN (metadata jsonb_path_ops)",
	}
	if opt.HNSW && opt.Dimension > 0 {
		ddl = append(ddl, "CREATE INDEX IF NOT EXISTS idx_ke_hnsw ON knowledge_embeddings USING hnsw (embedding vector_cosine_ops)")
	}
	for _, stmt := range ddl {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("rag: ensure index %s: %w", stmt, err)
		}
	}
	return nil
}

// IndexMeta 在线索引元数据（检索前一致性检查：模型/索引版本不匹配时禁用向量路）。
type IndexMeta struct {
	ModelID      string
	IndexVersion string
	Dimension    int // GORM 按列名映射：SQL 需 AS dimension（atttypmod 名无法映射）
}

// LoadIndexMeta 读取在线索引元数据（空表返回零值；不一致由调用方降级）。
// 维度取自 pg_attribute.atttypmod（pgvector 0.8.x 的 typmod = 维度，已实测；
// 升级 pgvector 大版本时需复核该编码，注释见 EnsureSchema）。
// 单条 SQL 一次取齐（检索热路径只多 1 次往返；Service 层另有 30s TTL 缓存）。
func LoadIndexMeta(db *gorm.DB) IndexMeta {
	var m IndexMeta
	_ = db.Raw(`
		SELECT
			(SELECT model_id FROM knowledge_embeddings LIMIT 1) AS model_id,
			(SELECT index_version FROM knowledge_embeddings LIMIT 1) AS index_version,
			COALESCE((SELECT atttypmod FROM pg_attribute
				WHERE attrelid = 'knowledge_embeddings'::regclass AND attname = 'embedding'), 0) AS dimension`).
		Scan(&m).Error
	return m
}
