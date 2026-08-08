# Phase 3 规模化压测基准报告

> 范围：Postgres 加固（连接池/索引/保留策略）+ 万级吞吐验收 + 拓扑影响面（SQL 递归 CTE）。
> 验收口径：**万级告警/分钟 ≈ 167 告警/s**；达标要求：失败率 ≤1%。

## 环境

- 运行平台：Windows 11（本机回环 127.0.0.1），16 逻辑核
- PostgreSQL：Docker Desktop 容器（docker-compose），单实例
- 服务端：aiops（Go 1.26 + Gin + GORM），连接池 50/50，:8081 独立实例
- 压测工具：cmd/loadgen（模拟 Prometheus Alertmanager webhook）

## 结论摘要

| 场景 | 并发 | 吞吐 | 失败率 | P50 | P99 | 结果 |
|---|---|---|---|---|---|---|
| 200 rps（≈1.2万/min，加固前验收） | 40 | 200/s | 0% | 34ms | 55ms | ✅ PASS |
| 2000 rps（修复 loadgen 后） | 300 | **1786/s**（≈10.7万/min） | **0.12%** | 61ms | 100ms | ✅ **PASS** |
| 5000 rps（上限探索） | 500 | 2610/s | 2.2%¹ | 164ms | 516ms | 服务端未饱和 |

¹ 500 并发下 refused 来自压测客户端在 Windows 回环上的边缘行为（见「根因」），非服务端能力问题；服务端直方图 90% 完成 ≤250ms。

**验收达标**：万级/分钟口径以 **10 倍余量**通过（1786/s vs 167/s），失败率 0.12%，P99 100ms。

## 服务端观测（GET /metrics）

2000 rps / 300 并发 15s 轮（PASS 轮）后直方图：

```
aiops_pipeline_duration_seconds_count  111099   （累计，含上限探索轮）
aiops_pipeline_duration_seconds_sum    11421s   → avg ≈ 102ms（极值 500 并发下）
bucket le=0.05   45079   le=0.1  70702   le=0.25  99644   le=0.5  110681
```

- 单条告警管线（去重 → 入库 → CEL 规则 → Incident → WS → Agent/工作流投递）P50 ≈ 50–80ms、P90+ ≤250ms
- 去重构成（2000 rps 轮）：`update` 92,947 / `duplicated` 18,152 / `new` 0（重放既有 fingerprint，幂等路径）——**两级去重在洪峰下正确工作**：内容重复直接丢弃不入库

## 根因链：压测崩溃的真相（本阶段最重要的教训）

**现象**：300 并发/2000 rps 时客户端 60–90% 超时失败，而服务端直方图显示全部 ≤250ms、Postgres 空闲、无锁等待、无慢查询。

**诊断方法**（按序排除）：
1. 服务端 /metrics 直方图 → 排除 ProcessEvent 慢
2. pg_stat_activity / pg_locks 压测中采样 → 排除 DB 层（查询亚毫秒、0 锁等待、池 53 连接健康）
3. 阶梯并发实验（50→100→200→300）→ 定位拐点恰在 50–100 之间
4. loadgen 错误分类（timeout/socket/refused）→ **全部是 `refused`**
5. netstat 压测中采样 → **TIME_WAIT 爆炸式增长**
6. 结论：**压测客户端每请求新建 TCP 连接**（`resp.Body.Close()` 前未读 body → net/http Transport 判定连接污染而销毁 → 复用失效）→ 回环连接风暴 → Windows listen 队列拒绝 → 客户端误读为服务端故障

**修复（loadgen）**：
- 读完响应 body（`io.Copy(io.Discard, resp.Body)`）再 Close → 连接回池
- 共享 transport（`MaxIdleConnsPerHost` 放大）+ Dial 计数埋点 → 复用率 98.6%（387 dials / 27000 请求）
- 修复后同参数：84/s → 1786/s（**21 倍**），PASS

**启示**：压测工具必须模拟真实监控系统的 keep-alive 长连接行为；客户端指标与服务端指标出现分歧时，优先查连接层而非应用逻辑。

## 服务端加固清单（本阶段落地）

