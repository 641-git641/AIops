# AIOps RAG 建设与实施计划

> 状态：提案（Proposed）  
> 适用项目：AIOps 告警平台  
> 文档日期：2026-08-09  
> 核心原则：RAG 只增强证据，不替代确定性事实，不绕过自愈安全护栏。

---

## 1. 背景与结论

当前平台采用“双轨架构”：告警热路径负责归一化、维护窗口、去重、入库、规则关联和事件推送；Agent 智能路径负责 `monitor -> rca -> heal -> change`，并通过 dry-run、爆炸半径、熔断器和 L0/L1/L2 审批控制动作风险。

现有 RCA 已能查询服务拓扑和近期变更，形成确定性根因候选，再可选调用 LLM 补充自然语言分析。但 LLM 当前只看到本次告警及本次收集的证据，无法复用下列组织知识：

- 已解决事故的确认根因、处置过程和验证结果；
- Runbook、SOP、复盘文档中的诊断经验；
- 历史事故与当前事故之间的相似模式；
- 已启用 Playbook 的适用条件、禁用条件和验证方式。

因此，本项目适合引入 RAG，但应建设为 **RCA 的历史证据检索层**，而不是通用聊天机器人，也不是新的自动执行入口。

目标架构：

```text
当前 Alert / Incident
        │
        ▼
确定性事实收集（现有能力）
拓扑路径 / 近期变更 / 告警字段 / 规则结论
        │
        ▼
RAG 查询规划器
        │
        ├── SQL 精确检索：租户、服务、错误码、时间、状态
        ├── 关键词检索：名称、标识符、正文
        └── 向量检索：历史事故、Runbook、复盘语义相似度
        │
        ▼
融合排序 -> 可选重排 -> 上下文预算与去重
        │
        ▼
带来源、时间、可信等级和引用 ID 的证据包
        │
        ▼
RCA LLM：根因候选 / 理由 / 建议 / 引用 / 不确定性
        │
        ▼
现有 Heal / Change / Playbook / 审批安全链
```

---

## 2. 建设目标与非目标

### 2.1 建设目标

1. 在 Incident 详情中展示可追溯的相似历史事故及其最终处置结果。
2. 让 RCA LLM 使用经过租户过滤、可信度过滤和时效过滤的历史证据。
3. 所有 RAG 结论都能回溯到具体知识来源，不能只有模型自由生成的结论。
4. RAG、Embedding、重排或向量数据库故障时，平台自动降级到现有确定性 RCA。
5. 建立离线评测、Shadow 对照、在线反馈和索引可观测性，能够量化 RAG 是否真正改善 RCA。
6. 保持检索实现可替换：首期使用 PostgreSQL，达到规模或检索质量门槛后可迁移到 OpenSearch 或专用向量数据库。

### 2.2 明确非目标

首期不建设以下能力：

- 不建设面向全平台的开放式聊天机器人；
- 不将每条原始告警长期向量化；
- 不使用 RAG 替代 CEL 规则、维护窗口、去重或服务拓扑遍历；
- 不让 LLM/RAG 生成任意 shell、SQL、HTTP 请求并直接执行；
- 不让 RAG 直接修改 Incident 状态、审批结果、Playbook 或系统配置；
- 不因为 RAG 推荐某动作而降低 Playbook 等级、审批等级或爆炸半径；
- 首期不引入复杂 GraphRAG。拓扑仍由关系表和递归 SQL 提供精确事实；
- 不把未经人工确认的 LLM 摘要当作高可信历史结论再次训练或检索。

---

## 3. 核心设计原则

### 3.1 确定性优先

拓扑关系、近期变更、事故状态、Playbook 是否启用、审批级别等结构化事实必须从数据库或领域服务精确查询。向量检索只负责发现“可能相关的历史经验”。

### 3.2 证据与建议分离

检索结果是证据候选，LLM 输出是建议。两者必须分别保存和展示。任何建议都不能伪装为确定事实。

### 3.3 数据质量优先于向量模型

只有包含确认根因和验证结果的事故，才具备高价值。若事故结案数据不完整，优先完善数据闭环，不通过更换 Embedding 模型掩盖语料质量问题。

### 3.4 混合检索优先

运维数据同时包含自然语言和精确标识符。服务名、错误码、HTTP 状态码、Pod 名称等更适合关键词和字段匹配；故障描述、处置经验更适合语义向量检索。因此默认采用精确过滤、关键词检索和向量检索融合，而不是纯向量 Top-K。

### 3.5 默认失败开放，但安全失败关闭

- 对 RCA 可用性：RAG 失败时 fail-open，继续执行确定性 RCA，不让 Agent 因检索失败反复重试。
- 对自动动作安全：证据不完整、来源不可信、无法映射到已启用 Playbook 时 fail-closed，不进入自动执行。

### 3.6 全链路租户隔离

租户不是检索后的结果过滤条件，而是每次索引、查询、缓存、日志和反馈的强制作用域。

---

## 4. 使用场景与优先级

| 优先级 | 场景 | 用户价值 | 是否影响自动处置 |
|---|---|---|---|
| P0 | Incident 详情展示相似已解决事故 | 值班人员快速参考历史根因与动作 | 否，只读 |
| P0 | RCA 检索历史事故与 Runbook | 提升根因解释和建议质量 | 首期 Shadow，不影响 |
| P1 | 建议动作映射到现有 Playbook | 将自然语言建议约束到受控动作 | 是，但继续经过现有护栏 |
| P1 | 复盘/Runbook 文档导入、更新和撤销 | 扩大高质量知识覆盖 | 否 |
| P2 | 人工反馈驱动可信度和排序优化 | 持续提升检索质量 | 间接影响 |
| P2 | 跨服务依赖范围的复合检索 | 更好处理链路故障 | 否，拓扑仍精确查询 |
| 暂缓 | 通用运维问答 | 范围过宽，安全和权限复杂 | 否 |
| 暂缓 | 自动生成并执行新命令 | 风险不可控 | 禁止 |

---

