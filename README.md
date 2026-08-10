# AIOps 告警平台

由Golang搭建的AI告警平台。**双轨架构**：确定性规则热路径 + Agent 编排智能路径。

## 架构

```
                    ┌── 确定性热路径             ──┐
Alert → process_event ─┤ 去重→入库→CEL 规则关联 Incident ├→ WS 推送 / 审计
                    └── Agent 智能路径（编排器）──┘
                        monitor → rca → heal → change
```

- **热路径**：两级去重（fingerprint 逻辑键 + alert_hash 物理键）、CEL 规则引擎、Incident 聚合、**YAML 工作流（`provider: agent` 可切入 Agent 路径）**
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

## 数据安全与访问保护（KEEPON M1）

**访问保护（可选，默认 noauth）**：设置 `AIOPS_AUTHTYPE=apikey` 并重启后端：

1. 首次启动时控制台打印 **owner key（仅显示一次）**——前端自动进入解锁页，粘贴后凭据仅存于浏览器会话（`sessionStorage`）。
2. REST 统一携带 `X-API-Key`；WebSocket 用短期 ticket 握手（密钥不进 URL）。
3. 「设置 → 访问密钥」可新建/撤销密钥、重新生成 owner key；`/webhook` 等外部接入同样带 `X-API-Key`。
4. 密钥丢失：清空 `api_keys` 表后重启会自动生成新 owner key（幂等引导）。

**日志持久化**：设置 `AIOPS_LOGFILE=./logs/aiops.log` 后 stdout 与文件双写，按大小/数量/天数轮转（默认 ≈1GB 上限）；全局脱敏（apiKey/密钥/token/URL 查询参数 → `[REDACTED]`），请求 ID 贯穿 HTTP 访问日志。

**数据备份**：设置 `AIOPS_BACKUPENABLED=true` 后每 24h 自动 `pg_dump -Fc`（启动补跑、数量+容量双限轮转、默认备份后恢复校验）。「设置 → 数据备份」可查看记录/手动触发；恢复仅通过 `bash scripts/restore.sh`（演练 `--verify` / 真实恢复 `--target`，二次确认），详见 `docs/RESTORE.md`。

## 总览大屏与一条命令接入（KEEPON M2）

**总览与平板大屏**：打开即进入「总览」首页——活跃告警/事故/执行中任务/知识条目卡片、24h 告警趋势、严重度分布、最近事件（刷新后仍在，数据来自 DB 重查）、系统健康与最近备份。访问 **`http://localhost:5173/wallboard`** 进入暗色大字只读大屏（30s 自动刷新，适合挂墙/平板常驻）。

**一条命令接入告警**（个人脚本/任意监控系统）：总览或告警页空状态有可复制 curl（自动带平台地址与认证头）+「发送测试告警」按钮：

```bash
curl -X POST http://localhost:8080/api/v1/webhook/generic \
  -H 'Content-Type: application/json' \
  -d '{"title":"磁盘使用率过高","severity":"warning","status":"firing",
       "source":"my-script","dedupKey":"node-1-root-disk",
       "labels":{"host":"node-1","disk":"/"},"description":"当前 92%"}'
```

- `severity` 支持别名（p0/severe/error/warning/warn/notice），`status` 为 `firing|resolved`（恢复时发 `resolved` 即自动复位）。
- `dedupKey` 可选：同 `dedupKey` 的重复告警走更新而非新增；`description` 含变化内容（如"当前 92%"）不影响去重。
- 响应回显归一化 severity 与指纹，便于调试。

**通知节流**（默认开启，无需配置）：同一告警首条立即通知，冷却期内重复合并为周期汇总（"过去 30 分钟重复 47 次"），严重度升级与恢复立即通知；决策全部落审计（`notify.sent/throttled/summary`，可解释为何发送/被节流）。

## 结案即知识（KEEPON M3）

**结案发布**：事故详情页「结案并发布到知识库」——表单自动带出故障现象/关联告警/RCA 根因（确定性证据与 LLM 推测明确标注来源）；保存后结案数据立即进入知识索引（verified+success → **T3**），下方三态展示：`事故已解决 / 结案已填写 / 知识已索引`（索引中 3s 自动刷新，失败显示原因，`rejected` 结案退出召回）。

