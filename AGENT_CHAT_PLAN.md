# AIOps 对话式 Agent 架构与实施计划

> 状态：设计草案，尚未实施  
> 日期：2026-08-10  
> 目标：在不破坏现有告警热路径和 `monitor -> rca -> heal -> change` 安全链的前提下，增加一个面向值班人员的、可追踪、可引用、可审批的聊天 Agent。

## 1. 结论先行

当前项目已经具备告警、事故、拓扑、Playbook、工作流、RAG、审计和固定 Agent 流水线，但还不具备“对话 Agent”的三个核心能力：

1. 多轮会话、消息、运行步骤和中断恢复；
2. 模型驱动的受控工具调用；
3. 面向人的流式回复、证据引用和动作确认交互。

因此，不应把现有 `internal/agent.Agent` 直接包装成聊天接口。建议新增独立的 `internal/assistant` 会话编排域，将现有领域 Service 暴露成白名单工具。两个 Agent 路径共享领域能力和安全规则，但职责保持分离：

- 现有 Agent：由告警触发，执行固定、确定性的状态机；
- 对话 Agent：由用户触发，负责理解问题、检索证据、调用只读工具、生成处置草案；
- 所有有副作用的动作：必须由服务端鉴权、参数校验和人工确认后，再调用现有领域 Service；模型不能直接写库或执行任意命令。

首个可用版本建议只做“只读运维副驾驶”：查询告警和事故、分析当前事故、查看拓扑影响、检索相似历史事故和 Playbook，并给出带引用的建议。稳定后再开放审批式动作。

## 2. 现状审计与可复用能力

### 2.1 已有能力

| 领域 | 当前实现 | 对聊天 Agent 的价值 |
|---|---|---|
| 告警/事故 | `internal/alert`、`internal/incident` | 查询事实、建立事故上下文 |
| 固定 Agent | `internal/agent`、`internal/orchestrator` | 复用 RCA 结果、安全分级、审批语义，不直接复用为聊天循环 |
| 拓扑 | `internal/topology` | 查询依赖链和爆炸半径 |
| RAG | `internal/rag`、`internal/embedding` | 检索历史事故、Playbook 和个人知识源，输出结构化 EvidencePack |
| LLM | `internal/ai/llm.go` | 已支持 OpenAI 兼容 Chat Completions 和 SSE，但当前只支持 system/user 文本，不支持通用 messages/tool calls |
| 模型配置 | `internal/modelcfg`、模型配置页 | Chat 配置已 DB 化；Embedding 当前仍由环境变量装配 |
| 实时推送 | `internal/pusher`、`web/src/realtime.ts` | 继续用于全局告警/事故广播；聊天流另用按 run 隔离的 SSE |
| 审计 | `internal/eventbus.AuditService` | 记录工具调用、动作确认、拒绝和失败 |
| 鉴权 | API key + tenant | 已有租户隔离，但缺少可靠的用户身份和细粒度权限 |
| 前端 | React 19 + Vite + Ant Design + React Query | 增加 `/assistant` 页面和事故上下文入口 |

### 2.2 必须补齐的缺口

- `ai.Client` 会把流式响应聚合为最终文本，没有通用消息数组、结构化工具定义、工具调用增量和 usage 元数据；
- 当前内存队列承担告警 Agent/工作流，聊天任务不能与告警热路径争抢同一 worker 池；
- RAG 当前主要以事故构造结构化查询，缺少经过脱敏的通用知识搜索入口；
- 当前 API key 验证只得到 tenant，`actor` 仍可能来自请求体，不能作为高风险动作的可信审批身份；
- 当前模型密钥以明文保存在 `model_configs` 表中，计划实施时应同时解决静态密钥保护；
- 没有会话、消息、run、step、action request、聊天事件等持久化模型；
- 没有提示词注入防护、工具调用次数预算、上下文压缩和会话保留策略。

## 3. 设计参考与取舍

本方案借鉴成熟 Agent 项目的通用模式，但不直接引入大型框架：