| 项 | 内容 | 动机 |
|---|---|---|
| 连接池调优 | `DBMaxOpenConns=20` / `DBMaxIdleConns=20`（= MaxOpen）/ 存活 30min / 空闲 5min | 压测中曾现 Windows 回环 TIME_WAIT 冲突（`connectex: Only one usage of each socket address`）——空闲连接重连导致；MaxIdle=MaxOpen 消除连接抖动 |
| 启动重试 | `DBConnectRetries=5`，Ping 3s 超时 + 2s 退避 | docker-compose Postgres 健康检查窗口内启动不炸 |
| 热表索引 | `alerts.created_at`、`audit_logs.created_at`、`workflow_runs.started_at` | 保留策略批量清理的索引保障 |
| 保留策略 worker | alerts 30d / audit 180d / workflow_runs 90d，分批删除（5000/批，`DELETE ... id IN (SELECT ... LIMIT ?)` 循环至影响行数 < 批） | 防止明细表无限膨胀；boot 清理一次 + 定时（默认 60min） |
| WindowFilter 锁外查库 | 维护窗口缓存改为 RWMutex 快路径 + 锁外 DB 加载 + double-check 回填 | 原实现全局 Mutex 内查库：TTL 过期瞬间所有并发请求在锁前排大队，周期性吞吐崩塌（压测早期复现，防御性修复） |
| 指标出口 | `GET /metrics`（stdlib Prometheus 文本格式）：管线耗时直方图、去重构成、Incident 来源、队列深度 | 压测观测 + 生产可观测性基础 |

## 拓扑影响面（C1/C2 已 e2e 验证）

- `topology_edges` 归一化边表（服务 Create/启动 backfill 同步）
- `GET /api/v1/topology/:name/impact?depth=5`：SQL `WITH RECURSIVE` 反向遍历依赖图（深度/路径/防环）
- 验证：`p3-pay` 故障 → cart(1)/order(1)/checkout(2)/web(3) 影响面正确；depth=1 限制正确
- 扩展点：Neo4j 后置时仅替换该查询（接口与返回结构不变）

## 写入优化（规模化延伸，2026-08-08）

### D1：单 SQL 双表写入（CTE）

- 每事件 DB 往返 **4 次（BEGIN/INSERT/UPSERT/COMMIT）→ 1 次单语句**：`WITH ins AS (INSERT alerts ... RETURNING) INSERT last_alerts ... ON CONFLICT`（`internal/alert/save_sql.go`，单语句原子语义不变）。
- 实测（2000rps/300 并发/30s，同 Phase 3 参数）：**P50 61→51ms（-17%）、P99 100→88ms（-12%）**，吞吐 1790/s 持平——单次写入往返已不再是瓶颈，剩余 ~50ms/事件为管线串行成本（dedup 2 读 + 规则 1 读 + 写 1 次 ≈ 4 次 DB 往返 + 业务逻辑）。
- 注意：JSON 列必须 `string(datatypes.JSON)` + `?::jsonb` 绑定（直接传 []byte 会被 pgx 编成 bytea 报 cast 错误）；raw Exec 无 GORM 自动回填 CreatedAt/UpdatedAt，需 Go 侧回填。

### D4：批量扫描 → SaveAlerts 批量写（D2 落地，Phase 3g）

`loadgen -batch N`（-rate 恒为事件/秒；2000 事件/s、300 并发、variety 400/200）原扫描结论——**请求延迟 ≈ batch × 50ms（每事件处理时间恒定）**，瓶颈是逐事件管线串行，不是语句本身：

| batch | 请求延迟 P50 | 事件吞吐 | 失败率 |
|---|---|---|---|
| 1 | 51ms | 1790/s | 0.14% |
| 20 | 973ms | 1621/s | 5% |
| 50 | 2072ms | 1317/s | 12% |
| 100 | 3769ms | 1000/s | 31% |

- batch 失败全为 `refused`（单请求时长 1s+ 占连接 → Windows 回环 accept 队列耗尽），非服务端故障（Linux 部署无此边缘行为）。

