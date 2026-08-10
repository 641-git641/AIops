# AIOps 配置迁移与换机器恢复指南（KEEPON 8.1）

> 用途：迁移到新机器/新环境时的步骤与检查清单。原则：**数据与配置尽量走数据库与受保护备份迁移，模型密钥明文不单独导出**。

## 1. 配置分布一览

| 配置 | 存储位置 | 迁移方式 |
| --- | --- | --- |
| 告警/事故/规则/剧本/维护窗口/工作流 | PostgreSQL | 备份恢复（见 §2） |
| 通知渠道（钉钉/飞书/企微/HTTP） | `notification_channels` 表（env 仅首启种子） | 随 DB 迁移；重启后自动读取 |
| 模型配置（Chat / Assistant Chat 的 baseURL/model/**apiKey**） | `model_configs` 表 | 随 DB 迁移（**密文随备份走，不单独导出**）；不传环境变量 |
| 外部系统入口（Grafana/Prometheus/日志） | `external_systems` 表（env 仅首启种子） | 随 DB 迁移；`AIOPS_EXTERNALSYSTEMS` 仅首次播种用 |
| 知识库/评测集 | `knowledge_*` / `rag_*` 表 | 随 DB 迁移；向量列需同维度（见 §3 注意） |
| 对话 Agent 会话/动作请求 | `assistant_*` 表 | 随 DB 迁移 |
| 访问密钥（API key） | `api_keys` 表 | 随 DB 迁移；迁移后可用「设置 → 访问密钥」重新生成 |
| 备份镜像（可选异地副本） | `AIOPS_BACKUPMIRRORDIR` 目录（AES 加密） | 拷贝目录 + 保存密钥文件（`cmd/backup-tool decrypt` 可解密验证） |
| 日志 | `AIOPS_LOGFILE` 文件 | 不迁移（按需拷贝） |

## 2. 标准迁移流程（推荐：备份 → 恢复）

### 2.1 源机器：生成备份

```bash
# 方式 A：应用内定时/手动备份（推荐，已加密镜像可选）
AIOPS_BACKUPENABLED=true AIOPS_BACKUPDIR=./backups ./bin/aiops.exe
# 备份文件出现在 backups/aiops_*.dump（AIOPS_BACKUPMIRRORDIR 下为 .aes 加密镜像）

# 方式 B：docker 直接导出（不依赖应用配置）
docker exec aiops-postgres pg_dump -U aiops -Fc -d aiops > aiops_$(date +%F).dump
```

### 2.2 目标机器：恢复（详细步骤见 docs/RESTORE.md）

```bash
# 1. 启动 PostgreSQL（同一 docker-compose）
docker compose up -d postgres

# 2. 恢复（--verify 先演练，--target 真实恢复，均有二次确认）
./scripts/restore.sh --verify  --file aiops_2026-08-10.dump
./scripts/restore.sh --target  --file aiops_2026-08-10.dump
```

### 2.3 目标机器：启动

```bash
# 只带「非 DB 化」的环境变量：
#   AIOPS_DBDSN / AIOPS_AUTHTYPE / AIOPS_AGENTENABLED / AIOPS_NOTIFY*（可省略——渠道在 DB）
#   AIOPS_EMBEDDING*（Embedding 不走 DB，需重新配置）
#   AIOPS_BACKUP*（新机器的备份策略）
# 模型密钥不需要传环境变量：model_configs 表恢复后即生效
AIOPS_DBDSN="host=127.0.0.1 user=aiops password=aiops dbname=aiops port=5432 sslmode=disable" \
AIOPS_EMBEDDINGDRIVER=local AIOPS_EMBEDDINGMODEL=mock-local AIOPS_EMBEDDINGDIMENSION=64 \
./bin/aiops.exe
```

启动后验证：`GET /api/v1/overview`（知识计数/备份状态）、`GET /api/v1/model-configs`（apiKeySet=true 表示密钥随 DB 恢复）、`GET /api/v1/external-systems`。

## 3. 换机器检查清单

- [ ] PostgreSQL 版本一致（推荐与 docker-compose 相同的 PG16 + pgvector）
- [ ] **向量维度一致**：恢复后 `AIOPS_EMBEDDINGDIMENSION` 必须与在线索引维度相同（不一致时向量路自动降级关键词并提示 model/dimension mismatch；换维度需 `AIOPS_RAGINDEXVERSION` 递增并重建索引）
- [ ] 备份目录 `backups/` 与镜像目录（如启用）权限 0700/0600，密钥文件妥善保管
- [ ] `AIOPS_AUTHTYPE=apikey` 时：迁移后重启会自动沿用已有 key（表非空不重新生成）；丢失全部 key 时删除 `api_keys` 表再重启会重新打印 owner key
- [ ] 前端重新 `npm install && npm run build`（或拷贝 web/dist），`vite.config.ts` 的代理目标指向新后端
- [ ] 日志目录（AIOPS_LOGFILE）按需重建；`logs/` 不入 git
- [ ] 演练：用 `./scripts/restore.sh --verify` 恢复一次临时库，核对 incidents / alerts / knowledge_documents / assistant_threads 计数与源库一致

## 4. 显式不做

- **不单独导出模型密钥明文**：apiKey 只存在于 `model_configs` 表（受保护备份）与运行中进程内存；迁移一律通过备份完成
- **不做网页恢复按钮**：恢复是高风险操作，仅通过 `scripts/restore.sh`（二次确认）
- **不迁移本地日志/临时目录**：按需重建即可
