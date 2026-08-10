# 数据备份与恢复（KEEPON 4.1）

> 目标：**数据可恢复，并且定期验证真的能恢复**。备份由平台定时生成；恢复只通过
> `scripts/restore.sh` 命令行执行（高风险操作，强制二次确认，不做网页按钮）。

## 一、备份是怎么生成的

| 项 | 说明 |
|---|---|
| 开关 | `AIOPS_BACKUPENABLED=true`（默认关闭） |
| 产物 | `pg_dump -Fc` 自定义格式：`backups/aiops_<UTC时间戳>_<id>.dump`（目录 0700、文件 0600） |
| 节奏 | 默认每 24h 一次；**距上次成功超过间隔才补跑**（启动时也会补跑，`AIOPS_BACKUPONBOOT=false` 关闭） |
| 轮转 | 数量（默认保留 10 份）+ 总容量（默认 5GB）双限，任一达限删最旧 |
| 校验 | 默认每次备份后恢复到临时库 `aiops_bkverify_<ts>` 并计数 5 张关键表（incidents / alerts / knowledge_documents / model_configs / notification_channels），通过后删除临时库 |
| 镜像 | `AIOPS_BACKUPMIRRORDIR=<另一磁盘或 NAS 挂载点>` 时同步一份；`AIOPS_BACKUPMIRRORENCRYPT=true` 时以 AES-256-GCM 加密（需 `AIOPS_BACKUPPASSPHRASEFILE` 口令文件，权限建议 0600） |

执行细节：pg 工具经 `docker exec` 在 `aiops-postgres` 容器内运行（本机无需安装 PostgreSQL 客户端）；
装有客户端的机器可设 `AIOPS_BACKUPPGCONTAINER=host`。备份状态见「设置 → 数据备份」页与
`/metrics`（`aiops_backup_state`、`aiops_backup_last_success_age_seconds`、`aiops_backup_runs_total`）。

## 二、恢复演练（每月一次，有记录）

```bash
bash scripts/restore.sh --verify backups/aiops_20260809_153710.123_abc12345.dump
```

预期输出：`==> 演练通过 ✓`，且计数与当前库一致（可先在页面确认各表大致数量）。

```text
==> 计数：incidents | alerts | knowledge_documents | model_configs | notification_channels
12|345|8|2|1
```

> 演练会真实创建临时库并恢复，完成后自动删除；失败也会清理临时库。这是验证"备份真的能恢复"的唯一可靠方式，建议每月执行一次并在 PROGRESS/运维记录中留痕。

## 三、真实恢复（灾难场景）

```bash
# 1. 停止后端（恢复期间不能有写入）
#    前端无需停止；后端进程 Ctrl+C 或 taskkill 后确认退出

# 2. 执行恢复（会提示先确认目标库名，输入两次相同库名才继续）
bash scripts/restore.sh --target aiops backups/aiops_xxx.dump
# 或带自定义连接：--dsn "host=... user=... password=... dbname=..." 

# 3. 重启后端
./bin/aiops.exe
```

恢复后核对：

1. 启动日志无 `store open` 错误（AutoMigrate 自动补齐缺表/索引）。
2. 「设置 → 数据备份」最近记录与备份时一致。
3. `curl localhost:8080/api/v1/incidents` 计数与恢复时 psql 输出一致。
4. 抽查一次 RAG 相似事故检索（`/incidents/:id/similar`）与知识库索引状态（`/rag/status`）。

## 四、加密镜像的恢复

```bash
# 先解密（需要口令文件；本机需 Go 工具链）
go run ./cmd/backup-tool decrypt backups/aiops_xxx.dump.aes /tmp/restore.dump \
  --passphrase-file backups/.passphrase

# 再按上文演练/恢复流程，用 /tmp/restore.dump 作为备份文件
bash scripts/restore.sh --verify /tmp/restore.dump
```

> 口令文件一旦丢失，加密镜像不可恢复——请把口令文件与加密镜像分开存放（如镜像在 NAS，口令在
> 密码管理器或另一台机器），并确保至少一人知道口令文件位置。

## 五、恢复前检查清单

- [ ] 后端已停止（`netstat -ano | findstr :8080` 无监听）
- [ ] 目标库名确认无误（`--target` 参数与二次确认一致）
- [ ] 本次失败也不会丢数据：保留一份最新备份（轮转会自动保留，勿手动删）
- [ ] 源库与备份的连接串一致（`AIOPS_DBDSN` 与备份时相同）
- [ ] 演练模式先跑一遍（`--verify`）确认备份文件完好

## 六、换机器迁移

1. 旧机器：停后端 → `docker exec aiops-postgres pg_dump -Fc -U aiops -d aiops > aiops-migration.dump`
   （或直接用 `backups/` 里最近的备份文件）。
2. 新机器：装 docker + 拉取 `pgvector/pgvector:pg16`，`docker compose -f deploy/docker-compose.yml up -d postgres`。
3. `bash scripts/restore.sh --target aiops aiops-migration.dump`（需在新机器上跑，连接串指向新库）。
4. 环境变量清单：`AIOPS_DBDSN`、`AIOPS_AUTHTYPE`（含 owner key——若迁移前未保存密钥，
   恢复后直接访问会进解锁页，此时表内已有 owner key，重新生成即可：设置 → 访问密钥 →
   重新生成 owner key）、模型配置（Chat LLM 的 `model_configs` 表随库恢复，**apiKey 不单独导出**——见下；
   **Embedding 不走 DB**——`AIOPS_EMBEDDING*` 环境变量需在新机器重新配置）。
5. 密钥安全：模型 API Key 明文只存在数据库（`model_configs.api_key`，仅 Chat LLM）；迁移一律**随数据库备份恢复**，
   不要手动复制。Embedding key（`AIOPS_EMBEDDINGAPIKEY`）在环境变量里，需另行保管。备份目录与口令文件需一并迁移（口令文件权限 0600）。