**✅ Phase 3g 已落地**（`Repository.SaveAlerts(ctx, []*Alert, []DedupKind)` + `GetLastAlerts` + 批内 overlay `classifyBatchLast`）：
- dedup 批量读 + 批量写合并为**每批 2 次 DB 往返**（1 读 + 1 写），业务副作用（规则/WS/Agent/工作流）仍逐事件。批量读失败自动回退逐事件；批量写失败逐条 `SaveAlert` 降级。
- **禁止裸延迟 flush**：批内 overlay 模拟「前序事件已写」后的 last_alerts 状态，同批同 fingerprint 后序事件必须看到前序状态（等价逐事件读 DB）。
- **关键正确性**：单语句多行 CTE 的 last_alerts `ON CONFLICT DO UPDATE` 若两行同键会触发 PG「cannot affect row a second time」→ last_alerts 在 Go 侧**折叠**（每 fingerprint 保留批内最后一条），alerts 仍写全部历史行。
- 实测（loadgen `-batch 20`，2000 事件/s/300 并发/30s/variety 200）：

  | 路径 | 请求 P50 | 失败率 | 事件吞吐 |
  |---|---|---|---|
  | Phase 3b 逐事件（扫描） | 973ms | 5% | 1621/s |
  | **Phase 3g 批量写** | **197ms** | **0%** | **1774/s** |

  **请求延迟 -80%（973→197ms）、失败率 5%→0%**，吞吐持平。批内 overlay 去重正确拦截同内容（duplicated 6981 / update 26561 / new 2461）。

### D3：alerts 月分区 + 保留策略 DROP PARTITION

- 迁移（启动时 AutoMigrate 后原地执行，事务内幂等）：RENAME → 分区父表（PK `(id, created_at)`）→ 存量覆盖分区 + `alerts_default` 兜底 → 分批搬迁 → DROP legacy。已分区仅维护当月/次月 + default。
- 保留策略：整月过期分区 **DROP（零 WAL、无锁表）** + 当月残余行级 DELETE 兜底（e2e：6 月分区 DROP + 7 月残余行删除双路径验证通过）。
- 兼容性：Phase 3 binary 写分区表正常（透明路由）；`AIOPS_ALERTSPARTITIONS=false` 降级为纯行级 DELETE（已分区表可用，**不提供反向迁移**）。
- 踩坑记录：① AutoMigrate 先建索引于普通表，RENAME/DROP legacy 时被带走 → 迁移后需 `ensureAlertIndexes` 幂等补建（父表建 → PG 自动传播）；② default 分区已有数据时补建历史分区会报 `updated partition constraint ... would be violated`（PG 语义：先清数据再建分区）；③ GORM AutoMigrate 不建分区表，列 DDL 需手动逐列对齐真实表（varchar(n)/jsonb/timestamptz）。

### D5：规则引擎规则缓存（Phase 3c）

- 原逐事件管线 4 次 DB 往返（dedup 2 读 + **规则 1 读** + 写 1 次）；`RuleEngine` 按租户缓存启用规则（TTL 默认 5s，RWMutex + 锁外查库 + double-check，对齐 WindowFilter），`POST /rules` 后 `Invalidate` 立即失效。
- 实测（2000rps/300 并发/30s/variety 200，同 Phase 3b 基线）：

  | 版本 | P50 | P99 | 吞吐 | 失败率 |
  |---|---|---|---|---|
  | Phase 3b（无缓存） | 51ms | 88ms | 1790/s | 0.14% |
  | **Phase 3c（规则缓存）** | **47.4ms** | **80.7ms** | **1795/s** | **0.026%** |

  P50 -7%、P99 -8%，吞吐持平——剩余瓶颈是逐事件串行（dedup 2 读 + 写 1 次 + 业务逻辑），单次规则读已消除。规则表读在缓存命中时 TTL 内 0 次/事件。

### D6：dedup 单读（2 读合并，Phase 3d）

