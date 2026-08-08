# AIOps 告警平台

融合 [keephq/keep](https://github.com/keephq/keep) 与 [multi-agent-aiops](https://github.com/bcefghj/multi-agent-aiops) 思路的告警平台。**双轨架构**：确定性规则热路径 + Agent 编排智能路径。

## 架构

```
                    ┌── 确定性热路径（keep 式）──┐
Alert → process_event ─┤ 去重→入库→CEL 规则关联 Incident ├→ WS 推送 / 审计
                    └── Agent 智能路径（编排器）──┘
                        monitor → rca → heal → change
```

- **热路径**：两级去重（fingerprint 逻辑键 + alert_hash 物理键）、CEL 规则引擎、Incident 聚合、**YAML 工作流（keep 式，`provider: agent` 可切入 Agent 路径）**
- **Agent 路径**：severity≥high、规则命中或工作流显式接入 → `agent.run` 任务 → 编排器状态机（检查点落库）→ Monitor/RCA 根因（可选叠加 LLM 自然语言根因）→ **Heal 自愈（安全护栏链）→ Change 变更风险评估**
- **自愈安全护栏链**：dry-run → 爆炸半径（超限强制降级 L2）→ 熔断器 → L0/L1/L2 分级审批（approve/reject 落审计四元组 who/when/what/why）
- **AI 聚类/摘要**（背景 worker）：扫描未被规则/Agent 承接的散落告警（低/中危）→ 按服务+时间窗确定性聚类 → 达阈值确认生成 `source=ai` Incident（同键合并，名称/摘要随成员刷新）
- **实时推送**：自研 WS hub，告警/事故/编排状态 1 秒内到达前端
- **存储**：PostgreSQL（docker-compose 中间件）

## 快速开始

```bash
# 1. 中间件（PostgreSQL）
make infra            # docker compose -f deploy/docker-compose.yml up -d postgres

# 2. 后端（:8080）
make app              # go run ./cmd/aiops

# 3. 前端（:5173，Vite 代理 /api → :8080）
make web              # cd web && npm run dev
```

打开 http://localhost:5173（告警 / Incidents 页面）。

## 端到端验收（已通过）

```bash
# ① 打 Prometheus Alertmanager webhook → 前端 1 秒内出现（WS alert.new）
curl -X POST localhost:8080/api/v1/webhook/prometheus -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"pay-down","severity":"critical","service":"pay"},"annotations":{"summary":"pay service down"},"startsAt":"2026-08-07T10:00:00Z"}]}'

# ② 相同 payload 重复发送 → 去重丢弃（alerts total 不变）
# ③ 规则命中自动建 Incident：POST /api/v1/rules 配置 CEL 规则后，命中告警建 source=rule 的事故
# ④ 高严重度告警进入 Agent 流水线，Incident 详情可见 nodeResults（monitor + rca 根因/置信度/建议动作）

## Phase 2a：自愈 + 审批闭环（已通过）

```bash
# ① critical 告警 → 编排器产出 heal（自愈动作）+ change（风险评分），Incident 进入 pending_approval
curl -X POST localhost:8080/api/v1/webhook/prometheus -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"pay-slow","severity":"critical","service":"pay"},"annotations":{"summary":"pay latency spike"},"startsAt":"2026-08-07T10:00:00Z"}]}'
# → GET /api/v1/incidents/:id 见 nodeResults.heal（action/level/blastRadius/downgraded/status=dry_run）
#   与 nodeResults.change（riskScore/level/breakdown 5 维），state.status=pending_approval

# ② 审批（安全护栏链最后一道）：approve 闭环 / reject 保持 open 人工跟进
curl -X POST localhost:8080/api/v1/incidents/:id/approval -H 'Content-Type: application/json' \
  -d '{"decision":"approve","actor":"oncall-engineer","reason":"批准回滚"}'

# ③ 审计轨迹（who/when/what/why，correlation_id 贯穿）
curl localhost:8080/api/v1/incidents/:id/audits