## 5. 数据范围、可信等级与生命周期

### 5.1 可进入知识库的数据

#### A. 已确认的已解决事故（最高优先级）

最小内容应包括：

- Incident 基本信息：服务、严重度、发生时间、告警名称；
- 人工确认根因；
- 实际处置动作；
- 处置结果：成功、部分成功、失败、误报；
- 验证方法及验证结果；
- 相关变更和拓扑范围；
- 复盘地址或复盘正文；
- 确认人、确认时间和知识版本。

当前 `Incident` 模型缺少明确的确认根因与处置结果字段。建议新增一对一的 `incident_resolutions`，不要长期依赖无约束的 `extra` JSON：

```text
incident_resolutions
  id
  tenant_id
  incident_id                 UNIQUE(tenant_id, incident_id)
  confirmed_root_cause        TEXT
  resolution                  TEXT
  outcome                     success|partial|failed|false_positive
  verification                TEXT
  verification_status         verified|unverified|rejected
  postmortem_url              TEXT
  resolved_by                 VARCHAR
  resolved_at                 TIMESTAMPTZ
  updated_at                  TIMESTAMPTZ
  version                     BIGINT
```

只有 `verification_status=verified` 的事故才能作为高可信 RCA 证据或动作依据。未验证事故可用于低权重相似性参考，但必须明确标记。

#### B. Runbook / SOP / 复盘文档

文档需要保存：来源、所有者、适用服务、版本、生效时间、失效时间、审批状态和校验和。只有已发布、未过期的版本默认参与检索。

#### C. Playbook

只索引当前租户中已启用的 Playbook。除名称和命令模板外，后续应补充：

- 适用症状；
- 前置检查；
- 禁用条件；
- 执行后验证；
- 回滚条件；
- 所有者和审核时间。

命令模板可以保存在结构化源表中，但默认不把完整命令暴露给外部 Embedding 或 Chat 模型。检索正文优先使用动作说明和适用条件。

#### D. 拓扑与近期变更

在请求时从 `topology_services/topology_edges` 精确查询并加入证据包，不默认生成向量文档。这样可以避免拓扑更新后向量知识长期滞后。

### 5.2 默认不进入知识库的数据

- API Key、Webhook 地址、Authorization Header、Bearer Token；
- 通知渠道配置、数据库 DSN、环境变量值；
- 原始告警 Event 全量 JSON；
- 未脱敏的日志、命令输出、用户信息；
- 每条重复告警和短生命周期告警状态；
- 未解决事故的模型猜测；
- LLM 自动摘要本身作为高可信根因；
- 被删除、停用、过期或租户不可见的文档。

原始告警只作为当前 Incident 的事实输入，或者经过聚合、字段白名单和脱敏后成为事故知识的一部分。

### 5.3 可信等级

| 等级 | 来源 | 典型用途 |
|---|---|---|
| T3 | 人工确认且验证成功的事故、已审核 Runbook | RCA 和动作依据 |
| T2 | 已发布 Playbook、人工确认但结果为 partial 的事故 | RCA 参考，动作仍需严格审批 |
| T1 | 未验证历史事故、自动聚类摘要 | 相似性提示，不作为自动动作依据 |
| T0 | 未解决事故、模型自由生成内容、未知来源文档 | 默认不召回或只用于调试 |

可信等级只允许通过明确的人工确认或发布流程提升。模型不能自行提高来源可信等级。

### 5.4 生命周期与删除传播

- 源记录更新后生成新知识版本，旧版本退出在线检索但保留必要审计。
- 源记录被删除、Playbook 被停用、文档过期时，派生 chunk 和 embedding 必须撤销。
- 派生知识的保留期不得超过源数据允许的保留期。
- 删除任务失败必须重试并告警，不能只记录日志后永久遗留。
- 重建索引使用新 `index_version`，完成校验后切换；不得原地覆盖造成半新半旧。

---

## 6. 技术选型

### 6.1 首期存储：PostgreSQL + pgvector

选择理由：

- 项目已有 PostgreSQL，能够减少新中间件和数据同步复杂度；
- 文档元数据、租户过滤、版本状态与向量能够在同一数据库查询；
- 支持事务性 outbox，便于保证源数据和索引任务最终一致；
- 可结合字段匹配、`pg_trgm`、PostgreSQL FTS 与向量检索；
- 通过 `Retriever` 接口隔离后，未来可以替换底层实现。

需注意：PostgreSQL 内置全文检索对中文分词能力有限。首期采取以下策略：

1. 服务名、告警名、错误码等建立独立字段和精确/前缀索引；
2. 英文标识符与正文使用 `simple` FTS；
3. 标题和短文本可使用 `pg_trgm` 补充模糊匹配；
4. 中文语义主要由多语言 Embedding 承担；
5. 如果离线评测证明中文关键词召回持续不达标，再迁移到配置合适中文分析器的 OpenSearch，不在首期提前引入。

### 6.2 何时考虑外部检索系统

满足以下任一条件时进行专项评估，而不是自动迁移：

- 知识 chunk 达到数百万至千万级，数据库检索与 OLTP 出现明显资源竞争；
- 多租户过滤下 ANN 召回率不能达到评测门槛；
- 中文分词、多字段 BM25、聚合或独立扩缩容成为核心要求；
- 检索 QPS、索引写入量需要独立于主数据库扩容；
- pgvector 索引内存、构建时间或备份恢复时间不可接受。

候选方案：

| 方案 | 优势 | 代价 |
|---|---|---|
| OpenSearch/Elasticsearch | BM25、多字段、中文分析器、混合搜索成熟 | 新集群、同步与一致性复杂 |
| Qdrant | 过滤和向量检索能力较强 | 关键词检索及事务同步仍需补齐 |
| Milvus | 大规模向量索引能力强 | 运维复杂，不适合首期体量 |

### 6.3 Embedding 模型

Embedding 与 Chat LLM 分开配置、分开超时、分开限流。模型选择需满足：