- 借鉴 LangGraph 的“显式状态图、checkpoint、interrupt/resume”：运行状态持久化，遇到动作确认时中断，而不是在一个 HTTP 请求内一直等待；
- 借鉴 OpenHands 的“action/observation 事件流”：模型意图、工具参数、工具结果和最终回复分别记录，便于回放和审计；
- 借鉴 Dify 类产品的“conversation/message/run 分层”：会话历史不等于一次模型运行，一条用户消息可对应一次可取消、可失败、可重试的 run；
- 保留本项目已有的确定性护栏：RAG 只提供证据，模型建议不降低 L0/L1/L2、爆炸半径、dry-run 或审批等级。

不建议首期引入 LangChain/LangGraph Go 依赖。现有领域边界清楚，自建一个小型显式循环更容易测试、审计和限制行为，也能避免框架侵入告警热路径。

## 4. 目标能力与非目标

### 4.1 首期目标

用户可以在聊天窗口完成：

1. 询问平台状态、近期告警、未解决事故；
2. 从事故详情一键“问 AI”，自动绑定 incident 上下文；
3. 让 Agent 汇总已有 Monitor/RCA/Heal/Change 节点结果；
4. 查询依赖拓扑、影响范围、相似已解决事故、相关 Playbook/知识源；
5. 获得带来源链接、可信等级和降级说明的回答；
6. 查看 Agent 调用了哪些工具、耗时和结果摘要；
7. 停止生成、重试失败 run、继续已有会话。

### 4.2 后续目标

- 生成处置草案；
- 对 `ack alert`、`resolve incident`、`approve/reject existing action`、`run workflow` 等有限动作发起确认卡片；
- 由有权限的用户明确确认后执行，并完整审计；
- 基于离线问题集和在线反馈持续评测回答质量与工具选择。

### 4.3 明确非目标

- 不允许模型生成任意 shell、SQL 或任意 URL 并直接执行；
- 不让聊天路径替代现有告警去重、规则引擎或 Agent 状态机；
- 不允许模型直接修改规则、Playbook、工作流、模型配置或密钥；
- 不首期实现多 Agent 自由协作、长期人格记忆、联网搜索或跨租户知识共享；
- 不用 Embedding 模型生成回答或决定工具。Embedding 只参与语义检索。

## 5. 推荐架构

```text
React Chat UI
  │  REST: thread/message/action
  │  SSE: run events
  ▼
Assistant API ──鉴权/租户/幂等──► Assistant Service
                                      │
                                      ├─ Run Store + DB Lease Worker
                                      ├─ Context Builder / Token Budget
                                      ├─ Agent Loop（最大步数）
                                      ├─ Tool Registry（JSON Schema 白名单）
                                      │    ├─ alerts/incidents/overview
                                      │    ├─ topology
                                      │    ├─ RAG/knowledge/playbooks
                                      │    └─ guarded actions
                                      ├─ Chat Model（OpenAI-compatible tool calls）
                                      └─ Audit / Metrics / Event Stream

现有告警热路径 ──► queue(agent.run) ──► 原 Orchestrator
                    与聊天 run worker 隔离
```

### 5.1 包边界

建议新增：

```text
internal/assistant/
  model.go          # Thread/Message/Run/Step/ActionRequest/Event
  repository.go     # tenant-scoped 持久化、幂等和租约
  service.go        # 会话 API 用例
  runner.go         # Agent loop、步数/时间/上下文预算
  context.go        # 事故上下文、历史消息裁剪和摘要
  prompt.go         # 版本化系统提示词
  stream.go         # run 级 SSE broker 与断线快照恢复
  tools.go          # Tool 接口、注册表、权限/风险元数据
  tools_read.go     # 只读领域工具
  tools_action.go   # 需确认的动作工具（后续阶段）
  policy.go         # 权限、参数、提示注入和风险策略
  metrics.go

internal/llm/
  types.go          # Message/Tool/ToolCall/Delta/Usage
  openai_compat.go  # 通用 Chat Completions + SSE + tool_calls
  errors.go
```