# ④ 自愈剧本（数据驱动，可 UI 管理；未配置自动补种默认剧本）
curl localhost:8080/api/v1/playbooks
```

爆炸半径超限动作自动降级 L2（如 rollback 固有 0.35 > 0.2 阈值）；熔断器 5 次失败开启 60s 阻断自愈。

## 通知渠道（钉钉/飞书/企微/HTTP）

审批/处置结果经 `internal/notifier` 推送：进入 `pending_approval`、审批通过、审批拒绝都会通知并落审计（`notify.sent/failed/skipped`），未配置 webhook 的渠道自动跳过（fail-open，不影响主流程）。**事故新建（rule/agent/AI 任一来源）与事故解决**也会推送生命周期通知（`incident.created` / `incident.resolved`）。渠道配置支持 **UI 管理**（「通知渠道」页，保存即热生效）：env 仅作首启种子，DB `notification_channels` 表为准。

```bash
# 环境变量（docker-compose 或进程环境）：
#   AIOPS_NOTIFIERCHANNELS=dingtalk,feishu   # 逗号分隔启用渠道
#   AIOPS_NOTIFIERDINGTALK=https://oapi.dingtalk.com/robot/send?access_token=xxx
#   AIOPS_NOTIFIERFEISHU=https://open.feishu.cn/open-apis/bot/v2/hook/xxx
#   AIOPS_NOTIFIERWECOM=https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx
#   AIOPS_NOTIFIERHTTP=http://localhost:9999/webhook   # 通用 webhook（POST JSON Message）
#   AIOPS_NOTIFIERBASEURL=http://localhost:5173         # 前端地址（拼接 incident 详情链接）

# 手动测试渠道（前端 Incident 详情页「测试通知」按钮亦可用）：
curl -X POST localhost:8080/api/v1/notify/test -H 'Content-Type: application/json' \
  -d '{"title":"AIops 通知测试","severity":"critical"}'
# → {"channels":["dingtalk"],"results":[{"channel":"dingtalk","ok":true}]}
```

无真实机器人时可用 `AIOPS_NOTIFIERCHANNELS=http` + `AIOPS_NOTIFIERHTTP=<本地 mock>` 本地演练。

## Phase 2c：AI 聚类/摘要（已通过）

未被规则/Agent 承接的散落告警由后台 worker 定期聚类：按 `labels.service` 分组、时间窗断开、达 `AIOPS_AICLUSTERTHRESHOLD`（默认 3）确认生成 `source=ai` Incident（同 `cluster_key` 合并、名称/摘要随成员刷新）。聚类与摘要本身**确定性实现、不依赖 LLM**；配置 `AIOPS_LLMAPIKEY` 后可选叠加 LLM 自然语言摘要（Phase 2g，见下）。

```bash
# ① 发 30 条 medium 告警（每服务 ≥3，服务/告警名/pod 不同保证独立指纹）
# ② 手动触发一次聚类（worker 默认每 60s 也会自动跑）
curl -X POST localhost:8080/api/v1/ai/cluster
# → {"scanned":30,"clusters":3,"created":[3 个 source=ai Incident（中危 pay/order/search（N 条关联告警））],"merged":0}
# ③ 重跑 → scanned 0（全部已归属，防重复）；同键新告警 → merged 增加且不新建，名称刷新条数
```

配置：`AIOPS_AICLUSTERENABLED=true` / `AIOPS_AICLUSTERTHRESHOLD=3` / `AIOPS_AICLUSTERWINDOWMIN=10` / `AIOPS_AICLUSTERINTERVALSEC=60`。

## Phase 2d：YAML 工作流引擎（已通过）

keep 式 YAML 工作流：用户以 `workflow{id,triggers,filters[]}` + `steps[]`/`actions[]` 声明「告警命中 → 异步执行」，每步 `provider{type,with}`。内置 `provider: agent` 步骤可将告警接入 Agent 编排器（如 medium 告警默认不进 Agent 路径，通过工作流显式接入）。

**并发防重（验收④）**：`workflow_runs` 表唯一索引 `(tenant_id, workflow_id, dedup_key)`，`dedup_key=SHA256(workflow_id:alert.fingerprint)`；并发首达仅一个赢者，同一工作流对同一告警只执行一次。

```yaml
workflow:
  id: order-medium-agent-demo        # 种子演示工作流（零配置补种）
  name: 订单中危告警自动接入 Agent 编排
  triggers:
    - type: alert
      filters:
        - key: service
          value: order
        - key: severity
          value: ">=medium"
  steps:
    - name: enrich
      provider: { type: log, with: { message: "命中 {{.alert.name}}" } }
  actions:
    - name: run-agent
      provider: { type: agent }
```

```bash
# ① 命中告警（medium order，seed 工作流）→ provider: agent → 编排器 monitor→rca→heal→change → pending_approval
curl -X POST localhost:8080/api/v1/webhook/prometheus -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"order-slow","severity":"medium","service":"order","pod":"order-1"},"annotations":{"summary":"order latency spike"},"startsAt":"2026-08-07T12:00:00Z"}]}'