- 原 dedup 每事件 2 读：`GetLastAlert`（last_alerts 唯一索引）+ `Get(la.AlertID)`（回读 alerts 分区表）。`alert_hash` 冗余进 `last_alerts`（与 `status` 并列），写入路径（SaveAlert CTE / UpsertLastAlert）同步维护，迁移启动时 `backfillLastAlertHash` 幂等回填存量行 → `Classify` 只查 last_alerts 唯一索引 **1 次**，完全不再碰 alerts 分区表。
- 实测（2000rps/300 并发/30s/variety 200，同 3b/3c 基线）：

  | 版本 | P50 | P99 | 吞吐 | 失败率 |
  |---|---|---|---|---|
  | Phase 3b（无缓存） | 51ms | 88ms | 1790/s | 0.14% |
  | Phase 3c（规则缓存） | 47.4ms | 80.7ms | 1795/s | 0.026% |
  | **Phase 3d（dedup 单读）** | **33.1ms** | **61.9ms** | **1796/s** | **0.035%** |

  **P50 -30%、P99 -23%**，吞吐持平。收益比规则缓存更大——dedup 是每事件必走的 2 读，而规则读只在命中规则时才有。至此逐事件 DB 往返从 Phase 3b 的 **4 次 → 2 次**（dedup 1 读 + 写 1 次，规则读已缓存）。
- 正确性：last_alerts 为当前状态真源（retention 不清除），alerts 历史行被保留策略删除后仍能正确判 duplicate（旧逻辑会误判 update 反复刷表）。

## 建议（后续规模化）

1. **Linux 部署**：回环连接问题仅 Windows 开发态出现；连接池按实例规格调整。~~生产设置 `WriteTimeout`/`IdleTimeout`~~ **已落地（Phase 3e）**：全局 `ReadTimeout`（默认 15s）/`WriteTimeout`（30s）/`IdleTimeout`（120s），`ReadHeaderTimeout` 固定 5s 防 slowloris。**注意 WS 长连接**：coder/websocket hijack 后不清除 net/http 残留的读写 deadline，需在 WS handler（`hub.ServeWS`）Accept 前用 `http.NewResponseController` 显式清除（详见 PROGRESS.md Phase 3e）。gin debug 模式仍建议生产关闭。
2. **真实告警洪峰**（如 100+ 万/min）：瓶颈在逐事件管线串行（~35ms/事件 ≈ 2 次 DB 往返 + 业务逻辑，dedup 与规则读均已缓存/单读）。~~`SaveAlerts` 批量写~~ **已落地（Phase 3g）**：dedup 批量读 + 单语句批量写合并为每批 2 次往返（批内 overlay 去重，batch20 请求延迟 973→197ms）。剩余方向：规则缓存 TTL 下调（`NewRuleEngine` 构造参数）、dedup 读并入写入语句（write-return-last-alert，进一步 2 往返 → 1 往返，但改动大需评估）
3. ~~**压测脚本化**~~ **已落地（Phase 3f）**：loadgen 新增 `-summary`（JSON 汇总留痕）与 `-compare`（基线对比回归判定：吞吐 <0.8× 或 P50/P99 >1.5× 基线非零退出，profile 须一致）+ 容差可调；Makefile 封装 `make bench` / `make regress` / `make regress-baseline`（`.bench/` 已 gitignore）。首次 `make regress-baseline` 记录当前环境基线（**同环境同轮次比较才有效**），之后 `make regress` 当回归看门狗

### 当前环境复测说明（2026-08-08 Phase 3e 期间）

- Phase 3e 改动为纯配置层（HTTP 超时），理论上零管线回归；为排除性能影响，同环境用 **HEAD 基线（d5dbdb7）** 编译对比压测：基线 P50 **162ms**，改动后 **86–172ms** 波动（多轮），**基线不更快 → 确认无回归**。
- 但两轮均远高于 Phase 3d 记录值（33.1ms）：压测中采样 `pg_stat_activity`，PG 上**全部为 CTE 写入且大量 `LWLock`/`IO` 等待**——瓶颈在 PostgreSQL 写入侧的锁竞争 + Docker Desktop 磁盘 IO，非应用代码。阶段性的 DELETE 清理 + 反复压测会加剧表/索引膨胀（VACUUM 后反而更慢，波动 ±50ms）。
- **结论**：Phase 3d 的 33.1ms 是当时磁盘/表状态下的稳态值；当前 Windows/Docker 环境的绝对数值不可复现。横向对比（同环境同轮次）才是有效判据。