`internal/ai/llm.go` 的 RCA/聚类接口不能一次性删除。推荐让它保留兼容 facade，底层逐步改用 `internal/llm`；先让新聊天功能使用通用客户端，回归稳定后再收敛重复 HTTP/SSE 代码。

### 5.2 Agent 循环

每个 run 的服务端状态机：

```text
queued -> running -> model_call
                      │
                      ├─ final answer -> completed
                      │
                      └─ tool calls -> validate/authorize
                                         │
                                         ├─ read tool -> execute -> observation -> model_call
                                         ├─ action tool -> waiting_confirmation
                                         └─ invalid/over-budget -> safe failure/final answer

waiting_confirmation -> confirmed -> execute deterministic domain service -> model_call/completed
                     └-> rejected  -> observation -> model_call/completed
```

硬限制建议：

- 单次 run 最多 8 个模型回合、12 次工具调用；
- 同一批互不依赖的只读工具最多并行 4 个；
- 单工具默认 3 秒，RAG 使用现有 800ms 总预算，单 run 默认 60 秒；
- 限制单次工具结果大小，返回结构化摘要而非整表；
- 超限、超时、模型失败时输出明确的降级回答，并保留已取得的确定性事实；
- 每个 tool call 使用稳定 ID，重试时按 ID 幂等；有副作用工具必须另外使用 action request ID。

### 5.3 工具分层

首期只读工具：

| 工具 | 领域来源 | 说明 |
|---|---|---|
| `get_platform_overview` | `overview.Service` | 平台健康和趋势摘要 |
| `list_alerts` / `get_alert` | `alert.Repository` | 严格限制过滤字段、时间窗和条数 |
| `list_incidents` / `get_incident` | `incident.Service` | 含已有 nodeResults、结案数据和审计摘要 |
| `get_topology_impact` | `topology.Service` | 返回结构化依赖路径和影响分数 |
| `get_similar_incidents` | `rag.Service` | 复用 EvidencePack，并产生 trace/citation |
| `search_knowledge` | `rag.Service` 新通用入口 | 用户查询先脱敏，再做关键词/向量混合检索 |
| `list_playbooks` | `playbook.Service` | 只返回元数据、适用条件和安全级别 |
| `get_workflow_run` | `workflow.Service` | 解释运行结果，不触发工作流 |

后续受控动作工具：

| 工具 | 风险 | 约束 |
|---|---|---|
| `ack_alert` | 低 | 明确 alert ID，一次确认 |
| `resolve_incident` | 中 | 展示事故和结案影响，确认后走现有 Service |
| `approve_incident_action` / `reject_incident_action` | 高 | 仅 owner/approver；复用 Orchestrator 状态校验和审批审计 |
| `run_workflow` | 高 | 仅白名单 workflow，显示输入、步骤和外部影响；确认后执行 |

工具实现直接调用 Go Service，不通过本机 HTTP 回调自身；每个工具都必须声明输入 JSON Schema、所需权限、是否有副作用、超时和结果裁剪器。

## 6. 数据模型

建议增加以下 Postgres 表，全部带 `tenant_id` 并在 Repository 层强制过滤：

### 6.1 `assistant_threads`

- `id`、`tenant_id`、`title`、`status`；
- `context_type`、`context_id`：可绑定 incident/alert；
- `created_by`、`created_at`、`updated_at`；
- 索引：`(tenant_id, updated_at desc)`。

### 6.2 `assistant_messages`

- `id`、`tenant_id`、`thread_id`、`run_id`；
- `role`：user/assistant/system/tool；
- `content`、`content_format`、`sequence`、`status`；
- `citations jsonb`、`metadata jsonb`、`created_at`；
- 唯一索引：`(tenant_id, thread_id, sequence)`。

最终 assistant 消息才作为后续上下文；内部推理不保存、不返回。工具事件和可解释的结果摘要保存在 step/event 中。

### 6.3 `assistant_runs`

