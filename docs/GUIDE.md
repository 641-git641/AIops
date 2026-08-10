# AIOps 告警平台 — 架构与功能使用指南

> 适用版本：Phase 3g（2026-08-08，commit `285a44a` 之后）
> 本文是系统的**深度文档**：从总体架构到事件全生命周期，到每一类功能的数据流与用法，到存储/性能设计，再到部署运维。快速上手请看 [README](../README.md)，逐阶段开发记录看 [PROGRESS.md](PROGRESS.md)，压测基准看 [PERF.md](PERF.md)。

---

## 目录

1. [总体架构](#1-总体架构)
2. [技术栈与代码布局](#2-技术栈与代码布局)
3. [数据模型](#3-数据模型)
4. [事件全生命周期](#4-事件全生命周期)
5. [两级去重](#5-两级去重)
6. [规则引擎与事故管理](#6-规则引擎与事故管理)
7. [Agent 编排与自愈安全护栏链](#7-agent-编排与自愈安全护栏链)
8. [分级审批与审计](#8-分级审批与审计)
9. [维护窗口](#9-维护窗口)
10. [通知渠道](#10-通知渠道)
11. [AI 聚类/摘要](#11-ai-聚类摘要)
12. [YAML 工作流引擎](#12-yaml-工作流引擎)
13. [服务拓扑与影响面](#13-服务拓扑与影响面)
14. [存储规模化设计](#14-存储规模化设计)
15. [AI 助手（对话式 Agent）](#15-ai-助手对话式-agent)
16. [指标与压测](#16-指标与压测)
17. [部署与运维](#17-部署与运维)
18. [常见问题与踩坑记录](#18-常见问题与踩坑记录)

> 不懂某个术语/缩写？先查 [GLOSSARY.md](GLOSSARY.md)（运维概念与黑话入门，面向新手逐条大白话解释）。

---

## 1. 总体架构

AIops 是一个**双轨架构**的告警平台：一条确定性规则热路径（去重/入库/规则关联 + YAML 工作流）+ 一条 Agent 编排智能路径：

```
                    ┌─────────────── 确定性热路径           ───────────────┐
Alert → process_event ─┤ 归一化→维护窗口→两级去重→入库→CEL 规则关联 Incident ├→ WS 推送 / 审计 / 工作流
                    └─────────────── Agent 智能路径（编排器）──────────────┘
                        monitor → rca → heal → change（安全护栏链门控）
```

**两条路径共享同一进程、同一事件来源，但职责明确分离：**

| 路径 | 职责 | 触发条件 | 性能特征 |
|---|---|---|---|
| **确定性热路径** | 去重、入库、规则关联 Incident、维护窗口静默、WS 实时推送、投递工作流 | 每一条入站告警 | 高吞吐（万级/分钟）、毫秒级、逐事件 2 次 DB 往返 |
| **Agent 智能路径** | Monitor 根因确认 → RCA 根因分析（可选 LLM）→ Heal 自愈 → Change 风险评估 | severity≥high、规则配置 `runAgent`、工作流显式接入 | 低频、异步（队列 worker）、有重试与熔断 |

**设计哲学**：
- **热路径保持确定性与低延迟**——重复告警直接丢弃（duplicate），内容变化刷新（update），命中规则的告警在热路径内聚合为 Incident，不阻塞 webhook 响应。
- **智能路径异步解耦**——`agent.run` 任务经内存队列投递，编排器状态机带检查点（`incident_states`），Agent 步骤可重试，heal 动作被安全护栏链（dry-run → 爆炸半径 → 熔断器 → 分级审批）层层门控。
- **数据以 PostgreSQL 为准**——alerts（全量历史）、last_alerts（当前状态缓存）、incidents、incident_states、rules、playbooks、maintenance_windows、workflows、topology_services/edges 等。

**核心数据流（一次 webhook 请求的完整旅程）：**

```
Prometheus/任意 Provider
   │  POST /api/v1/webhook/:provider
   ▼
provider.Registry 按 kind 取 WebhookParser → 归一化为 []*IngestEvent
   ▼
Pipeline.ProcessEvents
   ├─ Pass1 组装：BuildAlert（算 fingerprint/alert_hash/severity 归一化）
   │           + 维护窗口 IsSuppressed（命中 → status=suppressed）
   ├─ 批量读：GetLastAlerts（按租户一次读齐该批所有指纹）
   ├─ classifyBatchLast：批内 overlay 去重分类（new / update / duplicate）
   │      └─ duplicate → 广播 alert.duplicate，不入库
   ├─ SaveAlerts：单语句双 CTE 批量写（alerts 全量 + last_alerts 折叠 upsert）
   └─ 逐条 finalizeSaved：
         ├─ suppressed → 广播 alert.suppressed + 事件总线
         ├─ rules.Evaluate → 命中则建/更新 Incident（source=rule）→ 广播 incident.upsert
         ├─ WS 广播 alert.new / alert.update
         ├─ shouldRunAgent（severity≥high 或规则 runAgent）→ 投递 agent.run
         └─ workflowTrigger → 投递 workflow.trigger（异步）
   ▼
前端：WS（/api/v1/ws）1 秒内刷新；管理员在 UI/API 审批、查看 RCA/Heal/Change 结果
```

---

## 2. 技术栈与代码布局

| 层 | 技术 |
|---|---|
| 语言/运行时 | Go 1.26 + Gin + GORM |
| 存储 | PostgreSQL（docker-compose 中间件，连接池 20/20 + 启动重试） |
| 实时推送 | coder/websocket 自研 hub（gorilla 式按租户路由广播） |
| 规则引擎 | CEL（google/cel-go，表达式预编译缓存） |
| 前端 | Vite + React 19 + AntD + TanStack Query（WS 驱动缓存刷新） |
| LLM | OpenAI 兼容 Chat Completions（DeepSeek/Ollama/vLLM/LiteLLM，可选增强） |
| 指标 | 自研 Prometheus 文本格式 Registry（无第三方依赖） |

```
cmd/aiops/          装配根（config→store→领域服务→管线→编排器→API→worker）
cmd/loadgen/        压测工具（-rate/-batch/-summary/-compare，Phase 3f/3g）
internal/
  alert/            Alert/LastAlert/IngestEvent 模型 + 仓储 + 指纹/哈希 + 两级去重数据
  pipeline/         事件管线（批量/逐事件双路径）+ 去重分类 + 维护窗口过滤
  incident/         Incident/Rule/State 模型 + CEL 规则引擎（规则缓存）+ 事故服务
  cel/              CEL 引擎封装（预编译缓存）
  orchestrator/     编排器状态机 + agent.run worker + 审批 + 审计/通知
  agent/            Agent 接口 + Monitor/RCA/Heal/Change（确定性实现 + LLM 可选）
  playbook/         自愈剧本（数据驱动 + 默认种子 + 补种逻辑）
  circuit/          熔断器（安全护栏链）
  notifier/         通知渠道（钉钉/飞书/企微/HTTP）+ 生命周期通知 + 渠道配置 DB 化
  maintenance/      维护窗口（Window CRUD + 匹配 + WindowFilter 缓存）
  ai/               AI 聚类/摘要（确定性）+ OpenAI 兼容 LLM 客户端
  workflow/         YAML 工作流引擎（def/match/service + 并发防重 + interval 调度 + 异步执行）
  provider/         数据源抽象 + 注册表（builtin: prometheus/log）
  topology/         服务拓扑 + 归一化依赖边（影响面 SQL 递归 CTE）
  eventbus/         事件总线（内存）+ 审计日志
  queue/            内存任务队列（按类型分发 agent.run / workflow.trigger）
  pusher/           WebSocket hub + 广播
  auth/             API key 认证（sha256 落库）
  api/              Gin 路由 + 中间件 + handlers
  metrics/          指标 Registry（Prometheus 文本）
  retention/        保留策略（行级 DELETE + 月分区 DROP）
  store/            GORM 打开 + 迁移 + alerts 月分区迁移 + 索引幂等补建
  config/ util/
web/               Vite + React 19 + AntD 前端
deploy/            docker-compose（PostgreSQL）+ mock_webhook/mock_llm/mock_echo
```

---

## 3. 数据模型

| 表 | 关键字段 | 约束/设计 | 角色 |
|---|---|---|---|
| `alerts` | id, tenant_id, provider, fingerprint, alert_hash, event(jsonb), name, severity, status, labels(jsonb), started_at, last_seen_at, created_at, updated_at | **月分区表**，PK `(id, created_at)`；无唯一约束（同 fingerprint 允许多条历史） | 全量告警历史（保留策略清理） |
| `last_alerts` | id, tenant_id, fingerprint, alert_id, alert_hash, status, last_seen_at, created_at, updated_at | **唯一索引 `(tenant_id, fingerprint)`**；冗余 `alert_hash`+`status`（去重单读依赖） | 告警当前状态缓存，去重热路径唯一读取点 |
| `incidents` | id, tenant_id, rule_fingerprint, cluster_key, name, ai_generated_name, llm_summary, status, severity, summary, source(rule/agent/ai), merged_into, agent_state_id, created_at, resolved_at | `source` 区分事故来源 | 事故聚合 |
| `incident_states` | id(=incident_id), status(open/investigating/healing/pending_approval/resolved), alert_id, correlation_id, node_results(jsonb), current_agent, retry_count, error_message | 编排器检查点（持久化状态机） | Agent 路径共享上下文 |
| `alert_to_incidents` | alert_id(PK), incident_id(PK), tenant_id | 一个活跃告警至多归属一个 Incident | 告警↔事故关联 |
| `rules` | id, name, fingerprint, cel_expression, grouping_criteria(jsonb), create_on, resolve_on, threshold, run_agent, enabled | CEL 表达式 + 分组字段 | 关联规则 |
| `playbooks` | id, name, level(L0/L1/L2), command_template, blast_radius_factor, rollback_command, dry_run_first, enabled | 唯一 `(tenant_id, name)` | 自愈剧本 |
| `maintenance_windows` | id, name, fingerprint, service, severity, starts_at, ends_at, enabled, reason | 至少一个匹配条件 | 维护窗口静默 |
| `workflows` | id, name, yaml(text), enabled | 原始 YAML 存储，触发时解析 | 工作流定义 |
| `workflow_runs` | id, workflow_id, dedup_key, trigger(alert/interval/manual), status, result(jsonb), error, started_at, finished_at | **唯一 `(tenant_id, workflow_id, dedup_key)`** | 并发防重 + 运行记录 |
| `incident_resolutions` | id, tenant_id, incident_id, confirmed_root_cause, resolution, outcome(success/partial/failed/false_positive), verification, verification_status(verified/unverified/rejected), postmortem_url, resolved_by, resolved_at, version | **唯一 `(tenant_id, incident_id)`**；version 每次更新 +1（知识索引 source_version）；verified 必填根因与处置 | 结案数据契约（RAG 知识来源） |
| `knowledge_documents` | id, tenant_id, source_type(incident/playbook/...), source_id, source_version, title, body, service, severity, trust_level(T0-T3), verification_status, status(active/superseded/deleted/failed/quarantined), valid_from/valid_until, checksum, metadata(jsonb) | **唯一 `(tenant_id, source_type, source_id, source_version)`**；查询只取 active+未过期+非 rejected；GIN(metadata) | 知识源文档（规范化快照，版本演进） |
| `knowledge_chunks` | id, tenant_id, document_id, ordinal, section, content, token_count, content_tsv(tsvector), checksum, metadata(jsonb) | **唯一 `(document_id, ordinal, checksum)`**；GIN(content_tsv) + GIN(content gin_trgm_ops) | 知识切片（语义边界，不截断标识符） |
| `knowledge_embeddings` | id, tenant_id, chunk_id, model_id, index_version, embedding(vector(D)), created_at | **唯一 `(chunk_id, model_id, index_version)`**；向量列/扩展由显式 SQL migration（rag.EnsureSchema）管理 | 向量（模型/维度变更须换 index_version 重建） |
| `knowledge_jobs` | id, tenant_id, operation(upsert/delete/rebuild), source_type, source_id, source_version, status(pending/running/succeeded/failed/dead), attempts, next_run_at, last_error, locked_at/locked_by | **唯一 `(tenant_id, source_type, source_id, source_version, operation)`**；SKIP LOCKED 领取、2min 租约、8 次退避上限进 dead | 持久化索引任务（非内存队列） |
| `rag_traces` | id, tenant_id, incident_id, query_hash, retrieval_version, index_version, candidate_ids/selected_ids(jsonb), latency_ms(jsonb), degraded_reason | 每次相似检索落一条（调试/审计） | 检索轨迹 |
| `rag_feedback` | id, tenant_id, incident_id, trace_id, document_id, rating(useful/not_useful/wrong/unsafe), corrected_root_cause, actor | unsafe → 文档立即 quarantine 退出召回 | 人工反馈（不改索引内容） |
| `topology_services` | id, name, kind, depends_on(jsonb), recent_changes(jsonb), impact_score | 服务拓扑节点 | RCA 事实来源 |
| `topology_edges` | id, service, depends_on, tenant_id | 唯一 `(service, depends_on)`；依赖边归一化投影 | 影响面 SQL 递归 CTE |
| `notification_channels` | name, enabled, webhook, ... | 渠道配置以 DB 为准（env 仅首启种子） | 通知渠道管理 |
| `audit_logs` | actor, action, target, reason, metadata(jsonb), correlation_id | who/when/what/why | 审计轨迹 |
| `api_keys` | name, secret_hash | 明文仅创建时返回一次 | API key 认证 |
| `assistant_threads` | id, tenant_id, title, status(active/deleted), context_type/context_id | 会话（软删；context 绑定 Phase 2） | 对话式 Agent（AGENT_CHAT_PLAN） |
| `assistant_messages` | id, tenant_id, thread_id, run_id, role(user/assistant/tool), content, sequence, citations(jsonb) | **唯一 `(tenant_id, thread_id, sequence)`** | 会话消息（仅最终 assistant 消息作为后续上下文） |
| `assistant_runs` | id, tenant_id, thread_id, status(queued/running/completed/failed/cancelled), model, prompt_version, idempotency_key, attempts, next_run_at, locked_at/locked_by | **唯一 `(tenant_id, idempotency_key)`**（幂等防重）；SKIP LOCKED 领取、2min 租约、8 次退避上限 | run 状态机（对齐 knowledge_jobs 租约模式） |
| `assistant_steps` | id, run_id, ordinal, kind(model/tool_call/tool_result/policy), tool_name, arguments/result_summary(jsonb), status, duration_ms | 步骤可回放/审计；只存裁剪摘要 | 运行步骤 |
| `assistant_events` | id, run_id, sequence, type(run.*/message.delta/tool.*/heartbeat), data(jsonb) | **唯一 `(run_id, sequence)`**；SQL 原子分配 | SSE 事件持久化（断线按 lastSequence 续传） |
| `assistant_action_requests` | id, run_id, tool_name, arguments(jsonb), risk_level, status, idempotency_key | 本期仅建 schema（Phase 3 动作确认启用） | 需人工确认的动作请求 |

**两个关键冗余设计**（都是为了去重热路径单读）：
1. `last_alerts.alert_hash` + `status`：Phase 3d 把内容哈希与状态冗余进当前状态缓存，`Classify` 只查 `(tenant_id, fingerprint)` 唯一索引 **1 次**即可分类（不再回读 alerts 分区表）。
2. `topology_edges`：把 `topology_services.depends_on`（JSONB）投影成关系表，影响面查询在 PG 侧用 `WITH RECURSIVE` 完成（百万节点内不依赖整图加载；Neo4j 后置仅替换该查询）。

---

## 4. 事件全生命周期

### 4.1 归一化（Provider 层）

`POST /api/v1/webhook/:provider` → `provider.Registry.MustWebhook(kind)` 取解析器 → `Parse(body)` 产出 `[]*IngestEvent`。内置 provider：`prometheus`（Alertmanager 格式）、`grafana`（Grafana Alerting webhook）、`log`（演示）。事件以当前租户覆盖 provider 默认租户（多租户贯穿）。

**接入配置示例**：
- Prometheus/Alertmanager：`alertmanager.yml` 的 `receivers[].webhook_configs[].url = http://<host>:8080/api/v1/webhook/prometheus`（`send_resolved: true` 可收恢复事件）。
- Grafana：Alerting → Contact points → 新建 webhook 联系人，URL 填 `http://<host>:8080/api/v1/webhook/grafana`；Grafana Unified Alerting 负载的 labels/annotations 归一化同 Alertmanager 语义，`generatorURL/fingerprint/values` 存入告警 Event 详情。
- 大模型 API：Chat LLM 走前端「模型配置」页（`model_configs` 表，apiKey 只入库不回显；env 的 `AIOPS_LLM*` 仅首启种子）；**Embedding 无 UI/DB 入口**——由 `AIOPS_EMBEDDING*` env 直接配置（示例见 README 配置表千问块）。均**保存/修改后重启后端生效**；管理端点需 `AIOPS_MODELCFGADMINAPIS=true`（默认 403）。

`IngestEvent` 关键字段：`TenantID / Provider / Name / Severity / Status / Labels / Event / StartedAt`。

### 4.2 BuildAlert（指纹 + 哈希）

`BuildAlert` 计算：
- **Fingerprint（逻辑键）**：`SHA256(provider, name, 参与指纹的 labels)`。排除易变字段 `{timestamp, startsAt, endsAt, instance, job}`，其余标签键排序后哈希。→ 同名同源告警视为**同一逻辑告警**。
- **AlertHash（物理键）**：`ContentHash(event)`——完整 event 快照的稳定哈希。→ 内容完全相同 = 完全重复。

同时做严重度/状态归一化：
- severity：`critical/high/medium/low/info`；`error→high`、`warning/warn→medium`、`p0/severe→critical`、`notice→info`、未知→`medium`。
- status：`firing/active/triggered/open/pending→firing`；`resolved/ok/recovered/closed→resolved`；`acknowledged→acknowledged`；`suppressed→suppressed`；未知→`firing`。

### 4.3 维护窗口静默（热路径第 1 道闸）

`maintenance.IsSuppressed(ctx, a)` 查当前生效窗口（`WindowFilter` 缓存 TTL 5s，锁外查库 + double-check）：
- **仅 firing 告警可被静默**（resolved 一律放行，保证恢复链路不被吞）。
- 窗口至少配置一个匹配条件（fingerprint / service / severity），所有已配置条件必须全部满足；`severity` 是「覆盖 ≤ 该严重度」。
- 命中 → `a.Status = StatusSuppressed`，入库为 suppressed，跳过规则/Incident/Agent。

### 4.4 去重分类（热路径第 2 道闸）

见[第 5 节](#5-两级去重)。分类结果：
- `duplicate` → 广播 `alert.duplicate`，不入库不触发。
- `new` / `update` → 进写入列表。

### 4.5 入库（批量写 / 单事件写）

- **批量快路径（Phase 3g）**：`SaveAlerts` 单语句双 CTE（`WITH ins AS (INSERT alerts ... RETURNING), ups AS (INSERT last_alerts ... ON CONFLICT DO UPDATE) SELECT 1`）——alerts 写全量历史行，last_alerts 在 **Go 侧折叠**（每 `(tenant_id, fingerprint)` 保留批内最后一条）后 upsert。一次往返写完整批。
- **逐事件路径**：批量读失败自动降级为逐条 `ProcessEvent`（未写任何行，安全）；批量写失败逐条 `SaveAlert` 降级（单语句原子，失败未写任何行）。

### 4.6 逐条副作用（finalizeSaved）

入库成功后逐事件执行（不阻塞批量写）：
1. `suppressed` → `aiops_alerts_suppressed_total` + 广播 `alert.suppressed` + 事件总线。
2. 规则引擎 `rules.Evaluate` → 命中建/更新 `source=rule` Incident → 广播 `incident.upsert`。
3. WS 广播 `alert.new` / `alert.update`。
4. `shouldRunAgent(a, inc)`：severity≥high **或**（inc 非空且 `inc.Source=="rule"`）→ 投递 `agent.run` 队列任务。
5. `workflowTrigger` → 投递 `workflow.trigger` 队列任务（工作流引擎异步消费）。

### 4.7 前端实时刷新

前端 `useRealtime` 连 `ws://host/api/v1/ws`，收到 `alert.*` 刷新告警列表、`incident.*` 刷新事故列表；断线 3s 自动重连。

---

## 5. 两级去重

```
Deduper.Classify / classifyBatchLast
  ├─ last_alerts 无该 (tenant, fingerprint)            → new
  ├─ 有：alert_hash 相同
  │     └─ status == firing（当前活跃）                → duplicate（完全重复，丢弃）
  │     └─ status == suppressed && 窗口生效中            → duplicate（窗口期心跳不刷表）
  └─ 有：alert_hash 不同（状态/内容变化）               → update（刷新当前状态 + 新历史行）
```

**核心语义**：`last_alerts` 是**当前状态真源**（保留策略不清理它）。alerts 历史行可能被保留策略删除，但当前状态缓存仍准确 → firing + 同内容判 duplicate 不再误判 update。

**窗口期边界**：suppressed 告警在窗口内同内容复发算 duplicate（避免心跳刷表）；窗口失效后 suppressed 是过期状态，同内容复发重新上浮为 firing（`windowActive=false` → 非 duplicate）。

**批量路径的 overlay 正确性（Phase 3g 关键设计）**：
`classifyBatchLast(base, candidates)` 以 DB 批量读到的存量 `base` 为底，叠加**批内 overlay**（前序事件的写入意图）逐条分类——同批同 fingerprint 的后序事件必须看到前序状态（等价逐事件读 DB），**禁止裸延迟 flush**。`overlay[tenant+fp] = &LastAlert{AlertHash, Status}` 在每条 non-duplicate 事件后更新。纯函数、无 DB，可单测。

---

## 6. 规则引擎与事故管理

### 6.1 CEL 规则

规则 `rules` 表：`CELExpression`（匹配表达式）+ `GroupingCriteria`（分组字段）+ `Threshold`（分组内命中次数 ≥ N 才建 Incident）+ `RunAgent`（命中后是否进入 Agent）+ `Enabled`。

CEL 求值上下文（三个变量，均 map）：
```
alert: {id, provider, name, severity, status, labels}
labels: {service, host, ...}   ← 告警标签
event:  原始 event 快照
```

示例规则（`POST /api/v1/rules`）：
```json
{
  "name": "pay critical",
  "celExpression": "alert.severity == 'critical' && labels.service == 'pay'",
  "groupingCriteria": ["provider", "labels.host"],
  "threshold": 1,
  "runAgent": true,
  "enabled": true
}
```

**规则缓存（Phase 3c）**：`RuleEngine` 按租户缓存启用规则（TTL 5s，RWMutex + 锁外查库 + double-check）；`POST /rules` 后自动 `Invalidate(tenantID)` 立即失效。

### 6.2 事故聚合

- 规则命中：按 `GroupingCriteria` 生成分组指纹（≈ `rule_fingerprint`），同指纹未解决 Incident 合并（更新名称/摘要/关联告警），否则新建 `source=rule`。
- 事故状态机（`incident_states`）：`open → investigating → healing → pending_approval → resolved`。
- 一个活跃告警至多归属一个 Incident（`alert_to_incidents`）。

### 6.3 事故 API

- `GET /incidents?status=&limit=&offset=` 列表；`GET /incidents/:id` 详情（聚合关联告警 + `state` + `nodeResults`）。
- `POST /incidents/:id/resolve` 手动解决；`POST /incidents/:id/approval` 分级审批。

---

## 7. Agent 编排与自愈安全护栏链

### 7.1 编排状态机

`orchestrator.Orchestrator` 持有节点表（顺序执行，`MaxRetries`）：

```
monitor → rca → heal → change
   │        │      │      └─ 对 heal 提议的自愈动作做 5 维风险评分
   │        │      └─ 制定自愈动作（安全护栏链门控）
   │        └─ 根因分析（贝叶斯证据链 + 可选 LLM 补充）
   └─ 告警上下文确认（是否真的出问题）
```

- 每个节点结果写入 `incident_states.node_results[node]`（**检查点持久化**，进程重启可续）。
- `correlation_id` 贯穿 alert→rca→heal→change→审计。
- 队列任务 `agent.run` → `HandleAgentTask` → `ensureState`（按 incident 找/建状态）→ `Run` 状态机。

### 7.2 Heal 安全护栏链（自愈不失控的层层防线）

```
建议动作（RCA suggestedActions + LLM llmSuggestedActions 去重合并）
   → playbook 匹配（精确 name 优先，否则第一个可用 L0-L2 剧本；未配置自动补种默认剧本）
   → dry-run（AIOPS_AGENTDRYRUN=true 默认，只产出动作不执行）
   → 爆炸半径（动作固有 blast_radius_factor 叠加依赖链；> 0.2 强制降级 L2）
   → 熔断器（5 次失败开启 60s 阻断自愈）
   → 分级审批门控：L0 自动（需 dry_run=false）/ L1 oncall 确认 / L2 人工审批
```

### 7.3 Change 风险评估

对 heal 提议的自愈动作做 5 维风险评分（`riskScore` / `level` / `breakdown`）：动作固有爆炸半径、依赖链、历史变更等。输出 `node_results.change`，辅助审批决策。

### 7.4 LLM 增强（可选，fail-open）

`AIOPS_LLMAPIKEY` 配置后（openai-compat）：
- **RCA**：`EnhanceRCA` 基于确定性证据链生成自然语言根因 → `node_results.rca.llmRootCause/llmRationale/llmSuggestedActions`。
- **Heal 回填**：`suggestedActions = 确定性 suggestedActions + llmSuggestedActions`（去重、顺序保留）——建与 LLM 建议同名的剧本即可命中。
- 未配置 key / LLM 不可达 / 输出无法解析 → 保持纯确定性结论，**不阻塞编排**（fail-open）。

---

## 8. 分级审批与审计

### 8.1 审批闭环

`POST /incidents/:id/approval`：
```json
{"decision": "approve", "actor": "oncall-engineer", "reason": "批准回滚"}
```
- `approve` → 编排器执行自愈动作 + 事故闭环 `resolved`（走 `ResolveSilent`，避免与「审批通过」通知重复）。
- `reject` → 事故保持 `open`（人工跟进），摘要通过 `UpdateNameSummary` append（带换行，勿直接覆盖）。

### 8.2 审计轨迹

`audit_logs` 记录四元组 **who/when/what/why** + `correlation_id` 贯穿。action 约定：`approval.granted/rejected`、`heal.executed`、`notify.sent/failed/skipped`、`incident.pending_approval`。`GET /incidents/:id/audits` 查询。

### 8.3 通知联动

审批进入 `pending_approval`、通过、拒绝都推渠道 + 落审计（`notify.sent/failed/skipped`）；未配置 webhook 的渠道自动跳过（fail-open，不影响主流程）。**事故新建（rule/agent/AI 任一来源）与事故解决**也推生命周期通知（`incident.created` / `incident.resolved`）。

---

## 9. 维护窗口

- **静默语义**：窗口命中 → firing 告警以 `suppressed` 入库，跳过规则/Incident/Agent。resolved 一律放行。
- **匹配规则**：至少一个条件（fingerprint 精确 / service 精确 / severity「覆盖 ≤」），全部满足才命中；窗口 `enabled` 且 `now ∈ [starts, ends)`。
- **缓存**：`WindowFilter` TTL 5s（锁外查库 + double-check）；窗口 CRUD 后 API 层自动 `pipe.InvalidateMaintenance(tenant)`。
- **UI**：「维护窗口」页管理（建/改/删/启停）。

```bash
curl -X POST localhost:8080/api/v1/maintenance-windows -H 'Content-Type: application/json' \
  -d '{"name":"pay 周三维护","service":"pay","severity":"high","startsAt":"2026-08-12T00:00:00Z","endsAt":"2026-08-12T01:00:00Z","reason":"DB 迁移","enabled":true}'
```

---

## 10. 通知渠道

- **渠道**：钉钉 / 飞书 / 企微 / 通用 HTTP webhook。`notifier.Service` 线程安全（先快照渠道引用再释放锁发送，勿持锁调取锁方法）。
- **配置 DB 化**：`notification_channels` 表为准；env（`AIOPS_NOTIFIERCHANNELS` + 各渠道 webhook）仅作首启种子（`EnsureDefaults` 只插不覆盖），UI 保存即 `Reload` 热生效，重启不回退。
- **生命周期通知**：`incident.Service.SetNotifyFunc` 注入 `LifecycleNotifier.Notify`（签名 `func(ctx, *Incident, action)`），`Create`/`Resolve` 触发 `incident.created/resolved`。
- **测试**：`POST /notify/test` 全渠道；`POST /notify/channels/:name/test` 单渠道（无需启用）。本地演练用 `AIOPS_NOTIFIERCHANNELS=http` + mock webhook `node deploy/mock_webhook.mjs 9999`。

---

## 11. AI 聚类/摘要

- **背景 worker**（默认 60s 间隔，启动即跑一次）扫描**未归属** firing 告警（`id NOT IN alert_to_incidents`，不抢规则/Agent 已承接的告警）。
- **确定性聚类**：按 `labels.service` 分组 + 时间窗（`AIClusterWindowMin`，组内断开间隔 > window 断开）→ 组内达到 `AIClusterThreshold`（默认 3）确认生成 `source=ai` Incident（同 `cluster_key` 合并、名称/摘要随成员刷新）。
- **LLM 摘要（可选）**：`AIOPS_LLMAPIKEY` 后 `EnhanceCluster` 生成自然语言摘要写入 `llmSummary`（与确定性 `summary` 并存；失败保留确定性、不阻塞建事故）。
- 手动触发：`POST /api/v1/ai/cluster` → `{scanned, clusters, created[], merged}`。

---

## 12. YAML 工作流引擎

「定义即配置」：用户以 YAML 声明 `triggers` + `steps[]`/`actions[]`，命中告警时**异步**执行。

### 12.1 结构

```yaml
workflow:
  id: order-medium-agent-demo
  name: 订单中危告警自动接入 Agent 编排
  triggers:
    - type: alert                 # alert | interval | manual
      filters:                    # key: severity|service|name|provider|任意 labels 键
        - key: service
          value: order            # value 精确匹配；severity 支持 >=/<=
        - key: severity
          value: ">=medium"
  steps:                          # 先执行（富化/记录）
    - name: enrich
      provider: { type: log, with: { message: "命中 {{.alert.name}}" } }
  actions:                        # 后执行（处置）
    - name: run-agent
      provider: { type: agent }   # provider: agent 复用编排器接口
```

- **Provider 类型**：`log`（记录）、`http`（`with.method/headers/auth`，值支持 `{{.alert.*}}` 模板）、`agent`（接入 Agent 编排器）。
- **Trigger**：`alert`（filters 全部满足才命中）、`interval`（`with.interval`：Go duration "5m" 或纯秒）、`manual`（仅 API/UI「运行」按钮）。

### 12.2 并发防重（验收④）

`workflow_runs` 唯一索引 `(tenant_id, workflow_id, dedup_key)`，`dedup_key = SHA256(workflow_id : alert.fingerprint)`（alert 触发）；并发首达仅一个赢者，同一工作流对同一告警只执行一次。interval/manual 的 dedup 键每次唯一（不互相防重，便于反复演练）。

### 12.3 执行语义

- **异步解耦**：`launch` 落库后立即返回，`execute` 在独立 goroutine 跑（`execSem` 有界，`AIOPS_WORKFLOWEXECWORKERS` 默认 8）——慢步骤不阻塞队列 worker / agent.run；panic 转 failed、10min 超时兜底。
- 结果落 `WorkflowRun.Result`（`map[stepName]any`）；运行记录 `trigger` 列区分来源。
- `AIOPS_WORKFLOWENABLED=false` 关闭 pipeline 投递。

---

## 13. 服务拓扑与影响面

- **拓扑节点**：`POST /api/v1/topology` 创建 `{name, kind, dependsOn[], recentChanges[]}`；启动时 `SyncAllEdges` 把 JSONB 依赖投影到 `topology_edges`（幂等：先删后插）。
- **影响面查询**：`GET /api/v1/topology/:name/impact?depth=N` → SQL `WITH RECURSIVE` 反向遍历依赖图（谁依赖我），返回带深度/路径/防环的结果。
- **RCA 事实来源**：拓扑 `depends_on` / `recent_changes` 是 RCA 确定性证据链的输入（服务、依赖链、近期变更）。
- **扩展点**：Neo4j 后置时仅替换该查询实现，模型与 API 不变。

---

## 14. 存储规模化设计

### 14.1 alerts 月分区（Phase 3b）

- 表：`alerts_p202608 / alerts_p202609 / alerts_default`（兜底），PK `(id, created_at)` RANGE 分区。
- 迁移（启动 AutoMigrate 后原地执行，事务幂等）：RENAME → 建父表 → 存量覆盖分区 + default → 分批搬迁 → DROP legacy；已分区仅维护当月/次月 + default。
- **踩坑**：AutoMigrate 先建索引于普通表，RENAME/DROP 被带走 → 迁移后 `ensureAlertIndexes` 幂等补建（父表建 → PG 自动传播到分区）；default 分区已有数据时补建历史分区报 constraint 错误（先清数据再建）。

### 14.2 保留策略（Phase 3b）

- worker（默认 60min，启动即清一次）：alerts 30d / audit 180d / workflow_runs 90d，分批删除（5000/批循环）。
- **分区 DROP**：整月过期分区直接 `DROP PARTITION`（零 WAL、无锁表），分区名正则 `^alerts_p\d{6}$` 白名单（`alerts_default` 永不 drop）+ 当月残余行级 DELETE 兜底。
- `AIOPS_ALERTSPARTITIONS=false` 退回纯行级删除（不提供反向迁移）。

### 14.3 写入优化演进（Phase 3b→3g）

| 阶段 | 每事件 DB 往返 | 关键改动 |
|---|---|---|
| Phase 3b 前 | 4 次（BEGIN/INSERT/UPSERT/COMMIT） | — |
| Phase 3b | 1 次 | `SaveAlert` 单语句双表写入（CTE） |
| Phase 3c | 逐事件再减 1 | 规则引擎规则缓存（TTL 内 0 次规则读） |
| Phase 3d | 2 次（dedup 1 读 + 写 1 次） | `alert_hash` 冗余进 `last_alerts`，去重单读 |
| **Phase 3g** | **每批 2 次** | `SaveAlerts` 批量读 + 单语句多行 CTE 批量写（批内 overlay 去重） |

**批量写关键正确性**：单语句多行 CTE 的 last_alerts `ON CONFLICT DO UPDATE` 若两行同 `(tenant_id, fingerprint)` 会触发 PG「cannot affect row a second time」→ last_alerts 在 **Go 侧折叠**（每 fingerprint 保留批内最后一条，`lastIdx` map + `sort.Ints` 保证 VALUES 与 args 同序），alerts 仍写全部历史行。批量写分块 `batchWriteChunk=100`（每块 ≤ 100×15+100×9=2400 参数，远低于 PG 65535 上限）。

---

## 15. AI 助手（对话式 Agent，AGENT_CHAT_PLAN Phase 0+1 只读 MVP）

> 设计草案与阶段计划见根目录 `AGENT_CHAT_PLAN.md`；本阶段 = 只读运维副驾驶（无动作工具、无 RAG 引用）。

### 15.1 功能范围

- **会话**：新建/列表/软删；发送消息要求 `Idempotency-Key` 头（幂等防双击/断线重放）。
- **Agent loop**：独立 DB 租约 worker（不复用告警内存队列）领取 run → 多轮「模型 → 工具调用 → 工具结果回喂」→ 最终回答落库。
- **只读工具**（基础 6 个 + 知识 3 个，全部参数二次校验 + 结果裁剪 + 审计）：`get_platform_overview` / `list_alerts` / `get_alert` / `list_incidents` / `get_incident`（含 monitor/rca/heal/change 四节点摘要）/ `get_topology_impact`；**知识类**（Phase 2，RAG 关闭时不注册）：`search_knowledge`（知识库通用检索）/ `get_similar_incidents`（相似历史事故，排除自身）/ `list_playbooks`（剧本元数据）。
- **引用与反馈**（Phase 2）：检索类工具命中证据 → `citation.added` 事件实时推送 + 最终回答消息落 `citations` 字段；前端引用卡可点跳转（事故/剧本/知识）；回答下方「有用/没用」反馈（`POST /messages/:id/feedback`）。
- **上下文绑定**（Phase 2）：会话可绑定事故（`contextType: "incident"`）；事故详情页「在 AI 助手中分析」一键进入；run 自动注入绑定事故的确定性事实摘要；超长历史自动裁剪并生成早期对话摘要。
- **流式**：`GET /runs/:id/events` 是**全仓第一个出站 SSE**（快照续传 + 实时订阅 + 15s 心跳）；SSE handler 内清除底层 deadline（全局 WriteTimeout=30s 不豁免——对齐 WS 先例）。
- **硬限制**：单 run ≤8 模型回合 / ≤12 工具调用 / 60s 总超时 / 3s 工具超时；未知工具与非法参数**拒绝执行**并继续（不 panic、不终止 run）。
- **安全**：工具结果与知识内容视为不可信数据（防提示注入，疑似密钥的查询不送向量服务）；run/步骤/事件全 tenant 强制；审计 `assistant.*` 动作（Actor 本期 `local-noauth`）。

### 15.2 启用与配置

```bash
# mock 联调（无需 key）
node deploy/mock_llm.mjs 9999
AIOPS_ASSISTANTENABLED=true \
AIOPS_ASSISTANTCHATBASEURL=http://localhost:9998 \
AIOPS_ASSISTANTCHATAPIKEY=dummy AIOPS_ASSISTANTCHATMODEL=mock \
go run ./cmd/aiops
```

- 生成模型要求 OpenAI 兼容且支持 tool calling（qwen-plus / deepseek-chat 均可）；模型配置页「Assistant Chat」行可管理（env 仅首启种子）。
- 未配置模型时服务照常启动，run 快速失败 `model_not_configured`（不重试）；`AIOPS_ASSISTANTENABLED=false` 时全部 `/assistant` 端点 404，现有路径零变化。
- **知识类工具需要 RAG 开启**（`AIOPS_RAGENABLED=true` + embedding 配置，见 §14）：RAG 关闭时 `search_knowledge`/`get_similar_incidents` 不注册（工具列表不出现），聊天仍可用领域只读工具；向量路失败自动关键词降级，回答与工具卡均透出降级原因。
- 数据表 7 张（threads/messages/runs/steps/events/action_requests + **feedback**，见 §3 数据模型表），`assistant_events` 的 sequence 由 SQL 原子分配（重启安全）。

### 15.3 指标（`/metrics`）

`aiops_assistant_runs_total{status,model}`、`aiops_assistant_run_duration_seconds`、`aiops_assistant_model_duration_seconds`、`aiops_assistant_tool_calls_total{tool,outcome}`、`aiops_assistant_tool_duration_seconds`、`aiops_assistant_active_runs`、`aiops_assistant_tokens_total{direction,model}`；Phase 2 新增 `aiops_assistant_citations_total{source_type}`（回答引用）与 `aiops_assistant_feedback_total{rating}`（消息反馈）。

---

## 16. 指标与压测

### 16.1 Prometheus 指标（`GET /metrics`）

| 指标 | 类型 | 说明 |
|---|---|---|
| `aiops_pipeline_duration_seconds` | 直方图 | 单条告警管线处理耗时（含 `_bucket{le="+Inf"}`） |
| `aiops_alerts_total{kind}` | 计数 | 入库告警数（new/update） |
| `aiops_alerts_duplicated_total` | 计数 | 完全重复被丢弃数 |
| `aiops_alerts_suppressed_total` | 计数 | 维护窗口静默数 |
| `aiops_incidents_total{source}` | 计数 | 新建事故数（rule/agent/ai） |
| `aiops_queue_depth` | 仪表 | 任务队列深度（agent.run + workflow.trigger） |

### 16.2 loadgen 压测工具（cmd/loadgen）

```
go run ./cmd/loadgen -baseurl http://127.0.0.1:8080 -rate 2000 -duration 30s \
  -concurrency 300 -variety 200 [-batch 20] [-severity medium] \
  -summary .bench/latest.json [-compare .bench/baseline.json]
```

- `-rate` 恒为事件/秒；`-batch N` 每条请求带 N 条事件（请求速率 = rate/batch）。
- `-variety N` 决定去重重复比例（告警名轮换数，重复率 ≈ 1-1/N）。
- `-summary` 写出机器可读 JSON 汇总（含 profile：rate/duration/concurrency/variety/batch/severity + 吞吐/P50-P95/P99/失败率）。
- `-compare <baseline>` 回归判定：吞吐 < 0.8× 或 P50/P99 > 1.5× 基线非零退出（`-tol-throughput` / `-tol-latency` 可调）；**profile 与基线不一致直接判失败**。

### 16.3 基准结果摘要（详见 PERF.md）

| 场景 | 结果 |
|---|---|
| 万级验收（加固后） | 1786/s（≈10.7 万/min）、失败率 0.12%、P50 61ms、P99 100ms ✅ |
| Phase 3b→3g 逐事件优化 | 每事件 DB 往返 4→2 次，P50 61→33.1ms（Phase 3d 稳态值） |
| **Phase 3g 批量写** | batch20 请求延迟 **973→197ms（-80%）**、失败率 5%→0%、吞吐 1774/s |

---

## 17. 部署与运维

### 16.1 本地启动

```bash
make infra   # docker compose -f deploy/docker-compose.yml up -d postgres
make app     # go run ./cmd/aiops（:8080）
make web     # cd web && npm run dev（:5173）
```

后端启动装配顺序（main.go）：config → store(PostgreSQL，含分区迁移 + 索引补建 + last_alerts hash 回填) → 领域服务 → 事件管线 → 编排器 → 工作流 → API → 后台 worker（队列消费 / workflow.trigger / interval 调度 / AI 聚类 / 保留策略）。

### 16.2 认证

`AIOPS_AUTHTYPE=apikey`：`POST /api/v1/apikeys` 创建 key（明文仅返回一次）→ 后续请求带 `X-API-Key` 头，服务端 sha256 校验后注入租户。`noauth`（默认）固定 `default` 租户（本地/开发）。

### 16.3 HTTP 超时（生产规格）

- `ReadTimeout 15s / WriteTimeout 30s / IdleTimeout 120s`，`ReadHeaderTimeout` 固定 5s 防 slowloris。
- **WS 长连接特殊处理**：coder/websocket hijack 后不清除 net/http 落在底层连接上的读写 deadline → `hub.ServeWS` Accept 前用 `http.NewResponseController(w)` 显式清除 `SetReadDeadline/SetWriteDeadline(time.Time{})`（否则全局超时秒数后 WS 被强制断开）。

### 16.4 常用命令速查

| 命令 | 作用 |
|---|---|
| `make bench` / `make regress` / `make regress-baseline` | 压测留痕 / 回归判定 / 记基线 |
| `make bench-batch` / `make regress-batch` / `make regress-baseline-batch` | 批量路径同族（`BENCH_BATCH=20`） |
| `make test` / `make vet` / `make tidy` | 单测 / 静态检查 / 整理依赖 |
| `node deploy/mock_webhook.mjs 9999` | 本地 mock 通知 webhook |
| `node deploy/mock_llm.mjs 9998 [fail]` | 本地 mock LLM（SSE 流式，区分聚类/RCA） |
| `node deploy/mock_echo.mjs 9996` | 本地回显（工作流 http step 调试） |

---

## 18. 常见问题与踩坑记录

**去重与状态**
- `last_alerts` 是当前状态真源（保留策略不清除）；alerts 历史行被删后仍正确判 duplicate。
- 窗口期内 suppressed 同内容告警按 duplicate 处理（避免心跳刷表）；窗口失效后同内容复发重新上浮为 firing。
- 指纹排除 `{timestamp, startsAt, endsAt, instance, job}`——这些标签变化不应产生新逻辑告警。

**规则/事故**
- 规则 CRUD 后热路径缓存最长 5s 内生效（`Invalidate` 自动调用）；维护窗口 CRUD 后同理（`InvalidateMaintenance`）。
- 审批 reject 时 incident 保持 open，摘要通过 `UpdateNameSummary` append，勿直接覆盖。
- 审批闭环解决事故必须走 `ResolveSilent`（approve 已有「审批通过」通知，避免双条）。

**工作流**
- `workflow_runs` 唯一索引 `uk_wfrun_tenant_workflow_key` **勿移除**（alert 触发防重）；interval/manual 的 dedup 键每次唯一不冲突。
- 工作流 execute **勿改回同步**（慢步骤会阻塞队列 worker 与后续 agent.run）。
- interval/manual 无告警运行用 agent 步骤会明确报错（预期）。
- `provider: agent` 复用 `AgentRunner` 接口注入；`orch == nil` 时 agent 步骤报错不崩溃。

**通知**
- `notifier.Reload` 会清空未启用/空 webhook 渠道；停用渠道即静默（勿以为丢渠道）。
- 通知同步发送（事故创建/解决频率低可接受；勿把通知链路改成每告警同步调）。

**LLM**
- 门控 `LLMDriver==openai-compat && LLMAPIKey!=""`；LLM 增强是**可选补充**，永远不改变确定性结论。
- `llm*` 前缀字段与 Incident `llmSummary` 勿改名（前端按它渲染）。

**存储/分区**
- alerts 模型 PK 标签仍是单 `id`（实际 DB 是 `(id, created_at)`）；GORM diff 只补缺不删多，勿手动改回。
- default 分区已有数据时补建历史分区报 constraint 错误（先清数据再建）。
- `save_sql.go` 列清单是写入列的唯一事实来源（新增模型字段需同步）。
- 测试里熔断器冷却期必须 `60*time.Second` 而非 `60`（ns 会时序 flake）。

**压测**
- loadgen 必须保持 keep-alive 连接复用（读完 body 再 Close + 共享 transport），否则 Windows 回环 TIME_WAIT 爆炸、误读为服务端故障。
- **同环境同轮次横向对比才是有效判据**；Phase 3d 的 33.1ms 是当时磁盘/表状态的稳态值，当前 Windows/Docker 环境绝对数值不可复现。

---

*本文与代码同步维护；新增功能时同步更新 [README](../README.md) / [PROGRESS.md](PROGRESS.md) / [PERF.md](PERF.md) / 本文。*