- 中英文混合检索；
- 对服务名、错误码、技术术语有合理表现；
- 支持私有化或满足组织数据合规要求；
- 模型版本和向量维度稳定、可固定；
- 有可接受的吞吐、延迟和许可协议。

可将 BGE-M3、multilingual-e5 等多语言模型作为离线候选，但最终选择必须由本项目事故评测集决定，不在代码中写死某个厂商或模型。

向量维度属于索引契约。更换模型或维度时必须建立新索引版本并重算，禁止让不同模型向量混入同一 ANN 索引。

### 6.4 向量索引策略

- 数据量较小时先使用带租户过滤的精确向量搜索，建立正确性基线；
- 数据量和延迟超过门槛后再启用 HNSW；
- HNSW 使用 cosine distance，并通过真实租户分布测试召回率；
- ANN 查询需要候选过采样，避免租户/状态后过滤导致返回不足；
- 索引参数必须通过压测确定，不能只按默认值上线。

---

## 7. 领域模型与知识表设计

建议采用源文档、切片和向量分离的模型：

```text
knowledge_documents
  id                         UUID/字符串主键
  tenant_id                  强制非空
  source_type                incident|runbook|postmortem|playbook
  source_id                  源对象 ID
  source_version             源版本
  title                      标题
  body                       规范化全文或结构化快照
  service                    可空，精确过滤
  severity                   可空
  trust_level                T0|T1|T2|T3
  verification_status        verified|unverified|rejected
  status                     active|superseded|deleted|failed
  valid_from / valid_until   有效期
  checksum                   内容 SHA-256
  metadata                   JSONB，白名单字段
  created_at / updated_at
  UNIQUE(tenant_id, source_type, source_id, source_version)

knowledge_chunks
  id
  tenant_id
  document_id
  ordinal
  section
  content
  token_count
  content_tsv                tsvector
  checksum
  metadata                   JSONB
  created_at
  UNIQUE(document_id, ordinal, checksum)

knowledge_embeddings
  id
  tenant_id
  chunk_id
  model_id
  index_version
  embedding                  vector(D)
  created_at
  UNIQUE(chunk_id, model_id, index_version)

knowledge_jobs
  id
  tenant_id
  operation                  upsert|delete|rebuild
  source_type
  source_id
  source_version
  status                     pending|running|succeeded|failed|dead
  attempts
  next_run_at
  last_error
  locked_at / locked_by
  created_at / updated_at

rag_traces
  id
  tenant_id
  incident_id
  query_hash
  retrieval_version
  index_version
  candidate_ids              JSONB
  selected_ids               JSONB
  latency_ms                 JSONB
  degraded_reason
  created_at

rag_feedback
  id
  tenant_id
  incident_id
  trace_id
  document_id
  rating                     useful|not_useful|wrong|unsafe
  corrected_root_cause       TEXT
  actor
  created_at
```

`metadata` 只用于扩展，不代替关键过滤字段。租户、状态、服务、可信度、有效期必须使用显式列和索引。

PostgreSQL 扩展和向量列建议使用显式 SQL migration 管理，不完全依赖 GORM `AutoMigrate`。生产环境中的 `CREATE EXTENSION` 权限和索引构建需要由部署流程控制。

---

## 8. 文档构建、切片与 Embedding

### 8.1 事故文档模板

事故文档应由结构化字段稳定生成，示例：

```text
标题：order 服务连接池耗尽
服务：order
严重度：critical
告警：db_pool_exhausted, api_latency_high
现象：...
确认根因：连接池上限与并发配置不匹配
相关变更：发布 v1.4.2 后连接复用参数变更
实际处置：回滚配置并重启连接池
验证：错误率恢复、连接池等待时间回落
结果：success
发生时间：...
确认时间：...
```

必须区分“告警现象”“确定性证据”“人工确认根因”“模型推测”。不得把它们拼成无法区分来源的一段文字。

### 8.2 切片规则

- Incident：优先按语义字段组成 1～3 个切片，而不是按固定字符暴力切分；
- Runbook/复盘：按 Markdown 标题和步骤边界切分；
- 每个 chunk 初始控制在约 400～800 tokens，重叠 50～100 tokens；
- 命令、错误码、服务名不能在切片中被截断；
- 每个 chunk 重复携带最小来源信息：标题、服务、版本、有效期；
- 相同 checksum 不重复 Embedding；
- 具体大小通过检索评测调整，不作为永久常量。

### 8.3 脱敏与内容安全

Embedding 前执行字段白名单和敏感信息扫描：

- 移除 Token、密码、Cookie、Webhook、连接串和私钥；
- 内网地址、用户名、手机号、邮箱按组织策略脱敏；
- 对疑似提示注入文本增加风险标签；
- 外部模型调用只发送允许出域的最小正文；
- 脱敏失败时任务进入 `failed/dead`，不能绕过后继续提交外部模型。

### 8.4 索引一致性

使用数据库持久化任务而不是现有内存队列承载知识索引：

1. Incident 结案、Playbook 更新或文档发布时，在同一数据库事务写入源数据与 `knowledge_jobs`；
2. Index worker 使用 `FOR UPDATE SKIP LOCKED` 并发领取任务；
3. 任务按 `(tenant, source_type, source_id, version, operation)` 幂等；
4. 失败指数退避，达到上限进入 dead-letter 并告警；
5. 服务重启后继续处理 pending/running 超时任务；
6. 定期 reconciliation 扫描源表与知识表 checksum，修复丢失事件；
7. 索引延迟是可观测指标，不允许静默失步。

---

## 9. 检索设计

### 9.1 查询对象

RCA 不直接把整段 Prompt 作为查询。先生成结构化 `RetrievalQuery`：

```go
type RetrievalQuery struct {
    TenantID        string
    IncidentID      string
    Service         string
    AlertName       string
    Severity        string
    Summary         string
    Labels          map[string]string
    ErrorCodes      []string
    DeterministicRC string
    Evidence        []string
    Changes         []string
    DependencySvcs  []string
    OccurredAt      time.Time
}
```