- `id`、`tenant_id`、`thread_id`、`input_message_id`；
- `status`、`model`、`prompt_version`、`idempotency_key`；
- `step_count`、token/延迟/错误字段；
- `locked_at`、`locked_by`、`next_run_at`：使用与 RAG job 相似的 `FOR UPDATE SKIP LOCKED` 租约；
- `created_at`、`started_at`、`finished_at`。

### 6.4 `assistant_steps`

- `id`、`run_id`、`ordinal`、`kind`：model/tool_call/tool_result/policy；
- `tool_name`、`arguments jsonb`、`result_summary jsonb`；
- `status`、`duration_ms`、`error_code`、`created_at`；
- 敏感字段写入前脱敏，禁止保存 Authorization、模型 key 和完整秘密值。

### 6.5 `assistant_action_requests`

- `id`、`run_id`、`tool_name`、规范化后的 `arguments jsonb`；
- `risk_level`、`status`：pending/confirmed/rejected/executing/succeeded/failed/expired；
- `requested_by`、`decided_by`、`decision_reason`、时间戳；
- `idempotency_key` 唯一，防止重复点击或断线重放造成二次执行。

### 6.6 `assistant_events`

仅持久化 run/step/action 状态事件和合并后的文本块，不按单 token 写一行。SSE 在线增量可在内存 broker 中以 50～100ms 合并推送；断线重连先返回 DB 快照，再继续实时订阅。

建议为消息、step、event 配置独立保留期，默认 30～90 天；审计日志按现有更长期策略保留。

## 7. API 与流式协议

建议 API：

```text
POST   /api/v1/assistant/threads
GET    /api/v1/assistant/threads
GET    /api/v1/assistant/threads/:id
DELETE /api/v1/assistant/threads/:id              # 软删除
POST   /api/v1/assistant/threads/:id/messages      # 持久化 user message，返回 202 + runId
GET    /api/v1/assistant/runs/:id
GET    /api/v1/assistant/runs/:id/events           # SSE
POST   /api/v1/assistant/runs/:id/cancel
POST   /api/v1/assistant/actions/:id/confirm
POST   /api/v1/assistant/actions/:id/reject
POST   /api/v1/assistant/messages/:id/feedback
```

发送消息接口要求 `Idempotency-Key`；创建 run 后立即返回，避免占用一个长时间 POST。SSE 事件至少包含：

- `run.started`、`run.completed`、`run.failed`、`run.cancelled`；
- `message.delta`、`message.completed`；
- `tool.started`、`tool.completed`、`tool.failed`；
- `action.required`、`action.resolved`；
- `citation.added`、`usage.updated`、`heartbeat`。

每个事件带 `runId`、单调递增 `sequence` 和时间戳。前端按 sequence 去重，断线后以最后 sequence 恢复。聊天流不复用全局 `/ws`，因为 SSE 更适合单向 token/event 流，也更容易按 run 做取消、重连和代理层配置；全局 WebSocket 继续服务告警和事故广播。

## 8. 上下文、RAG 与引用

### 8.1 上下文构建

上下文按以下优先级组装，而不是把全库数据或整个事故 JSON 直接塞给模型：

1. 版本化 system policy；
2. 当前绑定对象的确定性事实摘要；
3. 最近若干轮用户/assistant 消息；
4. 工具结果摘要；
5. RAG 证据片段；
6. 较早历史的压缩摘要。

保留独立预算：系统策略、会话历史、工具结果、RAG 证据、回答输出。裁剪时优先保留当前问题、确定性事实和高可信引用。

### 8.2 通用知识搜索

在 `rag.Service` 增加显式的 `SearchKnowledge(ctx, tenantID, query, filters)`，复用现有 sanitizer、三路召回、RRF、可信等级和 token budget。需要特别处理：

