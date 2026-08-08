# AIOps 告警平台 — 项目进度交接文档

> 用途：跨会话/换窗口继续开发的现状快照。任何新窗口先读本文件 + README.md。
> 最后更新：2026-08-08（Phase 3c 规则缓存 `f7884fb`；Phase 3d dedup 单读完成，P50 -30%/P99 -23%；Phase 3e 生产 HTTP 超时调优完成）

---

## 0. 项目定位与一句话架构

融合 **keephq/keep**（告警平台：Provider 归一化 + 确定性管线 + YAML 工作流）与
**bcefghj/multi-agent-aiops**（多 Agent 智能运维：编排器状态机 + 分级自愈 + 安全护栏）。

**双轨架构**：

```
Alert → process_event ──┬── 确定性热路径（去重 → 入库 → CEL 规则 → Incident → WS）
                        └── Agent 智能路径（编排器 monitor → rca → heal → change，IncidentState 流转）
```

- 热路径处理可确定性判断的：维护窗口/去重/规则关联/**YAML 工作流（keep 式，provider: agent 可切入 Agent 路径）**。
- Agent 路径处理需推理的：根因分析、自愈动作、变更风险，**决策不绕过审批**。
- 背景 worker：AI 聚类/摘要兜底未被两条路径承接的散落告警（低/中危），生成 `source=ai` incident。
- 两条路径共享：Incident 模型、事件总线（审计）、知识层（拓扑）、执行层（provider 反向复用）。

技术栈：**Go 1.26 + Gin + GORM + PostgreSQL（docker-compose）+ google/cel-go**；前端 **Vite + React 19 + AntD + TanStack Query + zustand**。

---

## 1. 已完成阶段与验收

### Phase 1 — MVP 管线 ✅
- 事件管线（两级去重 / 入库 / CEL 规则关联 / WS 广播）、webhook + prometheus provider、API-key 认证、
  alerts/incidents 列表详情页、2 容器 compose + PostgreSQL、编排器骨架 + Monitor/RCA Agent 雏形。

### Phase 2a — 自愈 + 分级审批闭环 ✅
- **HealAgent**：RCA 建议动作 → Playbook 匹配 → 爆炸半径（剧本因子 + 拓扑影响放大）→ 超限强制降级 L2 → dry-run/审批门控。
- **ChangeAgent**：5 维加权风险评分（爆炸 0.30 / 操作 0.25 / 时间 0.20 / 历史 0.15 / 关键 0.10），分级 L0≤0.3 / L1≤0.5 / L2。
- **安全护栏链**：dry-run（默认 true）→ 爆炸半径 → 熔断器（5 次失败 → 60s OPEN）→ L0/L1/L2 分级审批。
- **审批闭环**：`POST /incidents/:id/approval`（approve→resolved+heal.executed / reject→保持 open+摘要备注）。
- **审计**：`AuditLog`（who/when/what/why + correlationId 贯穿），`GET /incidents/:id/audits`。
- **Playbook**：数据驱动 + 默认种子自动补种（零配置演示），UI 管理页 `/playbooks`。

### Phase 2b — 通知渠道 ✅
- **notifier 包**：钉钉/飞书/企微/通用 HTTP webhook，fail-open 聚合，未配置渠道自动跳过。
- **编排器接入**：进入 `pending_approval`（仅首次转换）+ 审批通过 + 审批拒绝均通知，落 `notify.sent/failed/skipped` 审计。
- **`POST /api/v1/notify/test`** 手动验证渠道；前端 Incident 详情页「测试通知」按钮。
- 验收标准①「critical 告警 → RCA → 自愈 dry-run → 审批 → 通知」**全部打通**。

### Phase 2b — 维护窗口 ✅（当前最新）
- **`internal/maintenance` 包**：`Window` 模型（tenant + 时间范围 + 匹配条件 fingerprint/service/severity 上限 + reason）+ Service（CRUD / ActiveWindows / Matches）。
- **pipeline 热路径过滤**（keep 式第 2 步）：`WindowFilter` 按租户缓存活跃窗口（TTL 5s，CRUD 失效），命中 firing 告警 → 以 `suppressed` 状态入库，跳过规则/Incident/Agent，广播 `alert.suppressed`。
- **去重联动**：`dedup.Classify` 增加 `windowActive` 参数——窗口期内 suppressed 同内容算重复（防心跳刷表）；窗口失效后同内容**重新上浮为 firing** 并触发 Incident（e2e 已验证）。
- **API**：`GET/POST/PUT/DELETE /api/v1/maintenance-windows`（校验时间合法 + 至少一个匹配条件）；前端导航「维护窗口」管理页（日期选择 + 生效中标注）。
- e2e：pay 窗口期内告警 → `suppressed` + 0 incident；停用窗口后同告警 → `firing` + incident。

### Phase 2c — AI 聚类/摘要 ✅（当前最新）
- **`internal/ai` 包**：确定性聚类（无 LLM）——`ClusterAlerts` 按 `labels.service` 分组 → 组内时间升序、间隔超窗口断开 → 稳定分组键 `SHA256(service|去重排序names|最高severity)`（跨运行稳定，用于 Incident 去重）；`Summarize` 生成「中危 pay（12 条关联告警）」名称 + 摘要模板。
- **`Service.RunOnce/RunAll`**：扫描租户窗口内 **未归属（NOT IN alert_to_incidents）的 firing 告警** → 聚类 → 簇成员数 ≥ `AIOPS_AICLUSTERTHRESHOLD`（默认 3）确认 → 复用同 cluster_key 的 open incident（source=ai，仅追加告警）或新建。
- **worker**：`AIOPS_AICLUSTERENABLED`（默认 true）开启，启动跑一次 + 每 `AIOPS_AICLUSTERINTERVALSEC`（默认 60s）扫描；`RunOnce` 以 `sync.Mutex` 串行化（worker 与手动 POST 并发防重复建事故）。
- **合并刷新**：合并后按累计成员重建名称/摘要（e2e 验证：pay 12→16→20 条，名称同步刷新，不新建事故）。
- **API**：`POST /api/v1/ai/cluster` 手动触发一次（验收②演示入口）；前端 Alerts 页「聚类生成事故」按钮。
- **设计边界**：高/危急告警由 Agent 路径承接（severity ≥ high 自动进入编排器），AI 聚类只处理未被规则/Agent 承接的散落告警（低/中危），两者互斥、无重复建事故。聚类键含告警名集合——新增告警名会改变键 → 视为新故障新建事故（有意为之）。
- e2e：30 条 medium（pay×12/order×9/search×9）→ 3 个 source=ai incident；重跑 0 新建；同键新告警合并刷新。

### Phase 2d — YAML 工作流引擎 ✅
- **`internal/workflow` 包**：keep 式 YAML schema（`workflow{id,name,disabled,triggers,filters[]}` + `steps[]`/`actions[]`，每步 `provider{type,with}`）。
- **触发匹配**：`type: alert`（MVP）；filters 支持 severity（精确/`>=`/`<=`）、service（labels.service）、name/provider/source、任意 labels 键；无 filters 全命中。
- **并发防重（验收④）**：`workflow_runs` 表唯一索引 `(tenant_id, workflow_id, dedup_key)`，`dedup_key=SHA256(workflow_id:alert.fingerprint)`；插入用 `OnConflict DoNothing` + `RowsAffected==0` 判定输掉竞态 → 不二次执行。同一工作流对同一告警（指纹）**只执行一次**，并发首达仅一个赢者。
- **providers**：`log`（模板渲染 `{{.alert.*}}`，missingkey=zero）/ `http`（POST JSON，body 模板）/ `agent`（接入编排器）。
- **`provider: agent` 防重复**：告警已有 open 的 source=agent incident（内置 severity≥high 路径）→ 跳过；有 rule incident → 复用 incident_id 继续编排；否则 HandleAgentTask 新建。
- **异步触发**：pipeline 命中 → 入队 `queue.workflow.trigger` → 单消费者按任务类型 dispatch（agent.run / workflow.trigger）。
- **API**：`GET/POST/PUT/DELETE /api/v1/workflows`（部分更新：未传字段保留现值，修复过误禁用 bug）+ `GET /api/v1/workflow-runs`；前端「工作流」管理页（表格 + 最近运行 + YAML 编辑弹窗）。
- **种子**：`EnsureDefaults` 零配置补种演示工作流「订单中危告警自动接入 Agent 编排」（medium order 告警默认不进 Agent 路径，通过工作流显式接入编排器）。
- e2e：medium order 告警 → 工作流命中 → 编排器全链 monitor→rca→heal(approvalRequired)→change → `pending_approval`；20 个相同告警并行 → 恰好 1 次运行；同指纹内容更新（DedupUpdate）重触发 → 零新增运行。

### Phase 2d+ — RCA LLM 增强 ✅
- **`internal/ai/llm.go`**：OpenAI 兼容 Chat Completions 客户端（DeepSeek / Ollama / vLLM / LiteLLM），`Complete`（temperature 0.3，15s 超时，先查状态再解析）+ `EnhanceRCA`（组装证据链 prompt → 解析 `rootCause/rationale/suggestedActions`，容忍 ```json 围栏，rootCause 缺失判定解析失败）。
- **RCAAgent 增强**：确定性证据链之外注入可选 `RCALLM`（接口在 `internal/agent/rca.go`），补充自然语言根因/建议写入节点结果 `llm*` 字段（llmRootCause / llmRationale / llmSuggestedActions / llmAnalyzedAt）；**确定性结论保留**，LLM 只做补充视角。
- **fail-open**：未配置 apiKey / 网络失败 / 输出无法解析 → 仅记 `slog.Warn`，保持纯确定性 RCA，不阻塞编排（RCA 节点不因 LLM 失败重试）。
- **装配**：`AIOPS_LLMAPIKEY` 配置后启用（driver 默认 `openai-compat`，baseURL 默认 `https://api.deepseek.com`，model 默认 `deepseek-chat`）；未配置 key 完全跳过（`rcaLLM = nil`）。
- **前端**：Incident 详情 RCA 卡片新增「LLM 补充分析」区块（LLM 结论存在时显示，紫色 Tag）。
- **config 修复**：LLM apiKey 的 viper key `llm_apikey`（下划线）与 baseURL/model 命名不一致，导致 `AIOPS_LLM_APIKEY`（而非直觉的 `AIOPS_LLMAPIKEY`）才能生效 → 统一为 `llmapikey`，并加 config 单测锁定命名。
- **e2e**（`deploy/mock_llm.mjs` mock OpenAI 端点 + 独立实例 :8081）：critical order 告警 → rca 节点含 `rootCause=recent_change`（确定性）+ `llmRootCause/llmRationale/llmSuggestedActions`（mock 返回）；停掉 mock 后新告警 → warn「rca: llm enhance skipped」+ 纯确定性结论，编排正常完成。

### Phase 2e — 通知补充（事故生命周期通知 + 渠道管理 UI）✅（当前最新）
- **事故生命周期通知**：`incident.Service` 增加可选 `IncidentNotify` 钩子（`SetNotifyFunc` 注入）——`Create` 触发 `incident.created`（新事故），`Resolve` 触发 `incident.resolved`（事故解决）。覆盖全部创建路径：规则（rule_engine 改走 `svc.Create`）、Agent（`CreateFromAgent`）、AI 聚类（`ai.Service` 走 `svc.Create`）。**审批闭环用 `ResolveSilent`**：approve 已发送「审批通过」通知，避免同一事件双条打扰（e2e 验证 approve 仅 `approved`，无重复 `resolved`）。
- **`internal/notifier/lifecycle.go`**：`IncidentLifecycleMessage`（纯函数构建「【新事故】/【事故已解决】+ 来源/严重度/摘要」）+ `LifecycleNotifier`（发送 + 落 `notify.sent/failed/skipped` 审计，target `incident.created/resolved`，actor `auto-system`）。fail-open：无渠道/失败不阻塞事故创建/解决。
- **通知渠道管理 UI**：渠道配置 DB 化（`notification_channels` 表），env（`AIOPS_NOTIFIERCHANNELS` + webhook）仅作首启种子（`EnsureDefaults` 仅表空时插入，之后 DB 为准，重启不回退）。`notifier.Service.Reload` 热重载（`sync.RWMutex` 快照，不换指针，编排器/API 引用保持有效）；`Notify` 快照渠道后释放锁再发送（避免嵌套 RLock 与 Reload 写锁互等）。
- **API**：`GET /api/v1/notify/channels`（配置列表）/ `PUT /api/v1/notify/channels/:name`（保存即热生效）/ `POST /api/v1/notify/channels/:name/test`（单渠道测试，无需启用，验证新 webhook 用）。前端新增「通知渠道」管理页（4 渠道启用开关 + webhook 编辑 + 保存/测试）。
- **config**：`notifier.ConfigService`（EnsureDefaults / List / Save / Reload / Test），`BuildChannel` 构造器复用。
- **e2e**（独立实例 :8081 + `deploy/mock_webhook.mjs :9997`）：env 种子 http 渠道 → GET 见配置；`incident.created` 在 agent/rule/ai 三条路径均触发（critical/rule medium/AI cluster 各产生【新事故】通知）；手动 resolve → `incident.resolved`；审批 approve → 仅 `approved`（无重复 resolved）；PUT 停用 http → 告警不再产生通知（热重载生效），PUT 启用 → 恢复；审计 3 条 `notify.sent`（created/pending_approval/resolved）。

### Phase 2f — 工作流扩展（interval/manual 触发 + http headers/auth + 并发执行）✅（当前最新）
- **interval 触发**：`triggers: [{type: interval, with: {interval: "3s"|"5m"|30}}]`（Go duration 或纯数字秒）。`Service.StartIntervalScheduler` 定时（`AIOPS_WORKFLOWSCHEDINTERVALSEC` 默认 10s）扫描启用工作流，到期触发 `runInterval`（无告警上下文，log/http 步骤可跑、agent 步骤明确报错）。last-fire 键含 trigger 下标（同工作流多 interval 各自计时），进程内去重防重复触发，扫描结束清理已删工作流残留键。interval run 的 `dedup_key= SHA256("interval:"+wfID+":"+触发时刻)` 每次唯一（间隔事件不互相防重）。
- **manual 触发**：`POST /api/v1/workflows/:id/run`（body 可选 `alert_id` 装载告警并按 filters 匹配）；前端工作流表格「运行」按钮。`RunManual` 不做并发防重（`dedup_key="manual:"+runID` 每次唯一），便于反复演练；工作流 disabled/未启用拒绝。
- **http step 扩展**：`with.method`（默认 POST）/ `with.headers`（map，值支持 `{{.alert.*}}` 模板）/ `with.auth`（`{type: basic, username, password}` 或 `{type: bearer, token}`，值支持模板）。
- **并发执行（关键）**：执行从队列 worker 解耦——`tryRun`/`runInterval`/`RunManual` 落库 + `launch` 后立即返回，`execute` 搬进独立 goroutine（`execSem` 有界并发，`AIOPS_WORKFLOWEXECWORKERS` 默认 8），慢步骤（http/agent）不再占用队列 worker、不阻塞后续 agent.run。panic 转 failed 落库，10min 执行超时兜底。
- **`WorkflowRun` 加 `trigger` 列**（alert|interval|manual，AutoMigrate 自动加列，前端最近运行表展示触发来源）。
- **单测**（不触 DB）：`Interval()` 解析（duration/数字秒/非法）、Parse 对 interval 缺 `with.interval`/未知类型拒绝、httpStep method+headers+bearer 抓包断言、basic auth、renderTemplate nil alert、agentStep nil alert 报错。
- **e2e**（独立实例 :8081 + `deploy/mock_echo.mjs :9996`）：manual 触发 → run trigger=manual + http echo 回显 PUT/X-Trace/Bearer 全部正确；interval 3s → 10s 内 5 次全部 succeeded 且 dedup 键唯一；**并发**：8 个 blocktest 告警命中含 4s 慢 http 步骤的工作流 → 8 个 run 并发执行全部 succeeded，同时 critical pay 告警走 agent 路径在慢步骤运行期间完整走到 `pending_approval`（monitor→rca→heal→change），证明慢步骤不阻塞 agent.run。

### Phase 2g — LLM 深化（聚类摘要接 LLM + Heal 回填 LLM 建议 + 流式/超时可控）✅（当前最新）
- **聚类摘要也接 LLM（非确定性模板）**：`ai.ClusterLLM` 接口 + `EnhanceCluster`（簇事实 prompt → 50-80 字自然语言摘要 JSON）。`Incident` 新增 `LLMSummary` 列（type:text，**与确定性 `Summary` 并存**，LLM 失败保留确定性模板）；`ai.Service.SetClusterLLM` 可选注入，`confirm` **先 Create + AttachAlert 落库完成、再做增强**（顺序刻意放在最后：LLM 卡住/请求取消也不会产生无告警关联的孤儿事故），`refreshMerged` 合并后重增强（成员变化刷新摘要）。全部 best-effort：失败仅 `slog.Warn`，不阻塞、不返回 error、不清空已有摘要（空串短路）。
- **LLM 建议动作回填 Heal**：`agent/heal.go` 的 `suggestedActions` 合并 `rca.suggestedActions`（确定性优先）+ `rca.llmSuggestedActions`（LLM 补充），按顺序去重，非字符串/空串跳过 → Resolve 剧本匹配能看到 LLM 给的自然语言建议（如「重启连接池」建同名剧本即可命中）。
- **流式（SSE）**：`Client.SetStream(true)` 后 `chat` 分发到 `CompleteStream`——请求 `stream:true`，逐行解析 `data:` 事件累计 `choices[].delta.content`，`data: [DONE]` 结束，非 JSON 行跳过（健壮性），Scanner 大缓冲（64KB→4MB）防长行截断。默认关闭（非流式行为不变），`AIOPS_LLMSTREAM=true` 开启。
- **超时可控**：`SetTimeout` 覆盖 `http.Client.Timeout`（**流式累计期间同样生效**——整体总时长上限，不是 per-chunk 空闲超时）；`AIOPS_LLMTIMEOUTSEC` 默认 15。`config` 加 `LLMTimeoutSec/LLMStream` 及默认值、env 单测锁定命名（`AIOPS_LLMTIMEOUTSEC`/`AIOPS_LLMSTREAM`）。
- **mock 升级**：`deploy/mock_llm.mjs` 支持 SSE 流式（按 16 码元切 delta 输出 + `[DONE]`），并按 user prompt 是否含「告警簇」自动区分返回聚类摘要 JSON / RCA JSON；`[port] fail` 参数不变。
- **前端**：`api.ts` Incident 加 `llmSummary`；IncidentDetail 摘要区新增「LLM 聚类摘要」紫色区块（`detail.llmSummary` 存在时渲染，与确定性 `summary` 并存展示）。
- **e2e**（独立实例 :8081 + mock :9998，`AIOPS_LLMSTREAM=true`）：① 流式 RCA——critical order 告警 → rca 节点 `rootCause=recent_change`（确定性保留）+ `llmRootCause/llmSuggestedActions`（mock 经 SSE 返回，mock 日志 `stream=true type=rca`）；② Heal 回填——heal 节点 `reason` 显示合并列表 `rollback、profiling、verify_dependency、重启连接池、回滚 v1.2.3、扩容 db 副本`（确定性 + LLM 全量传入 Resolve）；③ 聚类摘要——4 条 medium pay → `POST /ai/cluster` → source=ai incident 同时有确定性 `summary` 与 `llmSummary`，同键新告警合并后名称/摘要/llmSummary 同步刷新；④ fail-open——停 mock 后新簇 → 确定性 summary 保留、无 llmSummary、warn「cluster llm enhance skipped」，建事故不受影响。
- **review 修复**：`confirm` 的 LLM 增强从 Create 后、AttachAlert 前移到两者之后（消除孤儿事故窗口）；`refreshMerged` 的 `UpdateLLMSummary` 失败从 return error 改为 warn（fail-open 一致）；`Client.timeout` 死字段移除（实际生效的是 `http.Client.Timeout`）；`UpdateLLMSummary` 注释不再声称 source=ai 硬约束；`TestHealBlockedByOpenBreaker` 熔断冷却期 `60`（ns）→ `60*time.Second`（消除时序 flake）；新增流式中途超时单测。

### Phase 3b — 写入优化（规模化延伸）✅（当前最新）
- **D1 单 SQL 双表写入**（`internal/alert/save_sql.go` + `repository.SaveAlert`）：alerts insert + last_alerts upsert 合并为一条 `WITH ins AS (INSERT ... RETURNING) INSERT ... ON CONFLICT`，**每事件 DB 往返 4 次 → 1 次**（单语句原子，语义不变；suppressed 路径自动共用）。列清单收敛在 `buildSaveSQL`（单一事实来源 + 纯函数单测）。实测（同 Phase 3 参数 2000rps/300 并发）：**P50 61→51ms（-17%）、P99 100→88ms（-12%）**，吞吐 1790/s 持平（瓶颈已不在单次写入往返）。
- **D4 loadgen `-batch`**（`cmd/loadgen`）：-rate 恒为事件/秒，请求速率 = rate/batch；批内按全局事件序号分配（fingerprint 互异）；汇总按事件折算（失败率阈值/吞吐）。
- **批量写（D2）论证后降级不做**：batch 20/50/100 扫描（2000 事件/s 恒速）——
  | batch | 请求延迟 P50 | 事件吞吐 | 失败率 |
  |---|---|---|---|
  | 1 | 51ms | 1790/s | 0.14% |
  | 20 | 973ms | 1621/s | 5% |
  | 50 | 2072ms | 1317/s | 12% |
  | 100 | 3769ms | 1000/s | 31% |
  请求延迟 ≈ batch × 50ms：**每事件管线耗时恒定**（dedup 2 读 + 规则 1 读 + 写 1 次 ≈ 4 次 DB 往返）。批量写（`SaveAlerts` 多行）只优化写的一次且需批内内存去重 overlay——**禁止裸延迟 flush（破坏批内去重正确性）**。扩展点（`SaveAlerts` + Deduper overlay 注入位）记录于 docs/PERF.md，待真实 Alertmanager 大批次 push 出现时立项。batch 失败均为 `refused`（长请求占连接 → Windows 回环耗尽），非服务端故障。
- **D3 alerts 月分区 + 保留策略 DROP PARTITION**（`internal/store/partitions.go` + `internal/retention`）：
  - 迁移（`migrate` 内 AutoMigrate 后原地执行，事务内幂等）：RENAME → CREATE 分区父表（**PK (id, created_at)**，PG 分区唯一约束必须含分区键；B-tree 左前缀保 `WHERE id=?` 走索引）→ 存量覆盖分区（min 首月→次月）+ `alerts_default` 兜底 → 分批搬迁 → DROP legacy。已分区时仅维护当月/次月 + default（跨月自动补建）。
  - **review 修复：AutoMigrate 先建索引于普通表，RENAME/DROP legacy 时被带走** → 迁移后 `ensureAlertIndexes` 幂等补建（父表 CREATE INDEX → PG 自动传播全部分区）。
  - 保留策略：Rule 加 `DropPartitions`，先 DROP 整月过期分区（`pg_class JOIN pg_inherits` + 分区名正则 `^alerts_p\d{6}$` 白名单；`alerts_default` 永不 drop）→ 行级 DELETE 兜底当月残余（30d 场景：6 月 DROP + 7 月残余行删除）。
  - 配置：`AIOPS_ALERTSPARTITIONS`（默认 true；关闭退回纯行级 DELETE，已分区表降级可用；**不提供反向迁移**）。
- **e2e**（独立实例 :8081）：① D1 功能——同 fingerprint 两事件 → 2 行 alerts + 1 行 last_alerts 指向最新，孤立 0；② 路由——6/29、7/4 行落 `alerts_default` 兜底分区；③ 保留清理——6 月分区 `retention: dropped partition alerts_p202606` + default 中 7/4 行 `cleaned rows=1`；④ 兼容——:8080 主实例（Phase 3 binary）写入分区表正常（落 8 月分区）；⑤ 压测对比与 batch 扫描见上。
- **数据清理**：压测数据（alerts 11.6 万行 + 关联 ati/incidents/last_alerts）按名清理，DB 恢复 129 alerts / 123 last_alerts / 孤立 0；incidents 56 行（rule/agent/ai 真实数据）保留。

### Phase 3c — 规则引擎规则缓存 ✅（当前最新）
- **动机**：真实洪峰下逐事件管线 ~50ms/事件 ≈ 4 次 DB 往返（dedup 2 读 + **规则 1 读** + 写 1 次）——规则读原为每事件全量 `rules` 表查询。
- **实现**（`internal/incident/rule_engine.go`）：`RuleEngine` 增加按租户的启用规则缓存（`ruleCache map[tenantID]ruleCacheEntry`），模式对齐 `WindowFilter`——RWMutex 快路径 + TTL（构造参数，默认 5s）+ **锁外查库 + double-check 回填**；`loadRules` 可注入（单测假 loader）、`now` 时钟注入。`Evaluate` 改走 `enabledRules`（TTL 内缓存命中 0 次 DB 读）。
- **失效**：`POST /rules` 创建后 `s.rules.Invalidate(tenantID)` 立即失效（下次 Evaluate 重查库，无需等 TTL）；DB 外部改规则受 TTL 上限约束（有界陈旧，与 WindowFilter 一致）。API server 与 pipeline 共享同一 `RuleEngine` 实例，失效点对热路径即时生效。
- **单测**（rule_engine_test.go，5 个）：TTL 内命中不重查 / TTL 过期重查 / Invalidate 强制重查且不影响他租户 / **loader 失败不污染缓存**（错误不写入缓存，恢复后正常）/ 并发访问安全（Windows 下 `-race` 工具链不可用，常规 -run 通过）。
- **压测**（2000rps/300 并发/30s/variety 200，同 Phase 3b 基线）：**P50 51→47.4ms（-7%）、P99 88→80.7ms（-8%）**，吞吐 1795/s 持平（瓶颈仍在逐事件串行），失败率 0.026%。收益符合预期：规则读占 4 次往返的 1 次。
- **e2e**：API 创建规则（critical + service=rcsmoke）→ 同条件告警 → 命中创建 `source=rule` incident（验证缓存命中路径 + Invalidate 后立即生效）；压测数据（alerts 4.2 万 + 8 个 loadgen AI 聚类 incident + 冒烟痕迹）按名清理，DB 恢复 129 alerts / 123 last_alerts / 2 rules / 56 incidents / 孤立 0。
- **其余 lever**（尚未做）：`SaveAlerts` 批量写（需批内 overlay）。生产 HTTP 超时调优已由 Phase 3e 落地。

### Phase 3d — dedup 单读（2 读合并）✅（当前最新）
- **动机**：规则缓存（3c）后每事件管线剩余 3 次 DB 往返（dedup **2 读** + 写 1 次）——dedup 原为 `GetLastAlert`（读 last_alerts 唯一索引）+ `Get(la.AlertID)`（回读 alerts 分区表拿 hash/status）。
- **实现**：`alert_hash` 冗余进 `last_alerts`（与已有 `status` 并列，Phase 3b 设计上它本就是去重热路径唯一读表）。`dedup.Classify` 合并为**只查 (tenant, fingerprint) 唯一索引 1 次**，不再碰 alerts 分区表。所有写入路径同步维护冗余列：
  - `SaveAlert` CTE（save_sql.go）：RETURNING 加 `alert_hash` → last_alerts INSERT/SELECT/ON CONFLICT 全链路携带。
  - `UpsertLastAlert`（repository.go）：Create + `DoUpdates` AssignmentColumns 加 `alert_hash`。
  - 迁移：AutoMigrate 加列后 `backfillLastAlertHash` 幂等回填存量行（从当前活跃告警 alerts 行取值；`alert_id` 已指向已删告警的残留行不回填，下次事件自然走 update 并回填，降级安全）。
- **行为改进（review 确认）**：以 last_alerts 为当前状态真源——alerts 历史行被保留策略删掉后，旧代码 `Get` 查不到会误判 update 反复刷表；新代码基于状态缓存直接判 duplicate，语义更正确。
- **重构**：`Deduper.repo` 从具体 `*alert.Repository` 改为最小接口 `LastAlertReader`（`GetLastAlert`），便于单测注入（与 rule_engine 可注入 loader 风格一致）；分类逻辑抽纯函数 `classifyLastAlert`。
- **单测**（dedup_test.go，7 个）：classifyLastAlert 全分支（firing 同/diff hash、窗口内 suppressed、窗口失效上浮、resolved 复发）+ Classify 单读断言（恰好 1 次 GetLastAlert）+ new + 错误传播。
- **压测**（2000rps/300 并发/30s/variety 200，同 3c 基线）：

  | 版本 | P50 | P99 | 吞吐 | 失败率 |
  |---|---|---|---|---|
  | Phase 3b（无缓存） | 51ms | 88ms | 1790/s | 0.14% |
  | Phase 3c（规则缓存） | 47.4ms | 80.7ms | 1795/s | 0.026% |
  | **Phase 3d（dedup 单读）** | **33.1ms** | **61.9ms** | **1796/s** | **0.035%** |

  **P50 -30%、P99 -23%**，吞吐持平（瓶颈已是写 1 次 + 业务逻辑）。收益比规则缓存更大——dedup 是每事件必走的 2 读。
- **e2e**：三态验证——new（全新 fingerprint 入 1 行 alerts + 1 行 last_alerts）/ update（startsAt 变→hash 变→alerts 增行 + last_alerts 更新指向最新）/ duplicate（同 payload→alerts 不增）；`hash_matches_alerts=t` 证明 last_alerts.alert_hash == 当前活跃告警 hash；启动日志 `backfilled last_alerts.alert_hash rows=123`。压测/冒烟数据已清理，DB 恢复 129 alerts / 123 last_alerts / 56 incidents / 孤立 0。

### Phase 3e — 生产 HTTP 超时调优 ✅（当前最新）
- **动机**（docs/PERF.md 建议 #1 落地项）：生产要求设置 `WriteTimeout`/`IdleTimeout`，防慢响应客户端占住 goroutine、僵尸 keep-alive 连接占 fd；顺带补 `ReadTimeout`（慢 body）。
- **配置**（`config.go` + `main.go`）：`http.Server` 从仅 `ReadHeaderTimeout:5s` 扩为四项——`ReadHeaderTimeout 5s`（固定，防 slowloris）+ `ReadTimeout`（默认 15s）+ `WriteTimeout`（默认 30s，**须 > LLMTimeoutSec**）+ `IdleTimeout`（默认 120s，对 WS 不生效）。env：`AIOPS_HTTPREADTIMEOUTSEC` / `AIOPS_HTTPWRITETIMEOUTSEC` / `AIOPS_HTTPIDLETIMEOUTSEC`。
- **关键坑（必须记住）**：**coder/websocket hijack 后不会清除 net/http 落在底层连接上的读写 deadline（gorilla 有清、coder 没有）**。全局 `ReadTimeout` 直接 `SetReadDeadline` 残留、`WriteTimeout` 在 Go 1.26 是 readRequest 返回时 `defer SetWriteDeadline` 残留（不再是 timer）——WS 长连接会在超时秒数后读写被强制断开。修复：`hub.ServeWS` 在 `websocket.Accept` **前**用 `http.NewResponseController(w)` 清 `SetReadDeadline/SetWriteDeadline(time.Time{})`（沿 gin `ResponseWriter.Unwrap()` 到达底层 conn，Go 1.26 的 `response.SetReadDeadline` 直接转发无遮挡）。
- **验证**：WS 探针连接在 `ReadTimeout=3s/WriteTimeout=4s` 下存活 10s+ 且实时收到 `alert.new/update/duplicate` 广播（deadline 清除生效）；REST 正常；`go build/vet/test` 全绿。
- **性能**：纯配置层改动，无管线回归（同环境基线 d5dbdb7 对比 162ms vs 改动后 86ms，均受 PG 写入 LWLock/IO 瓶颈影响，见 PERF.md 当前环境说明）。

---

### Phase 3 — 规模化（Postgres 加固 + 万级压测 + 拓扑影响面）✅
- **Postgres 加固**：
  - **连接池调优**（`config` + `store.Open`）：`DBMaxOpenConns`（默认 20）/ `DBMaxIdleConns`（默认 **= MaxOpen**）/ `DBConnMaxLifetimeSec`（1800）/ `DBConnMaxIdleTimeSec`（300）；`AIOPS_DBMAXOPENCONNS=50 AIOPS_DBMAXIDLECONNS=50` 已验证。**MaxIdle=MaxOpen 是关键修复**：压测中现 `connectex: Only one usage of each socket address`（Windows 回环 TIME_WAIT 冲突，空闲连接重连导致），对齐后 dial 错误归零。
  - **启动重试**：`DBConnectRetries=5`（Ping 3s 超时 + 2s 退避），compose Postgres 未就绪不炸。
  - **热表索引**：`alerts.created_at` / `audit_logs.created_at` / `workflow_runs.started_at`（保留清理的索引保障）。
  - **保留策略 worker**（`internal/retention`）：alerts 30d / audit 180d / workflow_runs 90d，`DELETE ... WHERE id IN (SELECT id ... LIMIT 5000)` 分批循环（防长事务锁表），boot 清理一次 + 定时（`AIOPS_RETENTIONINTERVALMIN` 默认 60）。事故与 last_alerts 不参与。
- **万级压测**（验收口径：万级/分钟 ≈ 167/s，失败率 ≤1% —— **10 倍余量达标**）：
  - `cmd/loadgen` 告警洪水工具：限速/并发/告警名轮换（variety 200 → 自然覆盖 new/update/duplicate 三态）/吞吐/P50-P95-P99，失败率 >1% 非零退出（可接 CI）。
  - **PASS 轮**：300 并发 × 2000 rps → **1786/s（≈10.7 万/min），失败 0.12%，P50=61ms P99=100ms**；上限探索 500 并发 × 5000 rps → 2610/s，服务端 90% 完成 ≤250ms。
  - **根因链（本阶段最大教训）**：早期 300 并发 60-90% 失败而服务端直方图全 ≤250ms、Postgres 空闲——逐层排查（直方图→pg_stat_activity/pg_locks 采样→并发阶梯→错误分类→netstat）锁定**压测客户端每请求新建 TCP 连接**（`resp.Body.Close()` 前未读 body → net/http 判定连接污染销毁 → 回环连接风暴 → accept 拒绝）。修复（读 body + 共享 transport + Dial 计数）后同参数 84/s → 1786/s（21 倍）。**启示：压测工具必须模拟真实 keep-alive 行为；客户端与服务端指标分歧时先查连接层。**
  - `internal/metrics`：stdlib Prometheus 文本格式（counter 固定标签集 + histogram 累计桶 + gauge 采集时求值），`GET /metrics`（不挂鉴权组）；埋点：管线耗时直方图 / 去重构成 / Incident 来源 / 队列深度。
  - **WindowFilter 锁外查库**（review 防御性修复）：维护窗口缓存原为全局 Mutex 内执行 DB 查询——TTL 过期瞬间所有并发请求在锁前排大队（周期性吞吐崩塌）。改 RWMutex 快路径 + 锁外加载 + double-check 回填。
- **拓扑影响面**（C1/C2）：
  - `topology_edges` 归一化边表（`uk_topo_edge` 唯一 + `idx_topo_edge_dep` 反向遍历索引；Create/启动 `SyncAllEdges` 回填）。
  - `GET /api/v1/topology/:name/impact?depth=5`：SQL `WITH RECURSIVE` 反向遍历（深度/路径/防环，`array_to_string(path,'→')` 规避 pgx text[] 扫描限制）；返回 `{service, depth, affected[{service,depth,path}], total}`。
  - **Neo4j 后置仅替换本查询**（模型与 API 不变）。
  - 前端 Topology 页：服务表（点击选中）+ 影响面树（AntD Tree 由路径组装，defaultExpandAll）。
- **e2e**（独立实例 :8081，池 50/50）：① 保留策略——预插 40 天前测试行 → boot 日志 `retention: cleaned table=alerts days=30 rows=1`（已删）；② 影响面——`p3-pay` 故障 → cart(1)/order(1)/checkout(2)/web(3) 全链正确，depth=1 限制正确，`p3-checkout` → web(1)；③ 压测全链见上。
- **review 修复**：metrics 直方图补 `+Inf` 桶（Prometheus 规范：`_bucket{le="+Inf"}` 必须等于 `_count`，样本超桶时格式合法）；`retention.New` 不再复用入参 slice 底数组；`WindowFilter` 锁外查库。
- **数据清理**：压测产生的 loadgen 数据（`name='loadgen benchmark'`，alerts ~16 万行）已按名清理，DB 恢复干净；:8080 主实例已重启加载新 binary（metrics/保留 worker/拓扑全部生效）。

---

## 2. 目录结构与核心包职责

```
cmd/aiops/          装配根（config→store→pipeline→orchestrator→api→worker）
internal/
  alert/            Alert/LastAlert 模型 + 仓储 + 两级去重 + fingerprint/ContentHash
  pipeline/         事件管线（去重/入库/规则关联/WS/Agent 触发 agent.run）
  incident/         Incident/Rule/State 模型 + CEL 规则引擎 + 编排器检查点（incident_states）+ 生命周期通知钩子（Create/Resolve）
  cel/              CEL 引擎封装（预编译缓存）
  orchestrator/     编排器状态机 + worker(HandleAgentTask) + 审批 ApproveAction + 通知联动
  agent/            Agent 接口 + Monitor/RCA/Heal/Change（Context{State,Alert,Incident,Cfg}）
  playbook/         自愈剧本（数据驱动 + EnsureDefaults 种子）
  circuit/          熔断器（CLOSED/OPEN/HALF_OPEN，时钟注入可测）
  notifier/         通知渠道（Message/Channel/Service + dingtalk/feishu/wecom/http）+ 事故生命周期通知（lifecycle.go）+ 渠道 DB 配置（ChannelConfig/ConfigService）
  maintenance/      维护窗口（Window 模型 + CRUD + ActiveWindows + Matches 匹配）
  ai/               AI 聚类/摘要（确定性 cluster/summarize/service）+ OpenAI 兼容 LLM 客户端（llm.go，RCA 增强）
  workflow/         keep 式 YAML 工作流引擎（def/match/service + workflow_runs 并发防重 + interval 调度器 scheduler.go + 异步有界执行 launch）
  topology/         服务拓扑（RCA 事实来源，ImpactScore）
  eventbus/         事件总线抽象 + AuditLog + AuditService 落库
  queue/            内存任务队列（单消费者按类型 dispatch：agent.run / workflow.trigger）
  pusher/           WebSocket hub
  provider/         数据源抽象 + 注册表（builtin: prometheus 等）
  auth/             API key 认证
  api/              Gin 路由 + handlers（playbooks/incidents/approval/audits/notify/test + notify/channels）
  config/ store/ util/
web/               Vite + React 19 + AntD（pages: Alerts/Incidents/IncidentDetail/Playbooks/MaintenanceWindows/Workflows/NotificationChannels/Rules/Providers/Topology）
deploy/            docker-compose（PostgreSQL）+ mock_webhook.mjs（本地 mock 通知接收）
docs/              本文档
bin/aiops.exe      当前后端二进制（gitignore）
```

---

## 3. 关键实现约定（改动前必读）

### 编排器
- `Node{Name, Agent, Condition func(*incident.State) bool, MaxRetries}`，顺序 pipeline，条件路由跳过（fail-open）。
- 编排表在 `cmd/aiops/main.go buildOrchestrator`：monitor → rca → heal → change。
- `Run(ctx, st, a)`：每节点后 checkpoint 落库 + WS 广播 `incident.state`；末尾检测 pending_approval 转换并通知。
- `ApproveAction(ctx, tenantID, incidentID, decision, actor, reason)`：审批闭环 + 审计 + 通知。

### Agent 契约
```go
type Agent interface {
    Name() string
    Handle(ctx context.Context, c *Context) error // 结果写 ctx.State.SetNodeResult(name, map[string]any)
}
```
结果 map 字段约定：monitor{severity,service,alertName,...} / rca{rootCause,confidence,evidence,suggestedActions,changes} /
heal{status,action,level,blastRadius,downgraded,approvalRequired,command,rollbackCommand,...} /
change{riskScore,level,decision,breakdown,weights,...}。

### 安全护栏链（自愈）
dry-run（`AIOPS_AGENTDRYRUN=true` 默认）→ 爆炸半径 >`AIOPS_AGENTBLASTRADIUSLIMIT`(0.2) 强制降级 L2 →
熔断器 5 失败 60s → 级别 >`AIOPS_AGENTAUTOHEALLEVEL`(L0) 需审批。

### 审计
- `AuditLog{Actor,Action,Target,Reason,Metadata,CorrelationID,...}`，`Metadata` 存 `datatypes.JSON`。
- action 约定：`approval.granted/rejected`、`heal.executed`、`notify.sent/failed/skipped`、`incident.pending_approval`。
- `recordAudit` 辅助方法在 orchestrator；AuditService.Record/List 在 eventbus。

### 通知（notifier）
- 配置：`AIOPS_NOTIFIERCHANNELS`（逗号分隔）+ `AIOPS_NOTIFIER{DINGTALK,FEISHU,WECOM,HTTP}` + `AIOPS_NOTIFIERBASEURL`。env 仅作首启种子，DB `notification_channels` 表为准（`ConfigService.EnsureDefaults` 只插不覆盖，UI 保存即 `Reload` 热生效）。
- `notifyAndAudit`（orchestrator）统一封装：通知 + 落审计；URL 自动拼接 baseURL/incidents/:id；错误信息已脱敏（`?***`）。
- 测试用 `deploy/mock_webhook.mjs`：`AIOPS_NOTIFIERCHANNELS=http AIOPS_NOTIFIERHTTP=http://localhost:9999/webhook`。
- 事故生命周期通知：`incident.Service.SetNotifyFunc` 注入 `notifier.LifecycleNotifier.Notify`（签名 `func(ctx, *Incident, action string)`），`Create`/`Resolve` 触发 `incident.created/resolved`；**审批闭环必须用 `ResolveSilent`**（approve 已有「审批通过」通知，避免双条）。
- `notifier.Service` 线程安全：`Notify` 先快照渠道引用再释放锁发送（勿在持锁时再调 `Enabled()` 等取锁方法，会与 `Reload` 写锁互等死锁）。渠道名常量用 `notifier.ChannelDingTalk` 等，勿硬编码字符串。

### 维护窗口（maintenance）
- 模型：`Window{TenantID, Name, Fingerprint, Service, Severity(上限), StartsAt, EndsAt, Enabled, Reason}`；匹配需至少一个条件，全部满足才命中。
- 匹配仅对 **firing** 告警生效（resolved/suppressed 一律放行，保证恢复链路）。
- `WindowFilter` 在 `cmd/aiops/main.go` 装配：`pipe.SetMaintenanceFilter(pipeline.NewWindowFilter(mwSvc, 5*time.Second))`；窗口 CRUD 后必须 `pipe.InvalidateMaintenance(tenant)`（否则缓存最长 5s 内不生效）。
- `dedup.Classify(..., windowActive bool)`：窗口内 suppressed 同内容算 duplicate；窗口失效后重新上浮为 firing。

### AI 聚类（ai）
- 纯函数 `ClusterAlerts(alerts, window)` 不触 DB，便于单测；`Service` 在 `cmd/aiops/main.go` 装配：`ai.NewService(db, svc, cfg.AIClusterThreshold, time.Duration(cfg.AIClusterWindowMin)*time.Minute)`。
- `window` 双职：扫描回溯窗口（`created_at >= now-window`）与组内断开间隔（gap > window 断开）。>window 的长故障早期告警会被排除，MVP 可接受。
- 只处理**未归属** firing 告警（`id NOT IN alert_to_incidents`），不抢规则/Agent 已承接的告警。
- 复用规则引擎的 dedup 模式：`ClusterKey`（sha256）≈ `rule_fingerprint`，`source='ai'` 区分事故来源；合并时刷新名称/摘要。
- 触发方式：`POST /api/v1/ai/cluster`（手动，一次）+ 定时 worker（默认 60s）。

### YAML 工作流引擎（workflow）
- 存储原始 YAML 文本，触发时 `Parse` 解析校验（缺 `workflow.id`/无 trigger/step 缺 name 或 provider.type 均拒绝）；trigger 类型仅 `alert|interval|manual`，interval 必须带 `with.interval`；`disabled: true`（YAML）与 `enabled`（DB）双重开关。
- 执行顺序 steps 后 actions（fail-fast），结果落 `WorkflowRun.Result`（`map[stepName]any`）。
- **触发三条路径**：alert（pipeline → `workflow.trigger` 队列 → `tryRun`，并发防重）；interval（`StartIntervalScheduler` 扫描到期触发 `runInterval`，每次 dedup 键唯一）；manual（`POST /workflows/:id/run` → `RunManual`，不做防重）。`WorkflowRun.Trigger` 记录来源。
- **执行异步解耦**：`launch` 落库后立即返回，`execute` 在独立 goroutine 跑（`execSem` 有界，`AIOPS_WORKFLOWEXECWORKERS` 默认 8），慢步骤不占用队列 worker/不阻塞 agent.run；panic 转 failed、10min 超时兜底。**勿把 execute 改回同步**（会回退「慢步骤阻塞队列」问题）。
- 并发防重核心在 `tryRun`：唯一索引 + `RowsAffected==0` 判定丢者；**勿移除 `workflow_runs` 的 uk_wfrun_tenant_workflow_key 索引**（interval/manual 的 dedup 键每次唯一，不冲突）。
- `provider: agent` 复用 `AgentRunner` 接口（编排器实现）注入，便于单测；`orch == nil` 时 agent 步骤报错不崩溃；**无告警运行（interval/manual 不带 alert_id）用 agent 步骤会明确报错**。
- http step：`with.method/headers/auth`（值支持模板）；headers/auth 在 YAML 中解析为 `map[string]any`。
- pipeline 第 8 步投递 `workflow.trigger`（`AIOPS_WORKFLOWENABLED` 默认 true，false 关闭）；消费者在 `cmd/aiops/main.go` 按 `t.Type` 分发；interval 调度器在 main.go `wfSvc.StartIntervalScheduler(consumeCtx, schedInterval)`。
- API 部分更新：`handleUpdateWorkflow` **先读现有记录再合并**（yaml 非空才覆盖、enabled 用指针判断是否传），避免零值误禁用。

### LLM 增强（ai/llm.go + agent/rca.go + ai/service.go）
- 客户端 OpenAI 兼容：baseURL 可直接指向 `/chat/completions` 或根地址（`chatURL` 自动拼接）；`Complete` 先查状态码再解析，错误体非 JSON 也返回状态信息。
- 门控：`LLMDriver == "openai-compat" && LLMAPIKey != ""` 才构建 client；否则 `rcaLLM`/`clusterLLM = nil`（RCA 与聚类纯确定性）。LLM 增强是**可选补充**，永远不改变确定性结论。
- **同一 client 装配两处**（`cmd/aiops/main.go` 4.5 节）：`rcaLLM = llmClient`（RCAAgent `EnhanceRCA`）+ `aiSvc.SetClusterLLM(llmClient)`（聚类摘要 `EnhanceCluster`）；`llmClient.SetTimeout(...)`/`SetStream(...)` 由 `AIOPS_LLMTIMEOUTSEC`/`AIOPS_LLMSTREAM` 控制。
- `EnhanceRCA` 失败 → RCAAgent `enhance` 记 warn 后 return，**不向编排器抛错**（避免 MaxRetries 重试/阻塞）；`EnhanceCluster` 失败 → `ai.Service` 记 warn，**保留确定性 summary**（fail-open）。
- **流式**：`stream:true` 时 `chat` 走 `CompleteStream`（SSE）；默认非流式。**超时**：`http.Client.Timeout` 覆盖整体调用（含流式累计），超时点后调用方 fail-open。
- 节点结果新增 `llm*` 前缀字段；前端 IncidentDetail RCA 卡片按 `rca.llmRootCause` 存在性渲染「LLM 补充分析」；聚类事故详情按 `llmSummary` 存在性渲染「LLM 聚类摘要」（与确定性 summary 并存）。
- **LLM 建议动作回填 Heal**：`suggestedActions` 合并 `suggestedActions` + `llmSuggestedActions`（确定性优先、去重、顺序保留）——Heal 的剧本匹配可见 LLM 建议，建同名剧本即可命中。
- 本地 e2e 用 `deploy/mock_llm.mjs [port] [fail]`（fail 参数模拟 500；支持 SSE 流式 + 按 prompt 区分聚类/RCA）；注意 mock webhook 与 mock llm 是不同端口（9999 / 9998）。

---

## 4. 当前运行状态

| 组件 | 状态 | 备注 |
|---|---|---|
| PostgreSQL | docker 容器（compose） | `deploy/docker-compose.yml`，DSN `host=127.0.0.1 user=aiops password=aiops dbname=aiops port=5432` |
| 后端 | :8080 运行中（PID 20548，Phase 3e binary） | 启动命令含 `AIOPS_NOTIFIERCHANNELS=http AIOPS_NOTIFIERHTTP=http://localhost:9999/webhook AIOPS_NOTIFIERBASEURL=http://localhost:5173 ./bin/aiops.exe`；HTTP 超时默认 Read 15s / Write 30s / Idle 120s |
| mock webhook | :9999 运行中（PID 28620） | `node deploy/mock_webhook.mjs 9999`，收到的通知打印到 stdout |
| 前端 | Vite dev :5173 | `cd web && npm run dev` |

> 注意：后台进程随 Claude 会话退出可能被终止，重开后按上表命令重启即可。

> 启动说明：首启把 `AIOPS_NOTIFIERCHANNELS` 渠道种入 `notification_channels` 表，此后渠道以 UI/DB 为准；事故新建/解决推送生命周期通知；工作流 interval 调度器随启动运行；配置 `AIOPS_LLMAPIKEY` 后聚类摘要接 LLM（`AIOPS_LLMSTREAM=true` 默认流式、`AIOPS_LLMTIMEOUTSEC=15` 默认超时）。HTTP 超时默认值即生产规格，无需额外 env。

### 重启后端命令
```bash
# 先停旧进程（可选）
# netstat -ano | grep ':8080' | grep LISTEN  →  taskkill //PID <pid> //F
cd /d/CODE/GOLANG/AIops
go build -o bin/aiops.exe ./cmd/aiops
AIOPS_NOTIFIERCHANNELS=http AIOPS_NOTIFIERHTTP=http://localhost:9999/webhook AIOPS_NOTIFIERBASEURL=http://localhost:5173 ./bin/aiops.exe
```

---

## 5. 验收命令速查

```bash
# ① critical 告警 → 编排器（heal dry_run/降级 + change 风险评分）→ pending_approval
curl -X POST localhost:8080/api/v1/webhook/prometheus -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"pay-slow","severity":"critical","service":"pay"},"annotations":{"summary":"pay latency spike"},"startsAt":"2026-08-07T10:00:00Z"}]}'

# ② 查状态（nodeResults.heal / change，state.status=pending_approval）
curl localhost:8080/api/v1/incidents/<id>

# ③ 审批 / 拒绝
curl -X POST localhost:8080/api/v1/incidents/<id>/approval -H 'Content-Type: application/json' \
  -d '{"decision":"approve","actor":"oncall-engineer","reason":"批准回滚"}'

# ④ 审计轨迹（含 notify.sent）
curl localhost:8080/api/v1/incidents/<id>/audits

# ⑤ 手动测通知渠道
curl -X POST localhost:8080/api/v1/notify/test -H 'Content-Type: application/json' -d '{"title":"测试","severity":"critical"}'

# ⑥ 维护窗口：建窗口（示例覆盖 service=pay 全部严重度，当前生效）→ 发匹配告警应 suppressed
curl -X POST localhost:8080/api/v1/maintenance-windows -H 'Content-Type: application/json' \
  -d '{"name":"pay 维护","service":"pay","severity":"critical","startsAt":"2026-08-07T00:00:00Z","endsAt":"2026-08-08T00:00:00Z","reason":"e2e","enabled":true}'
curl -X POST localhost:8080/api/v1/webhook/prometheus -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"pay-slow","severity":"critical","service":"pay"},"annotations":{"summary":"pay latency spike"},"startsAt":"2026-08-07T10:00:00Z"}]}'
curl -s localhost:8080/api/v1/alerts | grep pay   # → status=suppressed，且无新 incident
# 停用窗口（PUT enabled=false）后发相同告警 → 恢复 firing + 新建 incident（重新上浮）

# ⑦ AI 聚类：POST 30 条 medium 告警（每服务 ≥3，同窗内）→ 触发聚类 → 3 个 source=ai incident
curl -X POST localhost:8080/api/v1/webhook/prometheus -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"pay-slow","severity":"medium","service":"pay","pod":"pay-01"},"startsAt":"2026-08-07T11:00:00Z"}]}'
curl -X POST localhost:8080/api/v1/ai/cluster   # → {"scanned":30,"clusters":3,"created":[...source=ai...],"merged":0}
# 重跑 → scanned 0（全部已归属）；再发同键告警 → merged 增加且不新建（名称刷新条数）

# ⑧ 工作流：查看/创建（YAML 含 {{.alert.name}} 模板、provider: agent）/ 最近运行
curl localhost:8080/api/v1/workflows
curl localhost:8080/api/v1/workflow-runs
# ⑨ 验收④ 并发防重：工作流命中告警后，同指纹并行发 20 次 → workflow-runs 对应工作流仅 1 条
# 命中 medium order 告警（seed 工作流）→ 自动接入编排器（source=agent incident，pending_approval）
curl -X POST localhost:8080/api/v1/webhook/prometheus -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"order-slow","severity":"medium","service":"order","pod":"order-1"},"annotations":{"summary":"order latency spike"},"startsAt":"2026-08-07T12:00:00Z"}]}'

# ⑩ LLM 深化：聚类摘要接 LLM + 流式 RCA + Heal 建议回填（AIOPS_LLMSTREAM=true 默认流式，AIOPS_LLMTIMEOUTSEC=15 默认超时）
# 本地演练：node deploy/mock_llm.mjs 9998  +  后端 AIOPS_LLMBASEURL=http://localhost:9998 AIOPS_LLMAPIKEY=dummy AIOPS_LLMMODEL=mock
# 流式 RCA：建拓扑 → critical 告警 → rca 节点 llmRootCause/llmSuggestedActions（mock 经 SSE 返回）
curl -X POST localhost:8080/api/v1/topology -H 'Content-Type: application/json' \
  -d '{"name":"order","kind":"service","dependsOn":["db"],"recentChanges":["v1.2.3 deploy"]}'
curl -X POST localhost:8080/api/v1/webhook/prometheus -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"llm-demo","severity":"critical","service":"order"},"annotations":{"summary":"order api latency spike"},"startsAt":"2026-08-07T13:00:00Z"}]}'
# → GET /api/v1/incidents/:id 的 nodeResults.rca 见 llmRootCause/llmRationale/llmSuggestedActions/llmAnalyzedAt；
#   heal 节点 reason 可见合并列表（确定性建议 + LLM 建议回填）；建同名剧本（如「重启连接池」）即可命中 LLM 建议
# 聚类摘要：≥3 条同服务 medium 告警 → POST /ai/cluster → 新 source=ai 事故详情同时有确定性 summary 与 llmSummary
curl -X POST localhost:8080/api/v1/ai/cluster
# 未配置 key / LLM 不可达 → 纯确定性，仅日志 warn（fail-open：确定性 summary 保留、llmSummary 为空）

# ⑪ 通知补充：渠道管理 + 事故生命周期通知（env 首启种子 http 渠道；保存即热生效）
curl localhost:8080/api/v1/notify/channels                             # 配置列表（含未启用）
curl -X PUT localhost:8080/api/v1/notify/channels/http -H 'Content-Type: application/json' \
  -d '{"enabled":true,"webhook":"http://localhost:9999/webhook"}'       # 保存即热重载（停用=静默）
curl -X POST localhost:8080/api/v1/notify/channels/http/test            # 单渠道测试（无需启用）
# 任意新事故（rule/agent/ai 任一来源）→ mock 收到「【新事故】…」+ 审计 notify.sent(incident.created)
# 手动解决事故（POST /incidents/:id/resolve）→ mock 收到「【事故已解决】…」+ audit incident.resolved
# 审批 approve → 仅「审批通过」通知（ResolveSilent，无重复 resolved）

# ⑫ 工作流扩展：interval/manual 触发 + http step headers/auth + 并发执行
# 建一个 interval 工作流（with.interval 周期）→ 每周期自动新增一条 workflow-runs（trigger=interval）
curl -X POST localhost:8080/api/v1/workflows -H 'Content-Type: application/json' -d '{"name":"定时演示","enabled":true,"yaml":"workflow:\n  id: wf-timer\n  triggers:\n    - type: interval\n      with:\n        interval: 5s\n  steps:\n    - name: ping\n      provider: { type: log, with: { message: \"tick\" } }"}'
# 手动触发任意启用工作流（前端「运行」按钮等价；可选 alert_id 装载告警）：
curl -X POST localhost:8080/api/v1/workflows/<wf-id>/run -H 'Content-Type: application/json' -d '{}'
# → {"runId":...,"trigger":"manual","status":"running"}；workflow-runs 对应记录 trigger=manual
# http step 扩展（method/headers/auth 值支持模板；本地可用 deploy/mock_echo.mjs 回显验证）：
#   provider: { type: http, with: { url, method: PUT, headers: {X-Trace: "wf-{{.alert.name}}"},
#               auth: {type: bearer, token: "tok-{{.alert.severity}}"}, body: "{\"x\":1}" } }
# 并发不阻塞：含慢 http 步骤的工作流命中大量告警时，agent.run 等队列任务不被阻塞（执行已异步解耦）
```

### e2e 实测结果（已通过，2026-08-07）
- pay-slow-nf → heal rollback L2（爆炸 0.35 降级）+ change 0.54 L2 → pending_approval → mock 收到 ⏳待审批通知（含降级警告/风险/详情链接/correlationId）→ audit `notify.sent`。
- approve → mock 收到 ✅审批通过（actor=oncall-engineer）→ audit `approval.granted + heal.executed + notify.sent`。
- reject → mock 收到 ⛔需人工处置 → incident 保持 open → audit `approval.rejected + notify.sent`。
- 重启后数据持久化正常（alerts/incidents/audits/playbooks 均保留）。
- **AI 聚类（Phase 2c）**：30 条 medium（pay×12/order×9/search×9，pod 不同保证指纹独立）→ `POST /ai/cluster` → 3 个 source=ai incident（「中危 pay/order/search（N 条关联告警）」+ 摘要）；重跑 0 新建；再发 4 条同键 pay 告警 → merged=1 不新建，pay incident 12→16→20 条且名称刷新。
- **YAML 工作流引擎（Phase 2d）**：medium order 告警（默认不进 Agent 路径）→ seed 工作流命中 → `provider: agent` → 编排器全链 monitor→rca→heal（approvalRequired=true, blastRadius 0.02, verify_dependency）→change → `source=agent` incident `pending_approval`（工作流 run 结果含 enrich 消息 + run-agent{handled,incidentId}）。
  - **并发防重**：同指纹告警并行发 20 次 → `workflow-runs` 对应工作流仅 1 条 succeeded；同指纹内容更新（DedupUpdate 重新入队 trigger）→ 零新增运行（dedup_key 基于稳定 fingerprint）。
  - **部分更新修复**：PUT 仅 `{enabled}` 不再清空 YAML、仅 `{yaml}` 不再重置 enabled（review 发现的零值误禁用 bug）。
- **RCA LLM 增强（Phase 2d+）**：`deploy/mock_llm.mjs`（OpenAI 兼容 mock :9998）+ 独立实例 :8081（AIOPS_LLMAPIKEY=dummy）→ critical order 告警 → rca 节点结果 `rootCause=recent_change`（确定性保留，confidence 0.69）+ `llmRootCause`（mock 返回「order 数据库连接池耗尽（mock LLM）」）`llmRationale` `llmSuggestedActions`（3 条）`llmAnalyzedAt`；mock 端打印的 user prompt 含完整证据链（告警/摘要/标签/根因候选/变更/依赖路径）。
  - **fail-open**：停掉 mock LLM 后新告警 → 后端 `WARN rca: llm enhance skipped`（connection refused），rca 节点无 llm* 字段、确定性结论与 suggestedActions 完整，编排正常完成。
- **通知补充（Phase 2e）**：`deploy/mock_webhook.mjs :9997` + 独立实例 :8081（`AIOPS_NOTIFIERCHANNELS=http AIOPS_NOTIFIERHTTP=http://localhost:9997/webhook`）→ `GET /notify/channels` 见 env 种子 http 渠道；`POST /notify/channels/http/test` → mock 收到测试消息。
  - **事故生命周期通知**：critical order 告警（agent 路径）→ mock 收到「【新事故】order panic loop」+「⏳等待审批」；rule medium checkout 告警 → 「【新事故】checkout latency high」；AI 聚类确认（3 条 medium search）→ 「【新事故】中危 search（3 条关联告警）」；手动 resolve → 「【事故已解决】order panic loop」；approve → 仅「✅审批通过」（**无重复 resolved**，ResolveSilent 生效）；审计 3 条 `notify.sent`（target=incident.created / incident.pending_approval / incident.resolved）。
  - **渠道热重载**：PUT 停用 http → 再发告警 mock 无新消息（静默）；PUT 启用 → 新事故恢复通知；合并到既有 pending_approval 事故的告警不重复通知（dedup 语义正确）。
- **工作流扩展（Phase 2f）**：`deploy/mock_echo.mjs :9996` + 独立实例 :8081（`AIOPS_WORKFLOWSCHEDINTERVALSEC=2`）→ manual 工作流（http step PUT + X-Trace + bearer）`POST /workflows/:id/run` → run 记录 `trigger=manual`，结果回显 `method=PUT / auth=Bearer tok-manual / trace=manual-demo`（headers/auth 模板渲染正确）；interval 工作流（`interval: 3s`）→ 10s 内 5 条 run 全部 `trigger=interval` succeeded、dedup 键互不相同（每次触发唯一）。
  - **并发不阻塞**：8 个 blocktest medium 告警命中含 4s 慢 http 步骤（`/slow`）的工作流 → 8 条 run 并发执行全部 succeeded（running 8 → succeeded 8）；同时发 critical pay 告警 → agent incident 在慢步骤运行期间完整走到 `pending_approval`（monitor→rca rootCause=recent_change→heal L2 dry_run 降级→change riskScore 0.54），证明慢步骤不再阻塞 agent.run。
  - review 修复：`sched.last` map 未初始化 panic（NewService 初始化）、interval last-fire 键含 trigger 下标 + 扫描清理残留键、manual 404 返回真实错误消息。
- **LLM 深化（Phase 2g）**：`deploy/mock_llm.mjs :9998`（SSE 流式 + 按 prompt 区分聚类/RCA）+ 独立实例 :8081（`AIOPS_LLMBASEURL=http://localhost:9998 AIOPS_LLMAPIKEY=dummy AIOPS_LLMMODEL=mock AIOPS_LLMSTREAM=true`）→
  - **流式 RCA**：critical order 告警 → rca 节点 `rootCause=recent_change`（确定性保留，confidence 0.69）+ `llmRootCause`（「order 数据库连接池耗尽（mock LLM）」经 SSE 逐 delta 返回）`llmRationale` `llmSuggestedActions`（3 条）`llmAnalyzedAt`；mock 日志 `stream=true type=rca`。
  - **Heal 回填 LLM 建议**：heal 节点 `reason = 依据 RCA 建议动作优先匹配：rollback、profiling、verify_dependency、重启连接池、回滚 v1.2.3、扩容 db 副本`——确定性建议在前、LLM 补充建议并入（去重），证明 Resolve 收到完整合并列表（建「重启连接池」同名剧本即可命中 LLM 建议）。
  - **聚类摘要接 LLM**：4 条 medium pay → `POST /ai/cluster` → source=ai 事故**同时**有确定性 `summary`（AI 聚类：…已确认生成事故…）与 `llmSummary`（「order 服务并发流量突增…（mock LLM）」）；同键再发 3 条 → `merged:1` 不新建，名称 4→7 条、确定性 summary 与 llmSummary 同步刷新（mock 日志再记一次 `type=cluster` 重增强）。
  - **fail-open**：停掉 mock 后新服务簇（cache）→ 确定性 summary 保留、llmSummary 为空、后端 `WARN ai: cluster llm enhance skipped (keep deterministic)`、建事故不受影响。

---

## 6. 测试与质量

```bash
go build ./... && go vet ./... && gofmt -l internal cmd   # 全绿
go test ./...                                              # 全包通过（含 notifier 10 个单测）
cd web && npm run build                                    # tsc + vite build 通过
```

单测覆盖：notifier（逐渠道 JSON 抓包断言/聚合/失败/跳过/URL 拼接/脱敏 + lifecycle 消息 created/resolved/LifecycleNotifier 发送且 audit=nil 不 panic/Reload 重建渠道含停用空 webhook 未知名忽略/BuildChannel 合法与非法/Supported）、circuit（4）、agent heal/change、ai/cluster（5：分组/gap 断开/键稳定/新增名换键/unknown 服务 + Summarize）、ai/llm（客户端默认值/chatURL 拼接/输出解析含围栏与缺 rootCause/EnhanceRCA 抓包断言请求与解析/401 错误/无 key 不带 Authorization/**EnhanceCluster 抓包断言 + parseClusterOutput 围栏与缺 summary/CompleteStream 流式累计逐 delta/流式中途超时/EnhanceRCA 流式分发与请求体 stream=true/非流式超时/SetTimeout 非正数忽略断言 http.Client.Timeout**）、agent/rca（无 LLM 不产生 llm* 字段/enhance 叠加与输入组装/fail-open 不写 llm* 字段/nil 输出忽略）、agent/heal（+**suggestedActions 合并 llm 建议去重顺序/非字符串忽略/Heal 回填 LLM 建议被 Resolve 收到**）、config（LLM 默认值与 env 命名：AIOPS_LLMAPIKEY/AIOPS_LLMTIMEOUTSEC/AIOPS_LLMSTREAM 生效 + 工作流扩展默认值 + **Phase 3 存储加固：连接池/重试/保留策略默认值与 env 命名（AIOPS_DBMAXOPENCONNS/AIOPS_DBCONNECTRETRIES/AIOPS_RETENTIONENABLED 等）**）、metrics（**Prometheus 文本输出精确行 + 直方图 +Inf 桶 + 未注册指标静默**）、workflow（Parse 校验/触发匹配 severity 范围+service+name/provider/无 filters/DedupKey 稳定/runSteps 顺序+fail-fast/renderTemplate + interval/manual 类型校验/Interval() 解析/httpStep method+headers+bearer 抓包断言/basic auth/renderTemplate nil alert/agentStep nil alert 报错）、alert、cel、incident、pipeline、util。

---

## 7. 下一步（Phase 3 规模化之后）

| 候选 | 验收/目标 | 涉及 |
|---|---|---|
| ~~**LLM 深化**~~（✅ 已交付，Phase 2g） | 聚类摘要也接 LLM（非确定性模板）；LLM 建议动作回填 Heal；流式/超时可控 | ai + agent + web |
| ~~**规模化**~~（✅ 已交付，Phase 3） | Postgres 加固、拓扑影响面（SQL 递归 CTE）、万级压测（10 倍余量达标）；Neo4j 后置仅替换 Impact 查询 | 全局 |
| ~~**规模化延伸**~~（✅ 已交付，Phase 3b/3c/3d） | alerts 月分区 + 保留 DROP、CTE 单语句写入、规则引擎规则缓存、dedup 2 读合并（last_alerts 冗余 alert_hash）；批量写（D2）扫描论证后降级 | 全局 |
| **规模化下一步**（候选） | 生产 HTTP 超时（WriteTimeout/IdleTimeout）按规格调优、`SaveAlerts` 批量写（需批内 overlay）、loadgen 接入 CI 回归基准 | 全局 |

基准报告见 **docs/PERF.md**（根因链/修复清单/服务端观测/建议）。

---

## 8. 工作规范（用户约定，必须遵守）

1. **全局操作先问**：全局删除/发布/设置环境变量等（`taskkill`、`rm -rf`、改全局 env）先确认。
2. **写完模块自测**：每个新模块跑 `go test`，失败自己修到绿。
3. **写完业务 review 一次**：review 发现的问题自己改完再收尾。
4. **中间件用 docker**：PostgreSQL/Redis 等一律 docker-compose 启动。
5. **其余一路绿灯**：普通命令/文件读写/构建直接执行，**不要停下来问权限**。
6. **不确定先 grep**：不要猜、不要编，先看现有实现对齐约定。

---

## 9. 已知注意点

- 前端 `web` 目录下的 `cd` 会让 bash 工作目录改变，跑 go 命令前先 `cd /d/CODE/GOLANG/AIops`。
- mock webhook 收到的通知在 stdout，跨会话后旧输出文件可能失效，重启 node 即可。
- `notification_channels` 表勿删：渠道配置以 DB 为准，env（AIOPS_NOTIFIERCHANNELS + webhook）只作首启种子（EnsureDefaults 只插不覆盖）。多实例共用该表时首启种子可能竞争，但幂等可接受。
- 事故生命周期通知同步发送（事故创建/解决频率低，可接受；勿把通知链路改成每告警同步调）。`notifier.Service.Notify` 已快照渠道后释放锁再发送，勿在持锁时调用取锁方法（`Enabled()` 等）与 `Reload` 写锁互等死锁。
- 审批闭环解决事故必须走 `incident.Service.ResolveSilent`（approve 已有「审批通过」通知）；手动 resolve 走 `Resolve` 触发 `incident.resolved`。改闭环路径时勿误用 `Resolve` 造成双条。
- 熔断器/时钟注入：`circuit.NewBreaker(5, 60*time.Second)`，测试用 cooldown 别写太小（ns 级别会提前过期）。
- `playbook.Resolve` 有补种逻辑（ErrRecordNotFound → EnsureDefaults → 重试），改动勿破坏。
- 审批 reject 时 incident 保持 open，摘要通过 `UpdateNameSummary` append（带换行），勿直接覆盖。
- interval 调度状态（`sched.last`）进程内内存：重启/停启后基线重置，首次触发延迟一个周期（有意避免启动立即突发）；扫描结束会清理已删工作流的残留键。`AIOPS_WORKFLOWSCHEDINTERVALSEC`（默认 10）越小响应越快。
- 工作流执行已异步解耦（`launch` + `execSem`，`AIOPS_WORKFLOWEXECWORKERS` 默认 8）：**勿把 execute 改回同步**，否则慢步骤会回退为阻塞队列 worker 与后续 agent.run；interval/manual 无告警运行时 agent 步骤会明确报错（属预期）。
- interval/manual 运行 `dedup_key` 每次唯一（不互相防重），`uk_wfrun_tenant_workflow_key` 唯一索引只在 alert 触发路径生效；手动运行不做防重，便于反复演练。
- 工作流 http step 的 `with.headers/auth` 值支持 `{{.alert.*}}` 模板；本地验证用 `node deploy/mock_echo.mjs <port>`（回显 method/headers/body，`/slow` 路径延迟 4s 可测慢步骤并发）。
