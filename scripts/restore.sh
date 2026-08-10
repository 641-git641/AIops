#!/usr/bin/env bash
# ============================================================
# AIOps PostgreSQL 恢复脚本（KEEPON 4.1 数据备份与恢复）
# 恢复是高风险操作：--target 模式强制二次确认；不提供网页按钮。
#
# 用法：
#   bash scripts/restore.sh --verify <backup.dump>            # 演练：恢复临时库 + 关键表计数
#   bash scripts/restore.sh --target <dbname> <backup.dump>   # 真实恢复（提示停 app + 二次确认）
#
# 可选参数：
#   --container <name>    容器名（默认 aiops-postgres；docker exec 执行 pg 工具）
#   --host                使用本机 pg 客户端（需已安装 pg_dump/pg_restore/psql）
#   --dsn <libpq-dsn>     连接串（默认取 AIOPS_DBDSN 环境变量）
#   --passphrase-file <f> 加密镜像（.aes）解密用口令文件
# ============================================================
set -euo pipefail

MODE=""
TARGET_DB=""
DUMP=""
CONTAINER="aiops-postgres"
USE_HOST=0
DSN="${AIOPS_DBDSN:-}"
PASSPHRASE_FILE=""

usage() {
  sed -n '3,13p' "$0"
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --verify) MODE="verify"; shift ;;
    --target) MODE="restore"; TARGET_DB="$2"; shift 2 ;;
    --container) CONTAINER="$2"; shift 2 ;;
    --host) USE_HOST=1; shift ;;
    --dsn) DSN="$2"; shift 2 ;;
    --passphrase-file) PASSPHRASE_FILE="$2"; shift 2 ;;
    -h|--help) usage ;;
    -*) echo "未知参数: $1" >&2; usage ;;
    *) [[ -z "$DUMP" ]] && DUMP="$1" && shift || { echo "未知参数: $1" >&2; exit 2; } ;;
  esac
done

[[ "$MODE" == "" || -z "$DUMP" ]] && { usage; exit 2; }
[[ -f "$DUMP" ]] || { echo "备份文件不存在: $DUMP" >&2; exit 2; }
[[ -n "$DSN" ]] || { echo "未提供连接串（--dsn 或 AIOPS_DBDSN）" >&2; exit 2; }

# ---- 连接信息（libpq keyword/value 解析，支持 '引号值'）----
dsn_val() { # key → value
  local key="$1" v
  for kv in $(echo "$DSN"); do
    case "$kv" in
      "$key="*)
        v="${kv#*=}"; v="${v#\'}"; v="${v%\'}"; echo "$v"; return 0 ;;
    esac
  done
  return 1
}
PG_HOST=$(dsn_val host || echo 127.0.0.1)
PG_PORT=$(dsn_val port || echo 5432)
PG_USER=$(dsn_val user)
PG_PASS=$(dsn_val password)
PG_DB=$(dsn_val dbname)
[[ -n "$PG_USER" && -n "$PG_DB" ]] || { echo "DSN 缺少 user/dbname: $DSN" >&2; exit 2; }

# ---- pg 工具执行（docker exec 或本机）----
pg() { # tool args...
  if [[ $USE_HOST -eq 1 ]]; then
    env PGPASSWORD="$PG_PASS" "$@"
  else
    docker exec -i -e PGPASSWORD="$PG_PASS" "$CONTAINER" "$@"
  fi
}

# ---- 解密加密镜像（.aes）→ 临时明文 dump ----
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
case "$DUMP" in
  *.aes)
    [[ -n "$PASSPHRASE_FILE" && -f "$PASSPHRASE_FILE" ]] || { echo ".aes 镜像需要 --passphrase-file <口令文件>" >&2; exit 2; }
    PLAIN="$WORK/restore.dump"
    go run ./cmd/backup-tool decrypt "$DUMP" "$PLAIN" --passphrase-file "$PASSPHRASE_FILE"
    DUMP="$PLAIN"
    ;;
esac

# ---- 关键表计数（演练与恢复后验证共用）----
verify_counts() { # dbname
  pg psql -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" -d "$1" -t -A -c \
    "SELECT (SELECT count(*) FROM incidents),(SELECT count(*) FROM alerts),(SELECT count(*) FROM knowledge_documents),(SELECT count(*) FROM model_configs),(SELECT count(*) FROM notification_channels)"
}

if [[ "$MODE" == "verify" ]]; then
  TS=$(date +%s)
  TEMP_DB="aiops_restore_${TS}"
  echo "==> 演练模式：恢复到临时库 $TEMP_DB 并计数（结束后自动删除）"
  pg createdb -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" "$TEMP_DB"
  if ! pg pg_restore --no-owner -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" -d "$TEMP_DB" < "$DUMP"; then
    pg dropdb --if-exists -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" "$TEMP_DB" >/dev/null 2>&1 || true
    echo "恢复演练失败（临时库已清理）" >&2
    exit 1
  fi
  echo "==> 计数：incidents | alerts | knowledge_documents | model_configs | notification_channels"
  verify_counts "$TEMP_DB"
  pg dropdb --if-exists -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" "$TEMP_DB"
  echo "==> 演练通过 ✓（计数与源库一致即可放行真实恢复）"
  exit 0
fi

# ---- 真实恢复 ----
echo "============================================================"
echo " 目标数据库 : $TARGET_DB"
echo " 备份文件   : $DUMP"
echo " 连接       : $PG_HOST:$PG_PORT user=$PG_USER（docker exec $CONTAINER）"
echo " 警告       : 目标库将被整体覆盖！"
echo "============================================================"
echo "请先停止后端进程（AIOPS 会持续写入，恢复期间必须停）"
read -r -p "再次输入目标数据库名确认（输入 $TARGET_DB 继续，其他任意键取消）: " CONFIRM
if [[ "$CONFIRM" != "$TARGET_DB" ]]; then
  echo "已取消。"
  exit 1
fi

pg dropdb --if-exists -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" "$TARGET_DB"
pg createdb  -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" "$TARGET_DB"
echo "==> 恢复中（pg_restore --clean --if-exists）..."
pg pg_restore --clean --if-exists --no-owner -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" -d "$TARGET_DB" < "$DUMP"
echo "==> 恢复后计数："
verify_counts "$TARGET_DB"
echo "==> 恢复完成。启动后端（AutoMigrate 会自动补齐缺表/索引），并按 docs/RESTORE.md 核对。"