- 疑似 key、token、Cookie、私有地址等命中现有敏感规则时，不发送到外部 Embedding；
- RAG 关闭或向量路失败时，降级到关键词检索；
- 每次检索生成 trace，回答引用保留 document/chunk/source/trust/time；
- 知识内容一律视为不可信数据，不能覆盖 system policy，也不能自行触发工具；
- 回答中的事实性结论尽量绑定结构化 citation，UI 可跳转到事故或知识库详情。

## 9. 鉴权、安全和审批

### 9.1 身份与权限

现有 API key 只解析 tenant，不足以支撑可信审批。实施动作阶段前应扩展为：

- 中间件注入 `Identity{tenantID, subjectID, keyID, role/scopes}`；
- 至少支持 `assistant:read`、`assistant:use`、`assistant:act`、`incident:approve`；
- owner key 拥有全部权限，普通 key 默认只读；
- `noauth` 仅用于本地开发，动作审计 actor 明确标为 `local-noauth`；
- actor 由中间件身份生成，不能再相信模型或请求体中的任意字符串。

### 9.2 动作隔离

模型只能“提出 action request”，不能直接执行。确认接口必须重新执行以下检查，不能复用模型调用时的旧判断：

1. tenant 和身份权限；
2. 目标对象是否仍存在；
3. 当前状态是否仍允许动作；
4. 参数 schema 和业务约束；
5. 风险级别、dry-run、爆炸半径和审批等级；
6. action request 是否未过期、未执行且 idempotency key 未使用。

### 9.3 密钥与出域

- 用户提供的 key 不写入源码、计划、日志、聊天消息、审计或前端 bundle；
- 当前已在聊天消息中暴露的 key 应在服务商侧轮换，实施时只使用轮换后的 key；
- Embedding key 继续只在服务端配置，至少支持环境变量/容器 secret；
- Chat 和 Embedding 密钥逻辑分离，即使暂时使用同一供应商凭据也不能共用配置字段；
- 建议把 `model_configs.api_key` 迁移为密文或 secret reference，由独立 master key/secret file 解密；API 永远只返回 `apiKeySet`；
- 对模型和 Embedding Base URL 做允许的 scheme/host 校验，防止 SSRF 和保存密钥被换域外带；
- 复用并扩展 `internal/logutil` 脱敏，覆盖请求参数、工具结果和模型错误体。

### 9.4 Prompt Injection 防护

- system policy 明确区分“平台指令”和“外部数据”；
- 工具仅由服务端注册表提供，模型返回未知工具一律拒绝；
- 所有参数二次校验，不把模型生成的 SQL/URL/命令交给执行器；
- RAG 文档、告警 annotation、工具 observation 标记为 untrusted content；
- 最终答案可引用数据，但数据中的“忽略之前指令”等文本不具有指令权；
- 建立恶意知识文档和恶意告警文本的固定回归样例。

## 10. 模型与配置要求

### 10.1 Chat LLM

聊天 Agent 必须有生成模型。要求：

- OpenAI-compatible Chat Completions；
- 支持多轮 `messages`、流式 SSE；
- 最好原生支持 tool/function calling；
- 中文运维问答、结构化参数遵循和 JSON Schema 表现稳定；
- 建议上下文至少 32K，并能设置超时和输出 token 上限。

如果继续采用当前默认地址，可配置 DeepSeek `deepseek-chat`；这需要 DeepSeek API key。也可以使用通义聊天模型：如果同一个阿里云工作空间 key 同时拥有聊天权限，可使用同一秘密值，但 Chat 与 Embedding 仍建立两个独立配置项。

若模型不支持可靠 tool calling，不建议用“从自由文本中正则提取动作”兜底；首期可退化为纯问答/RAG，而不是降低工具安全性。

### 10.2 Embedding

仅有 key 还不足以装配，实施前需要确认：

- 服务商和 OpenAI 兼容 Base URL；
- Embedding 模型名；
- 输出维度；
- 是否支持指定 dimension；
- 单批上限和限流。

模型/维度一旦用于在线索引就是索引契约。更换时必须增加 `AIOPS_RAGINDEXVERSION` 并重建，不能把不同模型向量混在同一版本。