**个人知识源（runbook/SOP）**：「知识库 → 知识源」拖拽上传 .md——支持 front matter（title/service/tags/updated_at）、发布前预览解析与脱敏检查（敏感串自动标出、提示注入警告）、发布即入索引（**T2**）、编辑生成新版本（旧版 superseded 保留）、删除同步撤销文档与向量；检索结果被标记 unsafe 后自动隔离，重新发布新版本即恢复。

**检索评测（golden set）**：「知识库 → 评测集」维护个人真实查询（每条标注期望命中的事故/runbook），运行：

```bash
go run ./cmd/rag-eval -topk-final 6 -json .bench/eval1.json          # 基线
go run ./cmd/rag-eval -topk-final 10 -json .bench/eval2.json -baseline .bench/eval1.json  # 调参对比
```

输出 Recall@K（候选/选中双口径）、首条命中率、空结果率、延迟与降级分布；`-fail-on-regression` 可接 CI。

## 数据接入（Prometheus / Grafana / 大模型 API）

**Prometheus / Alertmanager（webhook 推送）**：Alertmanager 配置 receivers 指向本平台即可：

```yaml
# alertmanager.yml
route:
  receiver: aiops
receivers:
  - name: aiops
    webhook_configs:
      - url: http://<aiops-host>:8080/api/v1/webhook/prometheus
        send_resolved: true
```

验证：`curl -X POST localhost:8080/api/v1/webhook/prometheus -d '{...Alertmanager 格式...}'` → `{accepted, processed}`；或发一条真实告警后 `GET /api/v1/alerts` 可见。

**Grafana Alerting（webhook 推送）**：Grafana → Alerting → Contact points → 新建 webhook 类型联系人，URL 填 `http://<aiops-host>:8080/api/v1/webhook/grafana`，通知策略引用该联系人。Grafana Unified Alerting 负载自动归一化（labels/annotations/generatorURL/values 入告警详情）。验证：`curl -X POST localhost:8080/api/v1/webhook/grafana -d '{...Grafana webhook 格式...}'`。

**大模型 API 接入**：Chat LLM（RCA 根因补充/聚类摘要）走前端「模型配置」页（或 `model_configs` 表），env 的 `AIOPS_LLM*` 仅首启种子，之后 DB 为准。Embedding（RAG 向量化）**无配置入口**——由 `AIOPS_EMBEDDING*` 环境变量直接配置（千问示例见配置表）。均**保存/修改后重启后端生效**；管理端点需 `AIOPS_MODELCFGADMINAPIS=true`（见配置表）。

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

YAML 工作流：用户以 `workflow{id,triggers,filters[]}` + `steps[]`/`actions[]` 声明「告警命中 → 异步执行」，每步 `provider{type,with}`。内置 `provider: agent` 步骤可将告警接入 Agent 编排器（如 medium 告警默认不进 Agent 路径，通过工作流显式接入）。

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
  embedding/        RAG 向量化 Provider（openai-compat /embeddings 客户端 + mock，独立于 Chat LLM）
  rag/              知识检索层：知识表模型 + 显式 schema（pgvector/pg_trgm）+ 持久化索引 worker（SKIP LOCKED/退避/死信/回填/对账）+ 混合召回（精确/关键词/向量）+ RRF 融合 + 相似事故证据包
  workflow/         YAML 工作流引擎（def/match/service + workflow_runs 并发防重 + interval 调度器 + 异步有界执行）
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
- **Phase 3f**：loadgen 接入 CI 回归基准 ✅（`-summary` JSON 汇总留痕 + `-compare` 基线对比判定：吞吐 <0.8× 或 P50/P99 >1.5× 基线非零退出、profile 须一致；Makefile `make bench` / `make regress` / `make regress-baseline`，本地无 remote 也能当回归看门狗）
- **Phase 3g（当前最新）**：SaveAlerts 批量写 ✅（dedup 批量读 + 单语句多行 CTE 批量写合并为每批 2 次 DB 往返；批内 overlay 去重（禁裸延迟 flush）；last_alerts Go 侧折叠避免 PG「cannot affect row a second time」；批量读失败回退逐事件；`make bench-batch` / `make regress-batch` / `make regress-baseline-batch`——batch20 请求延迟 **973→197ms（-80%）**、失败率 5%→0%）

## 维护窗口（Phase 2b）