# ② 管理 + 运行记录
curl localhost:8080/api/v1/workflows        # 工作流 CRUD（YAML 解析校验）
curl localhost:8080/api/v1/workflow-runs    # 并发防重后的实际执行次数
# ③ 同指纹并行发 20 次 → workflow-runs 仅 1 条（验收④）
```

配置：`AIOPS_WORKFLOWENABLED=true`（默认，false 关闭 pipeline 投递）。

## Phase 2d+：RCA LLM 增强（已通过）

确定性贝叶斯证据链之外，可配置 OpenAI 兼容 LLM（DeepSeek / Ollama / vLLM / LiteLLM）补充**自然语言根因与建议动作**，写入 RCA 节点结果的 `llm*` 字段。**可选增强、fail-open**：未配置 key / LLM 不可达 / 输出无法解析时保持纯确定性结论，不阻塞编排。

```bash
# 配置（进程 env）：
#   AIOPS_LLMAPIKEY=sk-xxx                # 配置后启用（默认 openai-compat）
#   AIOPS_LLMBASEURL=https://api.deepseek.com  # OpenAI 兼容端点（默认）
#   AIOPS_LLMMODEL=deepseek-chat                # 默认

# 本地演练：mock OpenAI 端点 :9998（无需真实 key）
node deploy/mock_llm.mjs 9998
# 后端启动 env 追加：AIOPS_LLMBASEURL=http://localhost:9998 AIOPS_LLMAPIKEY=dummy

# 建拓扑（RCA 确定性证据）→ 发 critical 告警 → Incident 详情 RCA 卡片「LLM 补充分析」
curl -X POST localhost:8080/api/v1/topology -H 'Content-Type: application/json' \
  -d '{"name":"order","kind":"service","dependsOn":["db"],"recentChanges":["v1.2.3 deploy"]}'
curl -X POST localhost:8080/api/v1/webhook/prometheus -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"llm-demo","severity":"critical","service":"order"},"annotations":{"summary":"order api latency spike"},"startsAt":"2026-08-07T13:00:00Z"}]}'
# → GET /api/v1/incidents/:id 见 nodeResults.rca.llmRootCause / llmRationale / llmSuggestedActions
```

设计：`internal/ai/llm.go`（OpenAI 兼容 Chat Completions 客户端 + `EnhanceRCA` 证据链 prompt / JSON 解析）；RCAAgent 注入可选 `RCALLM`，LLM 结论只作补充视角，确定性 `rootCause/confidence/suggestedActions` 保留。

## Phase 2e：通知补充（已通过）

**事故生命周期通知**：事故新建（rule/agent/AI 聚类任一来源）与事故解决自动推送渠道，无需人工触发。审批闭环用 `ResolveSilent`（approve 已有「审批通过」通知，不重复发「已解决」）。

**渠道管理 UI**：渠道配置存 DB（env 仅首启种子，保存即热生效，重启不回退），前端「通知渠道」页管理启用开关 + webhook 地址 + 单渠道测试。

```bash
# 渠道配置（env 首启种子后以 DB 为准）
curl localhost:8080/api/v1/notify/channels                                   # 列表（含未启用）
curl -X PUT localhost:8080/api/v1/notify/channels/http -H 'Content-Type: application/json' \
  -d '{"enabled":true,"webhook":"http://localhost:9999/webhook"}'             # 保存即热重载（停用=静默）
curl -X POST localhost:8080/api/v1/notify/channels/http/test                  # 单渠道测试（无需启用）