### 10.3 建议新增配置

```text
AIOPS_ASSISTANTENABLED=false
AIOPS_ASSISTANTREADONLY=true
AIOPS_ASSISTANTMAXCONCURRENCY=4
AIOPS_ASSISTANTMAXMODELROUNDS=8
AIOPS_ASSISTANTMAXTOOLCALLS=12
AIOPS_ASSISTANTRUNTIMEOUTSEC=60
AIOPS_ASSISTANTTOOLTIMEOUTSEC=3
AIOPS_ASSISTANTRETENTIONDAYS=90
AIOPS_ASSISTANTPROMPTVERSION=v1
```

模型配置建议增加独立的 `assistant_chat` key，而不是强制与 RCA/聚类共享 `chat`。这样可分别选择模型、上下文、超时、流式和工具能力，并能独立关闭聊天而不影响现有 RCA。

## 11. 前端聊天窗口

### 11.1 信息架构

新增导航“AI 助手”和 `/assistant` 页面；IncidentDetail 增加“在 AI 助手中分析”入口并带上 incident ID。桌面布局：

```text
┌────────会话列表────────┬────────────────对话区────────────────┬────上下文/证据────┐
│ 新建会话               │ 用户问题                              │ 当前 Incident     │
│ 最近会话               │ Agent 文本 + 引用                     │ Agent 步骤         │
│ 事故标题/更新时间      │ 工具调用折叠卡 / 动作确认卡           │ 引用与降级状态     │
│                        │ 输入框 / 停止 / 重试                  │                  │
└───────────────────────┴──────────────────────────────────────┴──────────────────┘
```

窄屏折叠左右栏，保留对话和输入主流程。

### 11.2 组件建议

```text
web/src/pages/Assistant.tsx
web/src/components/assistant/
  ThreadList.tsx
  ChatTimeline.tsx
  MessageBubble.tsx
  Composer.tsx
  RunStatus.tsx
  ToolStepCard.tsx
  ActionApprovalCard.tsx
  CitationList.tsx
  ContextPanel.tsx
web/src/assistant/api.ts
web/src/assistant/types.ts
web/src/assistant/useRunStream.ts
web/src/assistant/streamReducer.ts
```

### 11.3 React 实现约束

- React Query 管服务端 thread/message/run 快照；SSE 增量放在局部 reducer，不把每个 token 写入全局 query cache；
- 将 50～100ms 内的 delta 合并后更新，避免每个 token 引发整页重渲染；
- `MessageBubble`、工具卡、证据侧栏拆分订阅边界，长会话使用 `content-visibility`，数据量再大时引入虚拟列表；
- Markdown/代码高亮等重组件按需加载；禁止渲染原始 HTML，链接协议白名单化；
- 流式临时状态使用 ref/reducer，完成后一次性并入稳定消息；
- 输入发送、停止、确认动作在事件处理器中完成，不通过 effect 推导交互；
- 事故上下文和会话列表的独立请求并行发起，避免瀑布；
- 支持错误重试、断线恢复、键盘发送、代码复制、引用跳转和可访问性状态播报。

## 12. 可观测性与资源隔离

聊天路径不能影响告警吞吐：

- 使用独立 DB lease worker 和独立并发信号量，不复用当前 `queue.NewMemoryQueue(4)`；
- 模型、Embedding、每类工具分别设置超时和熔断；
- run 取消时传播 context cancellation；
- 限制每租户同时运行数和每分钟请求数；
- 数据库查询工具强制 limit、时间窗和只读语义；
- HTTP server 的聊天 SSE 不受普通 30 秒 write timeout 误杀，需要路由级/服务级验证，但不能放宽全局告警接口超时。

建议指标：

```text
aiops_assistant_runs_total{status,model}
aiops_assistant_run_duration_seconds
aiops_assistant_model_duration_seconds{outcome}
aiops_assistant_tool_calls_total{tool,outcome}
aiops_assistant_tool_duration_seconds{tool}
aiops_assistant_active_runs
aiops_assistant_tokens_total{direction,model}
aiops_assistant_action_requests_total{tool,decision,outcome}
aiops_assistant_citations_total{source_type}
aiops_assistant_feedback_total{rating}
```