计划性维护期间对告警热路径**静默**：命中窗口的 **firing** 告警以 `suppressed` 状态入库，**不触发**规则关联 / Incident / Agent 路径（resolved 一律放行，保证恢复链路）。窗口匹配需至少一个条件，且所有已配置条件全部满足；`severity` 为「覆盖 ≤ 该严重度」。

```bash
# ① 建窗口：周三 00:00–01:00 静默 pay 服务 ≤high 告警
curl -X POST localhost:8080/api/v1/maintenance-windows -H 'Content-Type: application/json' \
  -d '{"name":"pay 周三维护","service":"pay","severity":"high","startsAt":"2026-08-12T00:00:00Z","endsAt":"2026-08-12T01:00:00Z","reason":"DB 迁移","enabled":true}'
# ② 窗口期内发 pay 的 medium/critical 告警 → 返回 processed，但入库 status=suppressed、
#    广播 alert.suppressed，不建 Incident、不进 Agent
# ③ 管理：GET/PUT/DELETE /api/v1/maintenance-windows[:id]（UI「维护窗口」页等价）

# 注意：窗口 CRUD 后热路径缓存最长 5s 内生效（WindowFilter TTL）；窗口期内的 suppressed 同内容告警按 duplicate 处理
#（避免窗口期心跳刷表），窗口失效后同内容复发重新上浮为 firing。
```

配置：窗口 CRUD 走 API/UI；`WindowFilter` 缓存 TTL 固定 5s（`pipe.InvalidateMaintenance(tenant)` 由 API 层自动调用）。

## API 端点一览