# 事故生命周期通知（mock webhook :9999 可见）
# 任意新事故（critical 告警 / rule 命中 / AI 聚类确认）→ mock 收到「【新事故】…」（action=created）
curl -X POST localhost:8080/api/v1/incidents/<id>/resolve                      # 手动解决 → 「【事故已解决】…」（action=resolved）
```

设计：`incident.Service` 可选 `IncidentNotify` 钩子（`Create`/`Resolve` 触发，`SetNotifyFunc` 注入 `notifier.LifecycleNotifier`，fail-open 不阻塞）；`notifier.ChannelConfig` + `ConfigService`（DB 持久化）+ `Service.Reload` 热重载（RWMutex 快照，不换指针）。

## Phase 2f：工作流扩展（已通过）

- **interval 触发**：`triggers: [{type: interval, with: {interval: "5m"|30}}]` 定时执行（无告警上下文，适合巡检/心跳）；`manual` 触发通过 API/前端「运行」按钮，可选携带告警。
- **http step 扩展**：`with.method`（默认 POST）/ `with.headers`（值支持 `{{.alert.*}}` 模板）/ `with.auth`（basic / bearer）。
- **并发执行**：工作流执行从队列 worker 解耦（异步有界并发，`AIOPS_WORKFLOWEXECWORKERS` 默认 8）——慢步骤不再阻塞后续 agent.run。

```bash
# ① 建 interval 工作流 → 每周期自动产生一条运行（trigger=interval）
curl -X POST localhost:8080/api/v1/workflows -H 'Content-Type: application/json' -d '{"name":"定时演示","enabled":true,"yaml":"workflow:\n  id: wf-timer\n  triggers:\n    - type: interval\n      with:\n        interval: 5s\n  steps:\n    - name: ping\n      provider: { type: log, with: { message: \"tick\" } }"}'
# ② 手动触发任意启用工作流（前端「运行」按钮等价）
curl -X POST localhost:8080/api/v1/workflows/<wf-id>/run -H 'Content-Type: application/json' -d '{}'
# → {"runId":...,"trigger":"manual","status":"running"}；最近运行表可见 trigger=manual
# ③ http step headers/auth 本地回显验证
node deploy/mock_echo.mjs 9996    # 回显 method/headers/body；/slow 延迟 4s 测慢步骤
# ④ 运行记录带触发来源：GET /workflow-runs → trigger 列为 alert|interval|manual
```

设计：`Trigger.With.interval` 解析（Go duration 或秒）；`scheduler.go` 定时扫描启用工作流（last-fire 键含 trigger 下标 + 残留键清理）；`launch` 落库后异步有界执行（execSem，panic 转 failed + 10min 超时）；`RunManual` 每次 dedup 键唯一不做防重。

## Phase 2g：LLM 深化（已通过）

- **聚类摘要也接 LLM（非确定性模板）**：配置 key 后，AI 聚类确认/合并事故时用 LLM 生成自然语言摘要，写入 `Incident.llmSummary`（新列，**与确定性 `summary` 并存**；LLM 失败仅 warn、保留确定性摘要、不阻塞建事故）。建事故先落库再增强（无孤儿窗口）。前端事故详情展示「LLM 聚类摘要」紫色区块。
- **LLM 建议动作回填 Heal**：Heal 的剧本匹配列表 = 确定性 `suggestedActions` + LLM 补充 `llmSuggestedActions`（去重、顺序保留）——建与 LLM 建议同名的剧本即可命中（如「重启连接池」）。
- **流式/超时可控**：`AIOPS_LLMSTREAM=true`（默认）走 SSE 流式响应（逐 delta 累计）；`AIOPS_LLMTIMEOUTSEC=15`（默认）控制整体超时（流式累计期间同样生效）。

```bash
# 配置：AIOPS_LLMAPIKEY=sk-xxx（可叠加 AIOPS_LLMSTREAM / AIOPS_LLMTIMEOUTSEC）
# 本地演练：node deploy/mock_llm.mjs 9998（SSE 流式，按 prompt 自动区分聚类/RCA）
# 聚类摘要：≥3 条同服务 medium 告警 → POST /ai/cluster → 新 source=ai 事故详情同时有 summary 与 llmSummary
curl -X POST localhost:8080/api/v1/ai/cluster
# Heal 回填：critical 告警 → heal 节点 reason 可见「确定性建议、LLM 建议」合并列表；建「重启连接池」剧本即命中 LLM 建议
```

设计：`internal/ai/llm.go`（`CompleteStream` SSE 解析 + `SetTimeout`/`SetStream` + `EnhanceCluster`/`ClusterLLM` 接口）；`ai.Service.SetClusterLLM` 注入，`confirm`/`refreshMerged` best-effort 增强（失败 warn）；`agent/heal.go` 合并 `llmSuggestedActions`；mock `deploy/mock_llm.mjs` 支持流式与聚类/RCA 双场景。

## 项目结构

```
cmd/aiops/          装配根（config→store→pipeline→orchestrator→api→worker）
internal/
  alert/            Alert/LastAlert 模型 + 仓储 + 两级去重
  pipeline/         事件管线（去重/入库/规则关联/WS/Agent 触发）
  incident/         Incident/Rule/State 模型 + CEL 规则引擎 + 编排器检查点
  cel/              CEL 引擎封装（预编译缓存）
  orchestrator/     编排器状态机 + agent.run worker + 审批
  agent/            Agent 接口 + Monitor/RCA/Heal/Change
  playbook/         自愈剧本（数据驱动 + 默认种子）
  circuit/          熔断器（安全护栏链）
  notifier/         通知渠道（钉钉/飞书/企微/HTTP webhook + 审计联动）+ 生命周期通知（lifecycle.go）+ 渠道配置 DB 化（config_service.go，热重载）
  maintenance/      维护窗口（热路径静默：Window CRUD + 匹配 + WindowFilter 缓存）
  ai/               AI 聚类/摘要（确定性 cluster/summarize/service + 定时 worker）+ OpenAI 兼容 LLM 客户端（llm.go，RCA 增强）
  workflow/         YAML 工作流引擎（keep 式 def/match/service + workflow_runs 并发防重 + interval 调度器 + 异步有界执行）
  provider/         数据源抽象 + 注册表（prometheus 等）
  topology/         服务拓扑（RCA 事实来源）
  eventbus/         事件总线 + 审计日志（AuditService 落库）
  queue/            异步任务队列（内存 worker 池，按类型分发 agent.run / workflow.trigger）
  pusher/           WebSocket hub
  auth/             API key 认证
  api/              Gin 路由 + 中间件 + handlers
  config/ store/ util/