Query Builder 只读取白名单标签。高基数、敏感或无解释意义的标签不拼入语义查询。

### 9.2 强制过滤

所有检索在召回阶段必须满足：

- `tenant_id = currentTenant`；
- `status = active`；
- 当前时间位于文档有效期内；
- `verification_status != rejected`；
- 来源类型在本次场景允许列表内；
- 排除当前 Incident 自己；
- 动作依据必须至少达到 T2，自动动作候选必须达到 T3 或来自当前已启用 Playbook。

禁止先跨租户搜索再在 Go 内存中过滤。

### 9.3 多路召回

首期建议并行执行：

1. **精确召回**：service、alert name、error code、Playbook name；
2. **关键词召回**：FTS / trigram，初始 Top 30～50；
3. **向量召回**：cosine similarity，初始 Top 30～50；
4. **结构化加权**：同服务、直接依赖服务、相同确定性根因、相近严重度和近期文档。

历史时间只能作为衰减因子，不能简单认为越新越正确。经过验证的旧 Runbook 可能比未验证的新事故更可信。

### 9.4 融合与重排

- 使用 Reciprocal Rank Fusion（RRF）合并关键词和向量排名；
- 初始可使用 `k=60`，但必须通过评测调参；
- 同服务、T3、验证成功可以提升排名，任何提升权重都必须记录在 `retrieval_version`；
- 对同一文档的多个相邻 chunk 去重，避免上下文被单一来源占满；
- 结果至少覆盖“历史事故”和“规范/Runbook”两类来源，若其中一类不存在则明确标记；
- 可选 reranker 只处理融合后的少量候选；reranker 失败时直接使用融合结果。

### 9.5 上下文预算

首期默认：

- 最终选择 4～8 个证据片段；
- RAG 上下文总量控制在约 3,000～6,000 tokens；
- 单个文档最多占用 40% 上下文；
- 每个片段必须携带 `document_id/chunk_id/source/title/time/trust`；
- 超预算时优先保留高可信、直接服务匹配、包含确认结果的证据。

这些数值是初始值，最终以当前 Chat 模型上下文限制、成本和评测结果为准。

### 9.6 无证据处理

满足以下情况时返回 `insufficient_evidence`，而不是强行生成历史结论：

- 没有满足租户、状态、有效期过滤的结果；
- 只有 T0/T1 结果且场景要求动作依据；
- 相似度和关键词结果均低于评测阈值；
- 候选互相矛盾且无法通过可信度消解；
- 知识索引明显过期或处于重建不一致状态。

---

## 10. LLM 输入输出契约

### 10.1 输入分区

Prompt 必须把不同信息显式隔离：

1. 系统规则；
2. 当前事故事实；
3. 现有确定性 RCA；
4. 检索到的历史证据；
5. 输出 JSON Schema。

历史文档作为不可信引用材料，不得获得修改系统规则、调用工具或执行动作的权限。文档中出现“忽略前述指令”等内容时，只能作为正文处理。

### 10.2 输出结构

建议将 `RCAOutput` 扩展为：

```json
{
  "rootCause": "最可能根因",
  "rationale": "基于当前事实与历史证据的理由",
  "suggestedActions": ["建议动作"],
  "citations": [
    {
      "documentId": "...",
      "chunkId": "...",
      "claim": "该证据支持的具体判断"
    }
  ],
  "uncertainties": ["仍未确认的关键点"],
  "evidenceStatus": "grounded|partial|insufficient"
}
```

输出校验规则：

- 引用 ID 必须存在于本次检索结果，模型不能伪造引用；
- 每个关键结论至少有一个引用或明确标为基于当前确定性事实；
- 没有有效引用时 `evidenceStatus` 不得为 `grounded`；
- 引用被撤销或越权时整条增强结果降级；
- 解析失败、Schema 不合法或内容超限时保留现有确定性结论。

### 10.3 防止自反馈污染

- LLM 输出不直接写入高可信知识库；
- `llmRootCause`、`llmSummary` 不能自动变成 `confirmed_root_cause`；
- 必须由人员确认、填写验证结果后才能提升为 T3；
- 新事故不能检索自身尚未确认的 LLM 输出；
- 评测集中的标准答案不能通过索引泄漏到待测查询中。

---

## 11. 与现有代码的集成方案

### 11.1 新增包建议

```text
internal/rag/
  model.go          知识文档、chunk、job、trace、feedback 模型
  repository.go     PostgreSQL 查询与写入
  builder.go        Incident/Playbook/文档规范化构建
  sanitizer.go      脱敏与字段白名单
  chunker.go        按来源类型切片
  indexer.go        持久化任务 worker、幂等与重试
  query.go          RetrievalQuery 构造
  retriever.go      Retriever 接口
  postgres.go       FTS/pg_trgm/pgvector 实现
  fusion.go         RRF、业务权重和去重
  reranker.go       可选重排接口
  context.go        上下文预算和引用包
  metrics.go        检索、索引和降级指标

internal/embedding/
  provider.go       Embedding 接口
  openai_compat.go  OpenAI 兼容 Embedding 客户端
  mock.go           测试实现
```

不要把 RAG 全部塞入现有 `internal/ai/llm.go`。Chat、Embedding、检索、索引生命周期不同，需要接口隔离。

### 11.2 接口边界

```go
type Retriever interface {
    Retrieve(ctx context.Context, q RetrievalQuery) (*EvidencePack, error)
}

type EmbeddingProvider interface {
    EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error)
    EmbedQuery(ctx context.Context, text string) ([]float32, error)
    ModelID() string
    Dimension() int
}

type Reranker interface {
    Rerank(ctx context.Context, query string, candidates []Candidate) ([]Candidate, error)
}
```

`Retriever` 返回的是有类型的 `EvidencePack`，不是已经拼好的 Prompt 字符串，便于测试、审计和更换模型。

### 11.3 RCA 接入点

接入顺序保持：