全部业务端点挂在 `/api/v1` 下（`authMiddleware` 注入租户；`noauth` 模式固定 `default` 租户，`apikey` 模式校验 `X-API-Key`）。`GET /healthz`、`GET /metrics`、`GET /api/v1/auth/status` 不挂鉴权；`GET /api/v1/ws` 用短期 ticket 握手（`GET /api/v1/ws/ticket` 换取，30s 一次性，长期密钥不进 URL）。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/webhook/:provider` | 接收告警 push（provider 为注册表 kind：`prometheus` / `grafana` / `generic`），批量归一化入管线 → `{accepted, processed, events}` |
| POST | `/webhook/generic` | **通用告警入口（一条 curl 接入）**：`{title, severity, status, source, dedupKey, labels, description}`；severity 别名归一化（p0→critical、error→high、warning→medium、notice→info）；description 只进快照不进指纹；非法 severity/status → 400 清晰错误 |
| GET | `/alerts` | 告警列表 `?status=&severity=&provider=&from=&to=&limit=&offset=`（created_at 倒序） |
| GET | `/alerts/:id` | 告警详情（DTO） |
| GET | `/alerts/:id/history` | 同 fingerprint 去重演进历史（≤50 条） |
| POST | `/alerts/:id/ack` | 确认告警（认领）→ status=`acknowledged` |
| GET | `/incidents` | 事故列表 `?status=&limit=&offset=` |
| GET | `/incidents/:id` | 事故详情（聚合关联告警 + 编排器状态 `state` + `nodeResults` monitor/rca/heal/change） |
| POST | `/incidents/:id/resolve` | 手动解决（闭环）→ `{status:"resolved"}` |
| PUT | `/incidents/:id/resolution` | 结案数据（仅已解决事故）：`{confirmedRootCause, resolution, outcome, verification, verificationStatus, postmortemUrl, resolvedBy}`，version 递增（RAG 知识来源） |
| POST | `/incidents/:id/approval` | 分级审批 `{decision: approve\|reject, actor, reason}` → approve 闭环 resolved / reject 保持 open |
| GET | `/incidents/:id/audits` | 审计轨迹（who/when/what/why，correlation_id 贯穿） |
| GET | `/incidents/:id/similar` | 相似历史事故（RAG 只读；关闭时 `{enabled:false}`）：来源/可信等级/处置结果/分数/理由 |
| GET | `/incidents/:id/knowledge-status` | 结案→发布→索引三态（KEEPON M3）：`{resolved, resolutionFilled, indexed, document{trustLevel,status,sourceVersion}, job{status,lastError}}` |
| POST | `/incidents/:id/rag-feedback` | 检索反馈 `{rating: useful\|not_useful\|wrong\|unsafe, documentId, traceId?}`；unsafe 立即 quarantine 文档 |
| GET | `/incidents/:id/rag-trace` | 本次事故检索轨迹（管理权限，`AIOPS_RAGADMINAPIS`） |
| GET | `/rag/status` | 索引/向量一致性/积压/失败任务/**文档级统计**（管理权限） |
| POST | `/rag/reindex` | 全量重建索引（异步入队，202；进度见 rag/status，管理权限） |
| GET/POST/PUT/DELETE | `/rag/golden-queries` | 检索评测集 CRUD（`{query, expectedSources[], tags, enabled, useful?}`；管理权限） |
| POST | `/rag/sources/preview` | 知识源上传预览（front matter 解析 + 脱敏检查，无副作用；管理权限） |
| GET/POST | `/rag/sources` | 知识源列表（含派生索引状态）/ 创建草稿 |
| GET/PUT | `/rag/sources/:id` | 详情（编辑回显）/ 更新（published 且内容变化 → 新版本自动发布） |
| POST | `/rag/sources/:id/publish` | 发布（Sanitize 预检 → 版本+1 → 入队索引） |
| DELETE | `/rag/sources/:id` | 删除（软删 + 撤销文档/向量） |
| POST | `/notify/test` | 全渠道测试 `{title, content, severity}` → `{channels, results[]}` |
| GET | `/notify/channels` | 渠道配置列表（含未启用） |
| PUT | `/notify/channels/:name` | 更新渠道 `{enabled, webhook}`（保存即热重载；停用=静默） |
| POST | `/notify/channels/:name/test` | 单渠道测试（无需启用） |
| GET/POST | `/playbooks` | 自愈剧本列表 / 创建 |
| PUT/DELETE | `/playbooks/:id` | 更新 / 删除剧本 |
| GET/POST | `/maintenance-windows` | 维护窗口列表 / 创建 |
| PUT/DELETE | `/maintenance-windows/:id` | 更新 / 删除窗口 |
| POST | `/ai/cluster` | 手动触发一次 AI 聚类 → `{scanned, clusters, created[], merged}` |
| GET/POST | `/workflows` | 工作流列表 / 创建（`yaml` 原文） |
| PUT/DELETE | `/workflows/:id` | 更新 / 删除（更新时 `enabled` 用指针判定「未传」与 `false`） |
| POST | `/workflows/:id/run` | 手动触发（`{alert_id?}`）→ `{runId, trigger:"manual", status}` |
| GET | `/workflow-runs` | 运行记录（`trigger` 列 alert\|interval\|manual） |
| GET/POST | `/rules` | CEL 关联规则列表 / 创建（`{name, celExpression, groupingCriteria[], threshold, runAgent, enabled}`） |
| GET | `/providers` | 已注册数据源 provider 列表（prometheus / grafana / generic） |
| GET/PUT/POST | `/model-configs` / `/model-configs/:key` / `/model-configs/:key/test` | 大模型 API 配置管理（仅 Chat LLM 一行，apiKey 脱敏；PUT 保存、test 连通性；管理权限 `AIOPS_MODELCFGADMINAPIS`；Embedding 由 env 配置不走此端点） |
| GET/POST | `/topology` | 服务拓扑节点列表 / 创建（`{name, kind, dependsOn[], recentChanges[]}`） |
| GET | `/topology/:name/impact` | 影响面 `?depth=N`（SQL 递归 CTE 反向遍历依赖图） |
| GET/POST | `/apikeys` | 密钥列表（含 owner 标记）/ 创建 → `{apiKey(仅此一次明文), tenantId}` |
| DELETE | `/apikeys/:id` | 撤销密钥（删除后立即失效） |
| POST | `/apikeys/owner/regenerate` | 重新生成 owner key（旧 owner 撤销，明文一次） |
| GET | `/auth/status` | 认证状态 `{mode: noauth\|apikey, hasKeys}`（前端解锁页守卫；免鉴权） |
| GET | `/ws/ticket` | 签发 WS 握手短期 ticket `{ticket, ttlSec:30}`（需 X-API-Key） |
| GET | `/ws` | WebSocket 实时推送（`?ticket=` 握手；事件：`alert.new/update/duplicate/suppressed`、`incident.upsert`、`incident.state`） |
| GET/POST | `/backups` | 备份记录列表 / 手动触发（运行中 409；未启用 `AIOPS_BACKUPENABLED` 时 404） |
| GET | `/overview` | **总览聚合**（`?window=24h`，上限 7d）：活跃计数 + 24h 趋势 + 严重度分布 + 最近事件（DB 重查）+ 系统健康/备份状态；默认首页与 `/wallboard` 数据源 |
| GET | `/infra/metrics` | **基础设施指标**（监控平板数据源）：主机 CPU/内存/磁盘/Go 运行时 + PostgreSQL 连接/缓存命中/库大小 + docker 容器资源占用 + 近 60 点采样历史 |
| POST/GET | `/assistant/threads` / `/assistant/threads/:id` | **AI 助手**：会话创建/列表/详情（软删 DELETE；消息列表 GET `/:id/messages`） |
| POST | `/assistant/threads/:id/messages` | 发送消息（**必须带 `Idempotency-Key` 头**防重复；202 → `{threadId,messageId,runId}`） |
| GET | `/assistant/runs/:id` | run 详情（状态/错误码/步骤） |
| GET | `/assistant/runs/:id/events` | **run 事件 SSE 流**（`?lastSequence=N` 断线续传；事件：run.* / message.delta / tool.* / heartbeat） |
| POST | `/assistant/runs/:id/cancel` | 取消 run（仅 queued/running 可取消） |
| GET | `/healthz` | 健康检查 |
| GET | `/metrics` | Prometheus 文本格式指标（含 `aiops_backup_state` 等备份指标） |

## 配置项（环境变量，前缀 `AIOPS_`）

| 分组 | 变量 | 默认 | 说明 |
|---|---|---|---|
| 服务 | `AIOPS_ADDR` | `:8080` | HTTP 监听地址 |
| 服务 | `AIOPS_LOGLEVEL` | `info` | `debug\|info\|warn\|error` |
| HTTP | `AIOPS_HTTPREADTIMEOUTSEC` | `15` | 读请求（含 body）超时秒；WS 长连接不受影响 |
| HTTP | `AIOPS_HTTPWRITETIMEOUTSEC` | `30` | 写响应超时秒（须 > `AIOPS_LLMTIMEOUTSEC`） |
| HTTP | `AIOPS_HTTPIDLETIMEOUTSEC` | `120` | keep-alive 空闲超时秒 |
| 存储 | `AIOPS_DBDRIVER` | `postgres` | 唯一支持（docker-compose 提供） |
| 存储 | `AIOPS_DBDSN` | — | Postgres DSN（如 `host=127.0.0.1 user=aiops ... TimeZone=Asia/Shanghai`） |
| 存储 | `AIOPS_DBMAXOPENCONNS` | `20` | 连接池最大打开数 |
| 存储 | `AIOPS_DBMAXIDLECONNS` | `20` | 最大空闲数（= MaxOpen 防连接抖动） |
| 存储 | `AIOPS_DBCONNMAXLIFETIMESEC` | `1800` | 连接最长存活秒（0 不限） |
| 存储 | `AIOPS_DBCONNMAXIDLETIMESEC` | `300` | 空闲回收秒（0 不限） |
| 存储 | `AIOPS_DBCONNECTRETRIES` | `5` | 启动连接重试（PG 未就绪时等待） |
| 保留 | `AIOPS_RETENTIONENABLED` | `true` | 保留策略 worker 开关 |
| 保留 | `AIOPS_RETENTIONALERTSDAYS` | `30` | alerts 明细保留天数（事故/当前状态不受影响） |
| 保留 | `AIOPS_RETENTIONAUDITDAYS` | `180` | audit_logs 保留天数 |
| 保留 | `AIOPS_RETENTIONWORKFLOWRUNSDAYS` | `90` | workflow_runs 保留天数 |
| 保留 | `AIOPS_RETENTIONINTERVALMIN` | `60` | 清理执行间隔分钟 |
| 分区 | `AIOPS_ALERTSPARTITIONS` | `true` | alerts 月分区（false 退回纯行级 DELETE，不提供反向迁移） |
| 队列/总线/认证 | `AIOPS_QUEUEDRIVER` / `AIOPS_EVENTBUSDRIVER` / `AIOPS_AUTHTYPE` | `memory` / `memory` / `noauth` | `noauth` 固定 default 租户；`apikey` 校验 `X-API-Key` |
| 通知 | `AIOPS_NOTIFIERCHANNELS` | — | 逗号分隔启用渠道 `dingtalk,feishu,wecom,http`（env 仅首启种子，DB 为准） |
| 通知 | `AIOPS_NOTIFIER{DINGTALK,FEISHU,WECOM,HTTP}` | — | 各渠道 webhook 地址 |
| 通知 | `AIOPS_NOTIFIERBASEURL` | `http://localhost:5173` | 前端地址（拼接事故详情链接） |
| 通知节流 | `AIOPS_NOTIFYENABLED` | `true` | 告警级通知（同 fingerprint 首条立即 → 冷却合并 → 升级/恢复突破 → 周期汇总）；无渠道 fail-open |
| 通知节流 | `AIOPS_NOTIFYCOOLDOWNSEC` | `300` | 冷却秒（0=每事件都发，关闭节流） |
| 通知节流 | `AIOPS_NOTIFYSUMMARYMIN` | `30` | 汇总周期分钟（0=关闭汇总；冷却期内重复按"过去 30 分钟重复 47 次"合并通知） |
| Agent | `AIOPS_AGENTDRYRUN` | `true` | 自愈动作默认 dry-run（安全优先） |
| Agent | `AIOPS_AGENTENABLED` | `true` | 是否投递 agent.run 任务 |
| Agent | `AIOPS_RCACONFIDENCE` | `0.3` | RCA 置信度门槛 |
| Agent | `AIOPS_AGENTMAXDEPTH` | `5` | 依赖链遍历深度 |
| Agent | `AIOPS_AGENTBLASTRADIUSLIMIT` | `0.2` | 爆炸半径超限强制降级 L2 |
| Agent | `AIOPS_AGENTAUTOHEALLEVEL` | `L0` | 允许自动执行的最高级别（L0/L1/L2） |
| LLM | `AIOPS_LLMAPIKEY` | — | 配置后启用（openai-compat）；未配置纯确定性 |
| LLM | `AIOPS_LLMBASEURL` / `AIOPS_LLMMODEL` | `https://api.deepseek.com` / `deepseek-chat` | OpenAI 兼容端点 / 模型 |
| LLM | `AIOPS_LLMTIMEOUTSEC` | `15` | 整体超时秒（流式累计期间生效） |
| LLM | `AIOPS_LLMSTREAM` | `true` | SSE 流式响应 |
| 聚类 | `AIOPS_AICLUSTER{ENABLED,THRESHOLD,WINDOWMIN,INTERVALSEC}` | `true`/`3`/`10`/`60` | 定时 worker / 达阈值确认 / 时间窗分钟 / 间隔秒 |
| 工作流 | `AIOPS_WORKFLOWENABLED` | `true` | pipeline 投递 workflow.trigger 并消费 |
| 工作流 | `AIOPS_WORKFLOWEXECWORKERS` | `8` | 工作流执行并发上限 |
| 工作流 | `AIOPS_WORKFLOWSCHEDINTERVALSEC` | `10` | interval 触发器扫描间隔秒 |
| RAG | `AIOPS_RAGENABLED` | `false` | 知识检索层总开关（默认关闭，现有行为不变） |
| RAG | `AIOPS_RAGMODE` | `off` | `off\|readonly`（本期只读；shadow/assist 配置即报错） |
| RAG | `AIOPS_RAGRETRIEVALTIMEOUTMS` | `800` | 检索总超时毫秒（超时立即降级，不阻塞） |
| RAG | `AIOPS_RAGTOPK{KEYWORD,VECTOR,FINAL}` | `40`/`40`/`6` | 关键词/向量召回条数 / 融合后最终证据条数 |
| RAG | `AIOPS_RAGMAXCONTEXTTOKENS` | `5000` | RAG 上下文 token 预算（单文档 40% 上限） |
| RAG | `AIOPS_RAGINDEXWORKERS` | `2` | 索引 worker 并发数（SKIP LOCKED 领取） |
| RAG | `AIOPS_RAGINDEXVERSION` | `v1` | 在线索引版本（换模型/维度必须递增并 `POST /rag/reindex` 重建） |
| RAG | `AIOPS_RAGINDEXINTERVALSEC` | `5` | 索引 worker 轮询间隔秒 |
| RAG | `AIOPS_RAGINDEXHNSW` | `false` | 向量 HNSW 索引（默认精确扫描基线；数据量大后启用） |
| RAG | `AIOPS_RAGBACKFILLONBOOT` | `true` | 启动回填已解决事故与已启用剧本（幂等） |
| RAG | `AIOPS_RAGADMINAPIS` | `false` | 管理端点（rag/status、reindex、rag-trace）开关；apikey 模式还需租户在 `AIOPS_RAGADMINTENANTS` 白名单 |
| RAG | `AIOPS_RAGADMINTENANTS` | — | 允许管理端点的租户（逗号分隔，apikey 模式） |
| 模型管理 | `AIOPS_MODELCFGADMINAPIS` | `false` | 模型配置管理端点（携带密钥，独立管理开关；默认 403） |
| 模型管理 | `AIOPS_MODELCFGADMINTENANTS` | — | 允许管理模型配置的租户（逗号分隔，apikey 模式） |
| Embedding | `AIOPS_EMBEDDINGDRIVER` | `off` | `off`（关键词-only）\| `openai-compat` \| `local`（确定性 mock，仅演示/测试） |
| Embedding | `AIOPS_EMBEDDINGBASEURL` / `AIOPS_EMBEDDINGMODEL` | — | OpenAI 兼容 `/embeddings` 端点 / 模型（与 Chat LLM 分开配置；**仅 env 配置，无 UI 入口**） |
| Embedding | `AIOPS_EMBEDDINGDIMENSION` | `0` | 向量维度（索引契约；与在线列不一致时报错/禁用向量路） |
| Embedding | `AIOPS_EMBEDDINGTIMEOUTSEC` | `10` | 单次 embedding 请求超时秒 |
| Embedding | `AIOPS_EMBEDDINGBATCHSIZE` | `32` | 批量 embedding 分片条数（千问 text-embedding 建议 16） |