日志使用 run ID、thread ID、tool call ID 和 correlation ID 关联，但不记录完整 prompt、秘密、敏感 annotation 或未经裁剪的工具结果。

## 13. 分阶段实施

### Phase 0：契约与安全底座

范围：

- 定义 `internal/llm` 通用消息、流式和 tool call 契约；
- 为现有 `ai.Client` 保留兼容 facade；
- 建 assistant 数据模型、Repository、租户隔离和 DB lease run worker；
- 扩展可信 Identity/scopes；
- 增加模型配置 `assistant_chat`，完成 key 加密/secret reference 方案；
- 增加总开关，默认关闭。

验收：

- 无模型配置时后端正常启动，现有行为和测试不变；
- run 可排队、取消、超时、进程重启后回收过期租约；
- 租户交叉读写测试全部拒绝；
- 密钥不出现在 API、日志、审计和数据库明文字段中。

### Phase 1：只读聊天 MVP

范围：

- thread/message/run API 和 SSE；
- React 聊天页面、会话列表、停止/重试；
- 只读工具：overview、alerts、incidents、topology；
- 工具步骤可视化，暂无动作按钮；
- mock LLM 增加 tool_calls 和碎片化 SSE 场景。

验收：

- “列出最近 critical 告警并解释关联事故”能调用正确工具并给出事实一致的回答；
- 停止、网络重连、重复提交不会产生重复 run/message；
- 10 个并发聊天 run 不阻塞告警 webhook 和原 Agent worker；
- RAG/Embedding 全关时聊天仍能用只读领域工具回答。

### Phase 2：事故诊断与 RAG 引用

范围：

- IncidentDetail 上下文入口；
- `get_similar_incidents`、`search_knowledge`、`list_playbooks`；
- 引用卡、可信等级、RAG 降级信息和反馈；
- 历史消息裁剪与摘要。

验收：

- 回答中的历史事故和知识结论均可点回来源；
- 向量服务失败时自动关键词降级，回答明确提示降级，不影响事故主链；
- 恶意知识内容不能改变工具策略或触发动作；
- 离线问题集记录 citation precision、工具选择正确率、无依据回答率。

### Phase 3：人工确认动作

范围：

- action request/confirm/reject/expire 状态机；
- 首批动作：ack、resolve、现有 incident action approve/reject；
- 权限、幂等、状态重验和审计；
- 前端动作确认卡展示目标、影响、参数、风险和取消选项。

验收：

- 未确认、权限不足、过期、对象状态变化、重复确认均不能执行；
- 模型不能直接调用有副作用 Service；
- 每个成功/拒绝/失败动作均能从审计回溯到用户、run、tool call 和目标对象；
- 现有 L0/L1/L2、dry-run、爆炸半径和审批行为完全保留。

### Phase 4：生产加固与灰度

范围：

- 限流、配额、保留策略、成本指标、告警；
- prompt/tool schema 版本管理和回归评测；
- 灰度租户/只读开关/单工具 kill switch；
- 容量测试和故障注入。

上线顺序：开发环境 mock → 测试环境真实模型只读 → 单租户只读灰度 → RAG 灰度 → 最后单独开启动作确认。任何阶段可通过 `AIOPS_ASSISTANTENABLED=false` 关闭聊天，不影响现有告警与固定 Agent。

## 14. 测试计划

### 14.1 Go 单元与契约测试

- OpenAI-compatible 普通响应、SSE 分片、跨 chunk tool arguments、多个并行 tool calls、finish reason、429/401/5xx、超时和取消；
- Agent loop 的最大步数、工具预算、未知工具、schema 错误、重复 tool call、模型空响应；
- 每个工具的 tenant、limit、过滤和结果裁剪；
- action request 权限、过期、状态重验和幂等；
- context budget、历史裁剪、引用映射、敏感信息脱敏；
- DB lease 领取、崩溃回收和多 worker 防重。