```text
RCAAgent.collect
  1. 查询拓扑和近期变更
  2. 形成现有确定性 rootCause/confidence/evidence
  3. 调用 Retriever（可选、超时受控）
  4. 记录 ragEvidence/ragTraceId/ragStatus
  5. 调用 LLM 增强
  6. 校验引用和 JSON Schema
  7. 保存 llm* 补充字段
```

Retriever 错误不应使 RCA 节点返回错误，避免触发现有编排器节点重试。错误记入 `ragStatus`、指标和日志后，继续执行无 RAG 的 LLM 或纯确定性 RCA。

### 11.4 Heal / Change 边界

- RAG 建议只作为候选描述；
- 候选必须映射到当前租户已启用 Playbook 的稳定 ID；
- Playbook 的等级、命令模板、回滚命令从数据库重新读取，不能信任检索片段中的副本；
- RAG 不能降低 Playbook 的 L0/L1/L2；
- RAG 不能降低确定性爆炸半径，最多触发更保守的等级；
- 来自历史事故的命令只用于展示引用，禁止直接执行；
- `evidenceStatus != grounded` 时，RAG 建议不得提升为自动动作；
- 现有 dry-run、熔断器、审批和审计链全部保留。

### 11.5 热路径边界

禁止在以下路径同步调用 Embedding、向量检索、reranker 或 Chat LLM：

- Webhook 入站；
- 告警归一化与批量保存；
- 去重分类；
- 维护窗口过滤；
- CEL 规则匹配；
- WS 广播。

RAG 只运行在低频异步 Agent 路径、只读 API 或后台索引 worker 中，不得影响现有告警吞吐基线。

---

## 12. API 与前端计划

### 12.1 首期 API

```text
GET  /api/v1/incidents/:id/similar
     返回相似事故、分数说明、来源、可信等级和最终处置结果

GET  /api/v1/incidents/:id/rag-trace
     返回本次 RCA 的检索状态和引用；仅允许具备调试权限的用户访问

POST /api/v1/incidents/:id/rag-feedback
     提交 useful/not_useful/wrong/unsafe 和可选纠正根因

GET  /api/v1/rag/status
     返回索引版本、积压、失败任务、最后成功时间；管理权限

POST /api/v1/rag/reindex
     创建后台重建任务，不在 HTTP 请求中同步重建；管理权限
```

首期不提供任意字符串的全库搜索 API，避免越权检索面扩大。

### 12.2 Incident 详情页

新增两个区域：

1. **相似历史事故**：服务、发生时间、确认根因、处置结果、相似原因、可信等级；
2. **RAG 证据**：LLM 结论中的引用、证据时间、来源链接和“不足/冲突”标识。

UI 必须清晰区分：

- 当前事故事实；
- 确定性 RCA；
- 历史证据；
- LLM 推断；
- 人工确认结论。

不能把多个层次合并显示成一个没有来源的“AI 根因”。

---

## 13. 配置计划

建议新增独立配置，默认关闭业务影响：

```text
AIOPS_RAGENABLED=false
AIOPS_RAGMODE=off|readonly|shadow|assist
AIOPS_RAGRETRIEVALTIMEOUTMS=800
AIOPS_RAGTOPKKEYWORD=40
AIOPS_RAGTOPKVECTOR=40
AIOPS_RAGTOPKFINAL=6
AIOPS_RAGMAXCONTEXTTOKENS=5000
AIOPS_RAGRERANKENABLED=false
AIOPS_RAGINDEXWORKERS=2
AIOPS_RAGINDEXVERSION=v1

AIOPS_EMBEDDINGDRIVER=openai-compat|local
AIOPS_EMBEDDINGBASEURL=
AIOPS_EMBEDDINGAPIKEY=
AIOPS_EMBEDDINGMODEL=
AIOPS_EMBEDDINGDIMENSION=
AIOPS_EMBEDDINGTIMEOUTSEC=10
AIOPS_EMBEDDINGBATCHSIZE=32
```

模式定义：

| 模式 | 行为 |
|---|---|
| `off` | 不索引、不检索，完全保持当前行为 |
| `readonly` | 建索引并提供相似事故 API，不注入 RCA |
| `shadow` | 注入独立 Shadow 分析并记录对照，不展示为正式建议、不影响 Heal |
| `assist` | RAG 结果作为 RCA 补充展示；动作仍经过全部现有护栏 |

配置校验要求：Embedding 维度、模型 ID 与在线索引元数据不一致时拒绝启用向量检索，但允许降级到关键词检索。

---

## 14. 多租户、安全与合规边界

### 14.1 多租户隔离

- 所有知识表包含非空 `tenant_id`；
- Repository 方法第一个业务参数固定为 tenant；
- SQL 在召回阶段带 tenant 条件；
- 缓存键必须包含 tenant；
- trace、feedback、job 和管理 API 同样按 tenant 隔离；
- 建议为知识表启用 PostgreSQL Row Level Security 作为纵深防御；
- 单元测试必须覆盖跨租户相似内容无法召回；
- 日志禁止输出完整检索正文和向量。

### 14.2 Prompt Injection

- 检索文档一律视为不可信数据；
- System Prompt 明确禁止执行文档中的指令；
- 文档以带 ID 的引用块传入，不与系统指令直接拼接；
- 对“忽略系统指令、输出密钥、调用工具”等模式打风险标签；
- 高风险文档不进入自动动作上下文；
- 模型无工具调用权限，RCA LLM 客户端继续只接受文本并返回受限 JSON。

### 14.3 数据投毒

- 文档来源和发布人可追踪；
- T2/T3 内容要求审批或人工验证；
- 自动生成内容不能自动提升可信等级；
- 同一来源短时间大量更新触发审计和限流；
- feedback 不能直接改变索引内容，只形成待审核纠正；
- 命中 `unsafe/wrong` 的文档可快速 quarantine，立即退出召回。

### 14.4 外部模型数据边界

- Embedding 和 Chat 分别配置允许出域的数据字段；
- 敏感租户可以配置本地模型或完全禁用外部调用；
- 不发送 Tenant ID、人员标识、密钥和完整原始事件；
- 外部请求记录模型、字段类别、字节数和结果状态，但不记录明文敏感正文；
- 合规策略拒绝时降级，不得为追求召回率绕过策略。