千问 Embedding 接入示例（仅 env，无 UI 入口）：

```bash
export AIOPS_EMBEDDINGDRIVER=openai-compat
export AIOPS_EMBEDDINGBASEURL=https://dashscope.aliyuncs.com/compatible-mode/v1
export AIOPS_EMBEDDINGAPIKEY=sk-xxx
export AIOPS_EMBEDDINGMODEL=text-embedding-v3
export AIOPS_EMBEDDINGDIMENSION=1024
export AIOPS_EMBEDDINGBATCHSIZE=16
```
| AI 助手 | `AIOPS_ASSISTANTENABLED` | `false` | 对话式 Agent 总开关（默认关闭；功能关闭时 `/assistant` 端点全部 404，现有路径零变化） |
| AI 助手 | `AIOPS_ASSISTANTREADONLY` | `true` | 只读模式（动作工具 Phase 3 开放，字段先立） |
| AI 助手 | `AIOPS_ASSISTANTMAXCONCURRENCY` | `4` | 并发 run worker 数（独立 DB 租约 + 信号量，不挤占告警热路径） |
| AI 助手 | `AIOPS_ASSISTANTMAXMODELROUNDS` / `AIOPS_ASSISTANTMAXTOOLCALLS` | `8`/`12` | 单 run 模型回合 / 工具调用预算（超限安全终止） |
| AI 助手 | `AIOPS_ASSISTANTRUNTIMEOUTSEC` / `AIOPS_ASSISTANTTOOLTIMEOUTSEC` | `60`/`3` | 单 run 总超时 / 单工具执行超时 |
| AI 助手 | `AIOPS_ASSISTANTRETENTIONDAYS` / `AIOPS_ASSISTANTPROMPTVERSION` | `90`/`v1` | 会话保留天数（Phase 4 启用清理）/ 系统提示词版本 |
| AI 助手 | `AIOPS_ASSISTANTCHAT{DRIVER,BASEURL,APIKEY,MODEL,TIMEOUTSEC,STREAM}` | — | 对话生成模型端点（OpenAI 兼容，需支持 tool calling；**模型配置页 Assistant Chat 行可管理，env 仅首启种子**） |