web/               Vite + React 19 + AntD 前端
deploy/            docker-compose（PostgreSQL）
```

## 阶段路线图

- **Phase 1**：MVP 管线 + Monitor/RCA Agent 雏形 ✅
- **Phase 2a**：Heal（Playbook + 熔断器 + 爆炸半径 + 分级审批）+ Change（5 维风险评分）✅
- **Phase 2b**：通知渠道（钉钉/飞书/企微/HTTP）✅、维护窗口 ✅
- **Phase 2c**：AI 聚类/摘要 ✅
- **Phase 2d**：YAML 工作流引擎 ✅、RCA LLM 增强 ✅
- **Phase 2e**：通知补充 ✅（事故生命周期通知 + 渠道管理 UI）
- **Phase 2f**：工作流扩展 ✅（interval/manual 触发 + http step headers/认证 + 并发执行）、LLM 深化
- **Phase 2g**：LLM 深化 ✅（聚类/摘要/RCA 增强全链路 LLM 可选 + 流式）
- **Phase 3**：规模化 ✅（Postgres 加固：连接池/启动重试/索引/保留策略；拓扑影响面 SQL 递归 CTE；万级压测 **1786/s**（≈10.7 万/min）10 倍余量达标；Neo4j 后置仅替换 Impact 查询）
- **Phase 3b**：写入优化 ✅（SaveAlert 单 SQL 双表写入——每事件往返 4→1，P50 61→51ms；alerts 按 created_at 月分区 + 保留策略整分区 DROP；loadgen `-batch` 批量扫描论证批量写降级；`AIOPS_ALERTSPARTITIONS` 默认 true）
- **Phase 3c**：规则引擎规则缓存 ✅（按租户缓存启用规则，TTL 内 0 次规则表读；`POST /rules` 即失效；P50 51→47.4ms、P99 88→80.7ms）
- **Phase 3d**：dedup 单读 ✅（`alert_hash` 冗余进 `last_alerts`，去重分类 2 读合并为 1 次唯一索引读，不再回读 alerts 分区表；启动幂等回填存量行；P50 47.4→33.1ms、P99 80.7→61.9ms——至此逐事件 DB 往返 4→2 次）
- **Phase 3e**：生产 HTTP 超时调优 ✅（`ReadTimeout` 15s / `WriteTimeout` 30s / `IdleTimeout` 120s + `ReadHeaderTimeout` 5s 防 slowloris；WS 端点 Accept 前显式清除 net/http 残留 deadline 防长连接被误杀；`AIOPS_HTTP*TIMEOUTSEC` 可调）
- **Phase 3f（当前最新）**：loadgen 接入 CI 回归基准 ✅（`-summary` JSON 汇总留痕 + `-compare` 基线对比判定：吞吐 <0.8× 或 P50/P99 >1.5× 基线非零退出、profile 须一致；Makefile `make bench` / `make regress` / `make regress-baseline`，本地无 remote 也能当回归看门狗）

详见 `docs/`：进度 [docs/PROGRESS.md](docs/PROGRESS.md)、压测基准 [docs/PERF.md](docs/PERF.md)。