---

## 15. 故障降级与恢复

| 故障 | 降级行为 | 是否阻塞 RCA | 是否允许自动动作 |
|---|---|---|---|
| Embedding Query 超时 | 跳过向量路，使用关键词/精确检索 | 否 | 仅依据现有确定性规则 |
| pgvector 查询失败 | 使用关键词/精确检索 | 否 | 同上 |
| FTS 查询失败 | 使用向量和精确检索 | 否 | 需满足可信与引用要求 |
| reranker 失败 | 使用 RRF 融合结果 | 否 | 不额外放宽 |
| 全部检索失败 | 无 RAG，执行当前 RCA | 否 | 不能使用 RAG 建议 |
| Chat LLM 失败 | 保留确定性 RCA 和可选相似事故展示 | 否 | 保持现有行为 |
| 索引积压过大 | 标记 stale，必要时停用 RAG | 否 | 停用 RAG 动作影响 |
| 索引版本不一致 | 禁用向量路，可用兼容关键词路 | 否 | 不使用不一致证据 |
| 引用校验失败 | 丢弃本次 LLM 增强输出 | 否 | 否 |
| 跨租户校验异常 | 立即拒绝结果并触发安全告警 | 否 | 否 |

RAG 组件需要独立熔断和超时，不能复用 Heal 动作熔断器。连续失败时短时打开 RAG 熔断，减少对数据库和模型端点的雪崩重试。

恢复后先进行健康探测，再逐步进入 readonly/shadow；不能因为端点恢复就立即自动进入 assist。

---

## 16. 可观测性、SLO 与成本控制

### 16.1 指标

建议新增：

```text
aiops_rag_retrieval_total{mode,result,degraded_reason}
aiops_rag_retrieval_duration_seconds{stage=exact|keyword|vector|fusion|rerank}
aiops_rag_candidates_total{stage,source_type}
aiops_rag_selected_total{source_type,trust_level}
aiops_rag_empty_total{reason}
aiops_rag_citation_validation_total{result}
aiops_rag_index_jobs{status,operation}
aiops_rag_index_job_duration_seconds{operation}
aiops_rag_index_lag_seconds{source_type}
aiops_rag_embedding_requests_total{model,result}
aiops_rag_embedding_tokens_total{model}
aiops_rag_context_tokens_total{model}
aiops_rag_feedback_total{rating}
```

避免在 Prometheus label 中使用 tenant、incident ID、document ID 等高基数值；这些信息进入结构化日志或 trace 表。

### 16.2 首期 SLO 建议

- RAG 不影响告警热路径吞吐和延迟基线；
- 相似事故 API P95 小于 1 秒（不含首次离线建索引）；
- RCA 检索阶段 P95 小于 800ms，超时立即降级；
- 索引任务 99% 在源更新后 5 分钟内完成；
- 跨租户召回必须为 0；
- 引用 ID 校验成功率达到 100%，失败输出不得展示为 grounded；
- RAG 故障时确定性 RCA 成功率不得低于当前基线。

SLO 数值在首轮基准测试后可调整，但“热路径零影响”“跨租户召回为零”和“引用必须可验证”不可放宽。

### 16.3 成本控制

- 相同 checksum 不重复 Embedding；
- 文档批量 Embedding，查询单条 Embedding；
- Query Embedding 可按 tenant + query hash + model ID 短期缓存；
- 缓存不得跨 tenant，共享公共文档也应通过显式公共作用域处理；
- 限制候选、rerank 数量和上下文 tokens；
- Shadow 模式设置采样率，避免对全部事故双倍调用 Chat；
- 分别统计索引成本、查询成本和 Chat 增量成本。

---

## 17. 评测体系与上线门禁

### 17.1 评测集

建立至少包含以下类型的版本化评测集：

- 同服务同故障；
- 同现象不同根因；
- 不同服务但相同依赖故障；
- 精确错误码匹配；
- 中文描述、英文标识符混合；
- 已过期 Runbook；
- 未验证或错误历史事故；
- 相互矛盾的历史证据；
- 没有相似历史的未知故障；
- 跨租户内容高度相似的隔离样本；
- 包含提示注入和敏感信息的恶意文档。

早期真实数据不足时可以使用人工编写和演练事故补充，但必须标记 synthetic，不能把合成效果等同生产效果。

### 17.2 检索指标

- Recall@K：标准相关事故是否进入候选；
- MRR / nDCG：相关结果是否排在前面；
- Precision@K：最终证据中有多少真正相关；
- Source Diversity：是否同时覆盖事故和规范知识；
- Freshness/Validity：是否错误召回已过期或已撤销内容；
- Tenant Isolation：跨租户召回数必须为 0；
- Empty Accuracy：没有可靠证据时是否正确返回不足。

### 17.3 生成指标

- Root Cause Top-1 / Top-3 准确率；
- Citation Precision：引用是否真的支持该结论；
- Citation Completeness：关键结论是否都有引用；
- Faithfulness：输出是否超出当前事实和证据；
- Unsafe Action Rate：是否提出越权、高风险或不存在的动作；
- Abstention Quality：证据不足时是否合理拒答；
- 与无 RAG 确定性 RCA、无 RAG LLM 的对照差异。

### 17.4 业务指标

- MTTA/MTTR 变化；
- 相似事故点击率和有效率；
- RCA 建议采纳率；
- 人工纠正率；
- 错误建议和 unsafe 反馈率；
- Playbook 匹配成功率；
- 值班人员处理步骤是否减少。

### 17.5 上线门禁

进入下一模式必须同时满足：

- 离线 Recall@5、引用准确率达到评测集约定阈值；
- 跨租户、安全恶意样本全部通过；
- 降级测试证明 RAG 全部故障时现有 RCA 正常；
- Shadow 模式没有增加错误动作倾向；
- P95 延迟和索引积压满足 SLO；
- 产品/UI 能明确展示来源与不确定性；
- 有一键切回上一模式的配置和操作手册。