对话式 Agent 快速启用（AI 助手页）：

```bash
# 1) 本地 mock（无 key 联调）：node deploy/mock_llm.mjs 9999
# 2) 后端指向 mock（或模型配置页填 Assistant Chat）
export AIOPS_ASSISTANTENABLED=true
export AIOPS_ASSISTANTCHATBASEURL=http://localhost:9998   # 或千问/DeepSeek 兼容端点
export AIOPS_ASSISTANTCHATAPIKEY=dummy
export AIOPS_ASSISTANTCHATMODEL=mock
```
| 日志 | `AIOPS_LOGFILE` | — | 日志文件路径（设置后 stdout+文件双写，Docker 不配则纯 stdout）；日志全局脱敏（apiKey/token/URL 查询参数 → `[REDACTED]`） |
| 日志 | `AIOPS_LOGMAXMB` / `AIOPS_LOGMAXBACKUPS` / `AIOPS_LOGMAXAGEDAYS` | `100`/`10`/`30` | 单文件轮转 MB / 保留轮转文件数 / 保留天数（≈1GB 总上限） |
| 日志 | `AIOPS_LOGREQUESTS` | `true` | HTTP 访问日志（含 requestID；path 经脱敏） |
| 备份 | `AIOPS_BACKUPENABLED` | `false` | 数据备份 worker（定时 pg_dump -Fc；恢复见 `scripts/restore.sh` 与 `docs/RESTORE.md`） |
| 备份 | `AIOPS_BACKUPINTERVALHOURS` | `24` | 执行间隔小时（距上次成功超间隔才补跑；启动补跑开关 `AIOPS_BACKUPONBOOT`） |
| 备份 | `AIOPS_BACKUPDIR` | `./backups` | 备份目录（0700 创建、文件 0600） |
| 备份 | `AIOPS_BACKUPKEEP` / `AIOPS_BACKUPMAXGB` | `10`/`5` | 保留份数 / 总容量上限（双限任一达限删最旧） |
| 备份 | `AIOPS_BACKUPMIRRORDIR` | — | 异地镜像目录（另一磁盘/NAS 挂载点；空=不镜像） |
| 备份 | `AIOPS_BACKUPMIRRORENCRYPT` | `false` | 镜像 AES-256-GCM 加密（需 `AIOPS_BACKUPPASSPHRASEFILE` 口令文件，权限 0600） |
| 备份 | `AIOPS_BACKUPPASSPHRASEFILE` | — | 镜像加密口令文件路径（加密必填；丢失则镜像不可恢复） |
| 备份 | `AIOPS_BACKUPPGCONTAINER` | `aiops-postgres` | pg 工具执行方式：docker exec 容器名 / `host`（本机 pg 客户端） |
| 备份 | `AIOPS_BACKUPVERIFY` | `true` | 备份后恢复到临时库校验关键表计数（通过后自动清理临时库） |
| 备份 | `AIOPS_BACKUPTIMEOUTMIN` | `60` | 单次备份超时分钟 |