### 14.2 API/集成测试

- thread/message/run CRUD、跨租户 404/403；
- SSE 顺序、heartbeat、断线快照恢复、cancel；
- mock LLM 发起多步工具调用后生成最终回答；
- RAG on/off/timeout/model mismatch 的降级；
- 审计与 metrics 一致性。

### 14.3 前端测试

- reducer 对乱序/重复/重连事件的确定性处理；
- 发送、停止、重试、切换会话、引用跳转、确认/拒绝动作；
- 长会话和高速 delta 下输入框保持响应；
- Markdown XSS、危险链接和恶意工具输出；
- `npm run build`，并补 Vitest/Testing Library；关键流程补 Playwright E2E。

### 14.4 性能与隔离测试

- 运行聊天并发测试的同时执行现有 `make regress` 和 `make regress-batch`；
- 门禁：告警吞吐与 P99 不超过现有回归阈值，聊天 worker 饱和只返回排队/限流，不挤占告警 worker；
- 模型不可达、Embedding 不可达、Postgres 慢查询、SSE 客户端断开等故障注入。

## 15. 预计改动面

主要新增/修改位置：

| 位置 | 变化 |
|---|---|
| `internal/assistant/*` | 新增会话 Agent 域 |
| `internal/llm/*`、`internal/ai/llm.go` | 通用 tool calling 流式协议 + 兼容 facade |
| `internal/rag/*` | 通用安全知识搜索和 citation/trace |
| `internal/auth/*`、`internal/api/middleware.go` | 可信 identity/scopes |
| `internal/modelcfg/*` | `assistant_chat` 与安全 secret 存储 |
| `internal/store/store.go` | 注册 assistant 模型；复杂约束继续显式 schema |
| `internal/api/assistant.go`、`server.go` | REST/SSE/动作端点 |
| `cmd/aiops/main.go` | assistant 服务、独立 worker、模型和指标装配 |
| `internal/metrics/*` | assistant 指标 |
| `web/src/pages/Assistant.tsx` 等 | 聊天 UI、流状态、引用和动作卡 |
| `deploy/mock_llm.mjs` | tool_calls/SSE/异常场景 mock |
| `README.md`、`docs/GUIDE.md`、`docs/PROGRESS.md` | 配置、使用和验收记录 |

这属于跨后端协议、持久化、鉴权和前端的架构级改动，应按 Phase 独立提交和验收，不做一次性大合并。

## 16. 实施前需要用户确认/提供的信息

1. 参考项目：仓库内没有找到独立参考项目或链接。如果有指定项目，请提供名称/路径/URL；否则按本文列出的成熟 Agent 模式实施。
2. Embedding：确认服务商、Base URL、模型名、维度和批量限制。不要再次在聊天里粘贴现有 key，先在服务商侧轮换。
3. Chat LLM：选择 DeepSeek 或通义等支持工具调用的模型，并提供对应服务端 secret。若当前阿里云工作空间凭据同时有聊天权限，可不另购 DeepSeek，但仍需确认聊天模型和 endpoint。
4. 首版范围：建议确认“只读 MVP”，暂不开放动作工具。
5. 部署形态：当前单体/单实例可先上线，但 run 已按 DB lease 设计，避免未来多实例时重写核心状态。

## 17. 推荐的第一批实施切片

在上述信息确认后，第一批只实施 Phase 0 + Phase 1，交付边界为：

- 一个可新建/恢复会话的聊天页面；
- 一个支持流式文本和只读工具调用的服务端 Agent loop；
- overview/alert/incident/topology 四类只读工具；
- 完整 tenant 隔离、run/step 记录、取消、超时、审计和 metrics；
- mock 模型下全套自动化测试；
- 默认功能关闭，不改变当前生产路径。

RAG 引用和任何写操作均放到后续独立阶段，避免一次同时引入“通用工具调用、外部 Embedding、动作审批”三类风险。