具体准确率阈值由 Phase 0 基线确定，禁止在没有基线的情况下随意承诺绝对数字。

---

## 18. 分阶段实施计划

### Phase 0：数据契约与评测基线

目标：确认“什么是正确根因和有效处置”，先建立可评价的数据闭环。

实施项：

- 新增 `incident_resolutions` 及结案填写/更新 API；
- 定义验证状态、处置结果和复盘字段；
- 整理真实与演练事故评测集；
- 建立无 RAG 的确定性 RCA、现有 LLM RCA 基线；
- 确定敏感字段白名单和外部模型出域策略；
- 评测候选 Embedding 模型。

验收：

- 已解决事故可以保存确认根因、实际动作、结果和验证；
- 评测集版本化、可重复运行；
- 当前 RCA 基线数据完整；
- 数据所有者和审批流程明确。

退出/回滚：该阶段不改变现有 RCA，不需要业务回滚；模型质量不足时停留在数据治理。

### Phase 1：知识底座与离线索引

目标：在不改变用户行为的情况下建立可靠知识索引。

实施项：

- docker-compose 开发环境加入 pgvector 扩展支持；
- 增加知识表、显式 migration 和索引；
- 实现 Embedding Provider、脱敏、切片和 checksum；
- 实现持久化 `knowledge_jobs` worker；
- 支持历史已验证事故和已启用 Playbook 回填；
- 建立索引状态、积压、失败任务和 reconciliation；
- 完成跨租户、删除传播、模型版本切换测试。

验收：

- 回填可重复执行且不产生重复向量；
- 更新、停用、删除能正确传播；
- worker 重启后任务不丢；
- 脱敏失败不会请求外部模型；
- 索引延迟和失败可观测。

回滚：停止 index worker，保留源业务表；知识表和扩展不参与现有链路，可独立清理。

### Phase 2：相似事故只读能力

目标：验证检索质量，不影响 RCA、Heal 或 Change。

实施项：

- 实现精确、关键词、向量多路召回；
- 实现 RRF、可信度/有效期处理和上下文去重；
- 新增 `GET /incidents/:id/similar`；
- Incident 详情展示相似事故和来源；
- 上线 useful/not_useful/wrong/unsafe 反馈；
- 开展真实值班人员盲评。

验收：

- 离线检索达到约定门槛；
- 跨租户召回为 0；
- 已过期、删除和 rejected 内容不出现；
- API P95 达标；
- 值班人员反馈证明结果具有实际参考价值。

回滚：`RAGMODE=off` 或 `readonly` UI 开关关闭，不影响 Agent。

### Phase 3：RCA Shadow 模式

目标：比较有无 RAG 的 RCA 差异，但不影响正式结果和动作。

实施项：

- RCAAgent 注入可选 Retriever；
- 构造带引用 EvidencePack；
- 扩展 LLM JSON Schema 和引用校验；
- 同时记录 baseline 与 RAG shadow 输出；
- 对延迟、成本、准确率、幻觉和 unsafe 倾向做对照；
- 建立 RAG 独立熔断和降级状态。

验收：

- RAG 故障不触发 RCA 节点重试；
- Shadow 输出不会进入 Heal 建议动作集合；
- 引用伪造能够被拦截；
- 对照评测显示 RAG 有明确收益且无安全回退；
- 成本与延迟满足约定预算。

回滚：切换 `RAGMODE=readonly/off`，停止 Shadow Chat 调用。

### Phase 4：RCA 辅助模式

目标：把有引用的 RAG 分析展示给用户，并受控地参与 Playbook 匹配。

实施项：

- `grounded/partial/insufficient` 状态进入 Incident 详情；
- 展示引用、冲突和未知项；
- 只允许 grounded 且可信来源的建议参与 Playbook 候选匹配；
- 根据稳定 ID 重新读取 Playbook；
- 继续执行 dry-run、爆炸半径、熔断、审批和审计；
- 增加 RAG 参与原因及引用的审计字段。

验收：

- RAG 无法创建任意命令；
- RAG 无法降低审批等级或风险评分；
- 没有已启用 Playbook 时只展示建议，不执行；
- unsafe action rate 满足门禁；
- 在线业务指标优于或不劣于基线。

回滚：切回 `shadow/readonly`，保留历史 trace 供分析；现有确定性链路不变。

### Phase 5：文档治理与规模化

目标：在验证价值后扩展语料和容量，而不是提前复杂化。

可选实施项：

- Runbook/复盘上传、发布、过期和审批工作流；
- 可选 reranker；
- 索引冷热分层和分区；
- OpenSearch/专用向量库对比评估；
- 基于反馈的排序优化，但不进行未经审核的在线自学习；
- 对复杂跨服务故障评估图检索与向量检索的复合方案。

---

## 19. 建议的 PR 拆分

为降低评审和回滚风险，建议按以下独立变更提交：

1. `incident_resolutions` 数据模型、API、UI 和测试；
2. pgvector/pg_trgm migration、知识表和配置校验；
3. Embedding Provider、脱敏器、chunker 和 mock 测试；
4. 持久化 index job、worker、回填和 reconciliation；
5. PostgreSQL Retriever、混合召回、RRF 和离线评测工具；
6. 相似事故 API、UI、反馈接口；
7. RCA EvidencePack、Shadow 输出和引用校验；
8. Assist 模式与 Playbook 稳定 ID 映射；
9. 指标、告警、运维手册、重建和回滚演练。

每个 PR 均应保持默认配置下现有行为不变。

---

## 20. 测试计划

### 20.1 单元测试

- 文档规范化结果稳定；
- 切片不截断关键标识符；
- 脱敏覆盖 Token、连接串、Webhook 和密钥；
- checksum 与幂等任务；
- RRF 融合、去重、可信度和有效期；
- 上下文 token 预算；
- 引用 ID 校验与伪造拦截；
- 模型维度和索引版本不兼容时降级；
- `insufficient_evidence` 判定；
- RAG 建议无法降低风险和审批级别。