## 常用命令（Makefile）

```bash
make infra     # docker compose 启动 PostgreSQL
make app       # go run ./cmd/aiops（:8080，内嵌默认 DSN）
make build     # go build -o bin/aiops ./cmd/aiops
make test / vet / tidy
make web       # cd web && npm run dev（:5173）
make web-build # 前端构建
make clean     # rm -rf bin
```

压测回归基准（依赖 :8080 运行中，详见 docs/PERF.md）：

```bash
make bench                # 跑一次压测留痕 .bench/latest.json（不判定）
make regress-baseline     # 记录当前环境基线 .bench/baseline.json（换环境/改参数后重记）
make regress              # 压测 + 与基线对比，吞吐/P50/P99 退化非零退出（CI 看门狗）
make regress-baseline-batch   # 记录批量路径基线（-batch 20）
make regress-batch            # 批量路径回归判定
# 可覆盖：LOADGEN_BASEURL / BENCH_FLAGS（如 "-rate 1000 -duration 10s"）/ BENCH_BATCH
```

详见 `docs/`：进度 [docs/PROGRESS.md](docs/PROGRESS.md)、压测基准 [docs/PERF.md](docs/PERF.md)、架构与功能使用指南 [docs/GUIDE.md](docs/GUIDE.md)、**运维概念与黑话入门 [docs/GLOSSARY.md](docs/GLOSSARY.md)**（面向运维新手，术语逐个大白话解释）。
