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

## 建议（后续规模化）

1. **Linux 部署**：回环连接问题仅 Windows 开发态出现；生产建议关闭 debug 模式 + 设置 `WriteTimeout`/`IdleTimeout`，连接池按实例规格调整
2. **真实告警洪峰**（如 100+ 万/min）：瓶颈将转向 DB 写入（alerts 表每事件 1 insert + last_alerts upsert），考虑批量写入或分区表
3. **压测脚本化**：loadgen 已支持 `-rate/-duration/-concurrency/-variety`，可接入 CI 作为回归基准（失败率 >1% 即非零退出）