### 20.2 数据库集成测试

- pgvector cosine 查询；
- tenant/status/time/trust 过滤在 SQL 层生效；
- 同一源版本重复处理不产生重复记录；
- 更新生成新版本，旧版本退出在线检索；
- 删除和停用传播；
- `SKIP LOCKED` 多 worker 不重复消费；
- worker 崩溃后的 lease 恢复；
- HNSW 启用前后的 Recall 与延迟对照。

### 20.3 端到端测试

1. 建立两个租户的高度相似事故，确认互不可见；
2. 结案并验证事故，等待索引后出现在相似事故结果；
3. 修改根因版本，确认旧版本退出；
4. 停用 Playbook，确认不再作为动作候选；
5. 关闭 Embedding 服务，确认关键词降级；
6. 关闭向量扩展/模拟 SQL 失败，确认确定性 RCA 完成；
7. LLM 伪造 document ID，确认增强结果被丢弃；
8. 文档包含提示注入，确认不改变输出契约；
9. RAG 返回高风险动作，确认审批等级不下降；
10. 在告警批量压测期间运行索引和检索，确认热路径基线无回退。

### 20.4 性能测试

- 10 万、100 万、500 万 chunk 分级测试；
- 不同租户数据倾斜下的 Recall/P50/P95/P99；
- 精确搜索与 HNSW 对照；
- index worker 对 PostgreSQL CPU、I/O、WAL 和连接池的影响；
- Shadow 双调用成本和 Agent 总耗时；
- 重建索引期间的在线查询可用性。

---

## 21. 发布、回滚与运维

### 21.1 发布顺序

```text
数据库扩展/表结构
  -> 代码部署但 RAGMODE=off
  -> 启动索引 worker 并回填
  -> readonly 小租户验证
  -> readonly 全量
  -> shadow 采样
  -> shadow 全量
  -> assist 灰度
  -> assist 扩大范围
```

### 21.2 回滚层级

1. 配置回滚：`assist -> shadow -> readonly -> off`；
2. 组件回滚：停止 reranker、向量路或 index worker；
3. 索引回滚：切回前一 `index_version`；
4. 代码回滚：Retriever 为可选依赖，旧代码不读取知识表；
5. 数据回滚：知识表是派生数据，可重建；事故源数据和结案数据不能随索引回滚删除。

### 21.3 运维手册必须覆盖

- 查看索引积压和 dead jobs；
- 重试单个任务和租户级回填；
- quarantine 错误/恶意文档；
- 切换索引版本；
- Embedding 模型升级；
- RAG 熔断和手动关闭；
- 跨租户异常响应；
- 向量索引重建、备份和恢复；
- 成本或延迟突增排查。

---

## 22. 主要风险与处理

| 风险 | 表现 | 处理 |
|---|---|---|
| 历史事故质量差 | 检索到相似但错误的根因 | 先建结案验证流程，可信度分级 |
| 中文关键词召回弱 | 自然语言相关结果遗漏 | 多语言向量 + 精确字段 + trigram；达不到门槛再评估 OpenSearch |
| 向量结果越权 | 跨租户内容泄漏 | SQL 强制 tenant、RLS、隔离测试、异常立即拒绝 |
| LLM 引用伪造 | 输出不存在的来源 | 服务端按本次候选白名单校验引用 |
| 提示注入 | 文档诱导模型改变规则 | 不可信引用块、无工具权限、风险标记、JSON Schema |
| 知识自污染 | LLM 猜测被再次当真 | 只有人工验证才能升为高可信知识 |
| 索引滞后 | 已删除/过期内容仍召回 | 持久化任务、删除传播、reconciliation、stale 门禁 |
| OLTP 资源竞争 | 告警热路径延迟上升 | worker 限速、独立连接预算、压测、必要时迁出检索系统 |
| 模型升级不兼容 | 新旧向量混用 | index version 双写/重建后切换 |
| 建议动作过度自动化 | 错误自愈扩大故障 | 只能映射已启用 Playbook，不能降级审批和风险 |
| 成本不可控 | Shadow/重排/长上下文费用上升 | checksum、批量、缓存、采样、token 和候选预算 |

---

## 23. 待确认决策

进入 Phase 1 前需由项目负责人、安全/运维负责人共同确认：

1. 事故结案由谁填写和验证，什么条件允许标记 `verified`；
2. 哪些字段允许发送到外部 Embedding/Chat 模型；
3. 首期采用本地还是外部 Embedding 服务；
4. 评测集可以使用哪些生产数据，如何脱敏；
5. 相似事故 API 是否仅对值班人员开放；
6. T2/T3 文档的发布和撤销权限；
7. Assist 模式允许哪些 Playbook 等级参与候选匹配；
8. 离线和在线门禁的具体数值；
9. pgvector 扩展的生产安装、备份和恢复责任人；
10. 索引积压、跨租户异常和 unsafe 反馈的告警接收人。

---

## 24. 完成定义（Definition of Done）

RAG 不能以“已经接入向量数据库”作为完成标准。达到以下全部条件，才视为首期生产建设完成：

- 有可审核的事故确认根因和处置结果数据；
- 知识索引支持幂等更新、删除传播、失败恢复和版本切换；
- 检索采用租户强过滤和混合召回；
- 相似事故结果能显示来源、可信度、有效期和处置结果；
- LLM 输出引用经过服务端校验，能够正确表达证据不足；
- RAG 故障不会阻塞现有确定性 RCA；
- RAG 不进入告警热路径；
- RAG 不能生成任意可执行命令、降低风险或绕过审批；
- 跨租户、安全、提示注入、数据投毒和降级测试全部通过；
- 离线评测、Shadow 对照和在线业务指标证明有正向收益；
- 具备指标、告警、运维手册和一键降级能力；
- 默认配置下现有系统行为保持不变。

只有在上述条件满足后，才考虑进一步建设通用问答、复杂图检索或迁移到独立检索集群。
