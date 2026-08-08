# AIOps 运维概念与黑话入门

> 面向**不太熟悉运维/系统设计**的读者。本文不解释代码，只解释**文档里出现的术语**：每个词先给一句话大白话，再说明它在**本项目里对应什么**、为什么存在。
> 阅读本文前建议先看 [README](../README.md) 的总览，再对照 [GUIDE.md](GUIDE.md) 使用。

---

## 目录

1. [告警系统的基本概念](#1-告警系统的基本概念)
2. [监控生态（Prometheus 系）](#2-监控生态prometheus-系)
3. [告警在平台内的处理](#3-告警在平台内的处理)
4. [Agent 自愈与安全护栏](#4-agent-自愈与安全护栏)
5. [性能与压测](#5-性能与压测)
6. [网络与连接](#6-网络与连接)
7. [数据库（PostgreSQL）](#7-数据库postgresql)
8. [可靠性与发布](#8-可靠性与发布)
9. [软件设计概念](#9-软件设计概念)
10. [AI / LLM](#10-ai-llm)
11. [常用缩写速查表](#11-常用缩写速查表)

---

## 1. 告警系统的基本概念

| 术语 | 大白话 | 在本项目里 |
|---|---|---|
| **告警（Alert）** | 一条「出事了」的通知，比如「pay 服务 CPU 高」 | `alerts` 表里的一行；前端「告警」页看到的就是它 |
| **事件（Event）** | 平台收到告警的**原始信息**，还没被处理 | webhook 解析出的 `IngestEvent`（名称、严重度、标签、原始快照） |
| **事故（Incident）** | 多个相关告警聚合成**一个需要人跟进的案子** | `incidents` 表；前端「Incidents」页。一条事故可挂多条告警 |
| **严重度（Severity）** | 这件事多严重，决定处理优先级 | `critical > high > medium > low > info`；≥high 才进 Agent 智能路径 |
| **状态（Status）** | 一条告警/事故当前处于什么阶段 | 告警：`firing`(正在报) / `resolved`(已恢复) / `acknowledged`(已认领) / `suppressed`(被静默)；事故：`open / pending_approval / resolved` |
| **firing / resolved** | firing=正在触发中（报错中）；resolved=已经恢复（不再报） | 一条告警 resolved 后，同样内容再来会**重新触发**，而不是被当成重复 |
| **acknowledged（认领）** | 值班人点「确认」，表示「我看到了，正在处理」，防止重复报警刷屏 | `POST /alerts/:id/ack`；相当于钉钉/飞书里的「我已收到」 |
| **suppressed（静默）** | 已知会吵（比如正在维护），先让它闭嘴 | 维护窗口命中时告警以该状态入库，不建事故、不进 Agent |
| **去重（Dedup）** | 同一件事反复报，只保留一条有效处理 | 见[第 3 节](#3-告警在平台内的处理)；防止「每 30 秒报一次」刷几百条 |
| **心跳（Heartbeat）** | 探活/重复上报的规律信号，本身没信息量 | 维护窗口期内同内容告警被当 duplicate，避免窗口期反复写库 |
| **维护窗口（Maintenance Window）** | 预约一段「这段时间别报警」，如周三凌晨迁移 | `maintenance_windows` 表；命中窗口的 firing 告警被静默 |
| **关联规则（Correlation Rule）** | 定义「什么样的告警聚成事故」的规则 | `rules` 表；用 CEL 表达式写匹配条件，命中后按分组字段聚合 |
| **CEL 表达式** | 一种**给规则写判断条件**的表达式语言，类似 `if` 但专为这种场景 | 例：`alert.severity == 'critical' && labels.service == 'pay'` |
| **阈值（Threshold）** | 达到某个数/值才动作 | 规则 `threshold`：分组内命中次数 ≥N 才建事故；AI 聚类 `AIClusterThreshold`：≥3 条才确认建事故 |
| **指纹（Fingerprint）** | 一条告警的**逻辑身份证**——同名同源算同一条 | `Fingerprint(provider, name, labels)` 哈希；两个内容不同但「本质同一件事」的告警指纹相同 |
| **去重结果三态** | 每次告警进平台都要判定：新的 / 内容变了 / 完全重复 | `new`(新) / `update`(刷新) / `duplicate`(丢弃)。见[第 3 节](#3-告警在平台内的处理) |

---

## 2. 监控生态（Prometheus 系）

| 术语 | 大白话 | 在本项目里 |
|---|---|---|
| **Prometheus** | 最流行的开源监控系统：持续采集各服务的指标数据，按规则触发告警 | 本平台是它的**告警接收方**——Prometheus 报警时把信息 POST 过来 |
| **Alertmanager** | Prometheus 的告警管理组件：决定告警发给谁 | 本平台模仿 Alertmanager 的 webhook 格式，可直接对接 |
| **Webhook** | 一个「发生事情就通知你」的 HTTP 回调地址 | `POST /api/v1/webhook/prometheus`：Prometheus 报警时把 JSON 推到这里 |
| **Provider（数据源）** | 告警来源的抽象 | 内置 `prometheus`、`log`（演示）；要接阿里云等就加一个 provider |
| **Label（标签）** | 给告警打上的键值对「属性」，如服务名、机房 | `labels.service="pay"`；指纹计算、维护窗口匹配、规则分组都靠它 |
| **instance / job** | Prometheus 语境：instance=具体实例地址，job=采集任务名 | 这两个标签**不参与指纹**（它们变来变去，不该算「不同告警」） |
| **拉模式 vs 推模式** | 监控「自己来取数据」还是「别人把数据送来」 | Prometheus 是**拉模式**（主动抓指标）；本平台是**推模式**（等 webhook 送告警进来） |

---

## 3. 告警在平台内的处理

| 术语 | 大白话 | 在本项目里 |
|---|---|---|
| **管线（Pipeline）** | 一条告警进来后被依次处理的「流水线」：清洗→判定→入库→通知 | `Pipeline.ProcessEvents`；每一步是一个环节 |
| **热路径（Hot Path）** | 每一条告警都要走的、要求最快的路径 | 去重、入库、规则关联、WS 推送——本项目耗时优化全部打在这条路径上 |
| **智能路径（Agent 路径）** | 只有部分告警要走、更重、更慢、但更聪明的路径 | Agent 编排（monitor→rca→heal→change），异步、可重试 |
| **双轨架构** | 同时有「快而简单」和「慢而聪明」两条处理轨道 | 确定性热路径（快）+ Agent 智能路径（聪明），互不阻塞 |
| **入库（Save / Persist）** | 把数据写进数据库保存下来 | `SaveAlert` / `SaveAlerts`；每次写库都是一次「数据库往返」 |
| **DB 往返（Round Trip）** | 「问一次数据库」这个动作；越少越快 | 一条告警从 4 次往返优化到 2 次，再到**每批 2 次**——这就是 Phase 3 系列做的核心事 |
| **批量写（Batch Write）** | 攒一批一起写，而不是一条写一次 | `SaveAlerts`：一次请求带 N 条告警时，用一条 SQL 批量写，请求延迟大降 |
| **CTE（WITH 子句）** | 一条 SQL 里「先算临时结果，再复用」，能一次完成多步 | 写库用 `WITH ins AS (INSERT alerts...) ups AS (INSERT last_alerts...) SELECT 1`：一次往返双表写入 |
| **折叠（Collapse）** | 批里同一指纹的多条，只保留最后一条的状态 | 批量写时 `last_alerts` 在内存里折叠，避免违反数据库「同一行不能改两次」的限制 |
| **overlay（叠加层）** | 在已有数据之上「模拟」还没发生写入的中间状态 | 批内去重用内存 overlay 模拟「前一条已经写进去了」，保证批内先后顺序正确 |
| **副作用（Side Effect）** | 写完库之后附带要做的其他事 | 规则评估、WS 广播、投递 Agent/工作流任务——都在批量写之后逐条做 |
| **入库是原子（Atomic）** | 要么整批成功、要么整批失败，不会写一半 | 单语句 CTE 写库失败 = 一行都没写，可安全重试/降级 |
| **幂等（Idempotent）** | 同一操作做 1 次和做 100 次，结果一样 | 迁移/回填都设计成幂等，重启不炸；重放告警不会重复建事故 |

---

## 4. Agent 自愈与安全护栏

| 术语 | 大白话 | 在本项目里 |
|---|---|---|
| **Agent（智能体）** | 一个「会思考并做判断」的自动化环节 | 4 个节点：`monitor`(确认出事) → `rca`(查根因) → `heal`(提出修法) → `change`(评估改动风险) |
| **编排器（Orchestrator）** | 按固定顺序驱动多个 Agent 依次干活的总指挥 | `orchestrator.Orchestrator`；状态机调度 + 每步结果落库（检查点） |
| **状态机（State Machine）** | 「当前处于哪一步、能走到哪一步」的严格定义 | 事故/编排状态：`open → investigating → healing → pending_approval → resolved` |
| **检查点（Checkpoint）** | 每完成一步就把进度存下来，崩了能接着走 | `incident_states.node_results`：monitor/rca/heal/change 各自的结果都持久化 |
| **RCA（根因分析）** | 出了故障，找出「到底为什么」 | `rca` 节点；确定性证据链（拓扑、最近变更、依赖链）+ 可选 LLM 补充自然语言结论 |
| **Bayes（贝叶斯证据链）** | 用多条证据互相印证来推算「哪个原因更可能」 | RCA 的确定性部分：把依赖关系、最近变更等作为证据加权判断 |
| **自愈（Heal / Self-healing）** | 出了小毛病自动修掉，不用等人工 | `heal` 节点：根据根因建议「重启/回滚/扩容」等动作 |
| **剧本（Playbook）** | 预置的「每种故障怎么修」的手册 | `playbooks` 表：每条含级别、命令模板、爆炸半径系数、回滚命令 |
| **Dry-run（试跑）** | 只做「演练/预览」，不真正执行 | 默认 `AIOPS_AGENTDRYRUN=true`：heal 只产出动作并给出 `status=dry_run`，不真执行 |
| **爆炸半径（Blast Radius）** | 这个动作可能波及多大范围/多少服务 | 每个动作有固有系数（如回滚 0.35）；超阈值 `AIOPS_AGENTBLASTRADIUSLIMIT=0.2` 强制降级到 L2 人工审批 |
| **熔断器（Circuit Breaker）** | 连续失败多次后「先断开，别再试了」，过一会再放开 | 自愈连续 5 次失败 → 熔断 60s，期间不再自动执行 |
| **降级（Downgrade）** | 原本能自动做的，因风险改成需要人工 | 爆炸半径超限 → 动作强制降级为 L2 人工审批 |
| **分级审批（L0/L1/L2）** | 按风险分等级决定「谁点头才能做」 | L0=低风险自动执行；L1=oncall 确认；L2=技术负责人人工审批（最高风险） |
| **oncall（值班）** | 轮班负责处理故障的人 | 审批 `actor` 字段，如 `oncall-engineer`；L1 级需要这类人确认 |
| **审批闭环** | 「要人点头才能做」的完整流程 | `POST /incidents/:id/approval`；approve 就执行+闭环，reject 保持 open |
| **审计（Audit）** | 每一步关键操作都留痕，出了问题能追责 | `audit_logs` 表：who(谁)/when(何时)/what(做了什么)/why(理由) + correlation_id |

---

## 5. 性能与压测

| 术语 | 大白话 | 在本项目里 |
|---|---|---|
| **吞吐（Throughput）** | 每秒能处理多少件 | 压测用「告警/秒」：如 1786/s ≈ 每小时 640 万 |
| **QPS / RPS** | 每秒请求数 / 每秒事务数 | loadgen 的 `-rate`（告警/秒，事件口径） |
| **并发（Concurrency）** | 同时有多少个「正在进行的动作」 | `-concurrency 300`：同时 300 个请求在跑 |
| **延迟（Latency）** | 一个请求从发出到响应花了多久 | 压测统计 P50/P99；延迟越低越好 |
| **P50 / P95 / P99** | 按耗时从小到大排序，**第 50/95/99 百分位的值** | P50=一半请求快于它；P99=99% 请求快于它（代表「最差的那批」）。**看 P99 比看 P50 更能发现慢的问题** |
| **直方图（Histogram）** | 把一堆耗时按区间分桶统计，看分布 | `/metrics` 的 `aiops_pipeline_duration_seconds_bucket`：有多少条落在 ≤0.05s、≤0.1s 等区间 |
| **基准（Baseline）** | 事先测好的「正常值」，用来和以后对比 | `.bench/baseline.json`；`make regress` 把本次结果和它比 |
| **回归（Regression）** | 改完代码后性能/功能变差了 | `-compare` 判定：吞吐 < 0.8× 或 P50/P99 > 1.5× 基线 → 判回归，非零退出 |
| **看门狗（Watchdog）** | 自动盯着、出问题就报警/退出的机制 | `make regress` 当 CI 看门狗；loadgen 失败率 >1% 非零退出可接 CI |
| **留痕** | 把压测结果存成文件备查 | `-summary latest.json`：每次压测的 JSON 汇总 |
| **profile 一致** | 压测参数要一样，对比才有意义 | `-compare` 要求 rate/batch 等与基线一致，否则直接判失败（吞吐与 rate 强相关） |

---

## 6. 网络与连接

| 术语 | 大白话 | 在本项目里 |
|---|---|---|
| **回环（Loopback）** | 本机访问本机（127.0.0.1），不经过网卡出去 | 压测在 Windows 本机跑；回环有特殊行为（连接队列浅、TIME_WAIT 容易爆） |
| **TIME_WAIT** | TCP 连接关闭后内核保留的一小段「残留记录」，防止乱序包误判 | 压测早期每请求新建连接 → 大量 TIME_WAIT 挤爆 → 客户端误报服务端故障（实际是客户端自己的问题） |
| **keep-alive（连接复用）** | 用完的连接不关，下个请求接着用 | 修复后连接复用率 98.6%：387 次新建服务 27000 个请求 |
| **连接池（Connection Pool）** | 预先建好一批数据库连接反复用，避免每次新建 | `DBMaxOpenConns=20 / DBMaxIdleConns=20`；空闲连接不会被反复关闭重建（防抖动） |
| **慢连接 / 僵尸连接** | 占着不放、响应又慢的连接 | HTTP `WriteTimeout` 30s：太久没响应就断开，释放资源 |
| **slowloris（慢速攻击）** | 故意一点一点发请求头，拖住服务器 | `ReadHeaderTimeout=5s` 专门防它：请求头 5s 内没发完就断开 |
| **超时（Timeout）** | 等多久就放弃，别无限等 | 读 15s / 写 30s / 空闲 120s；LLM 调用整体超时 15s |

---

## 7. 数据库（PostgreSQL）

| 术语 | 大白话 | 在本项目里 |
|---|---|---|
| **PostgreSQL（PG）** | 一款开源关系数据库，本项目唯一存储 | docker-compose 启动；所有表都在里面 |
| **DSN** | 连接数据库的「地址+账号」串 | `host=127.0.0.1 user=aiops password=aiops dbname=aiops port=5432 ...` |
| **docker / 容器** | 把程序+环境打包成一体的「沙箱」，一键启动 | PostgreSQL 跑在容器 `aiops-postgres` 里；`make infra` 启动 |
| **表（Table）** | 一类数据的「表格」 | `alerts` 表存告警、`incidents` 表存事故……见 [GUIDE §3](GUIDE.md#3-数据模型) |
| **索引（Index）** | 数据库给某列建的「快速查找目录」 | 如 `(tenant_id, fingerprint)` 唯一索引：按租户+指纹找当前状态，从全表扫描变成直接命中 |
| **唯一索引（Unique Index）** | 同一组合的值只允许出现一次 | `last_alerts` 的 `(tenant_id, fingerprint)` 唯一：每个逻辑告警只有一条「当前状态」 |
| **分区表（Partition）** | 一张大表按某个值（如月份）切成多张物理子表 | `alerts` 按 created_at 月分区：`alerts_p202608`；查询/清理只碰相关月份，更快 |
| **保留策略（Retention）** | 超过一定时间的数据自动删除 | alerts 30 天 / 审计 180 天 / 工作流记录 90 天；整月过期的分区直接 DROP |
| **CTE** | 见[第 3 节](#3-告警在平台内的处理) | 写库单语句 + 影响面递归查询都用它 |
| **递归 CTE（WITH RECURSIVE）** | SQL 里「一层层往上查」的写法，用于图/层级 | 影响面查询：从故障服务反推「谁依赖我」的整条链 |
| **JSONB** | PostgreSQL 里存 JSON 数据（还支持查询）的列类型 | `alerts.event`、`labels`、`node_results` 等都用它；写入要 `?::jsonb` 显式转换 |
| **WAL（预写日志）** | 数据库先把变更记日志再改数据，崩了能恢复 | 整分区 DROP 几乎不产生 WAL、不锁表 → 清老数据又快又省 |
| **VACUUM** | 清理删除后留下的「垃圾空间」，整理表 | 压测反复删数据会加剧表/索引膨胀；VACUUM 后可能反而更慢（环境波动） |
| **锁等待 / LWLock** | 多个写操作互相等锁，会变慢 | 压测中发现瓶颈在 PG 写入的锁竞争 + Docker 磁盘 IO，不是应用代码 |
| **事务（Transaction）** | 一组操作要么全成功要么全失败 | 迁移/批量写都是事务；中途失败自动回滚到最初状态 |
| **ON CONFLICT** | 插入撞上唯一约束时「怎么办」（更新/忽略） | last_alerts upsert：冲突就更新当前状态 |
| **约束（Constraint）** | 数据库强制的规定（唯一、非空等） | 如「同一行在同一语句里不能改两次」→ 触发报错，所以批量写要在内存里折叠 |

---

## 8. 可靠性与发布

| 术语 | 大白话 | 在本项目里 |
|---|---|---|
| **CI/CD** | 持续集成/持续部署：自动测试 + 自动发布 | 本仓库有回归压测目标，可接 CI 当门禁（当前无 remote，本地当看门狗用） |
| **回滚（Rollback）** | 改坏了就退回到上一个版本 | 自愈剧本的 `rollbackCommand`；回滚动作爆炸半径大 → 强制 L2 审批 |
| **灰度（Canary）** | 先放一小部分流量试，稳了再全量 | 本仓库未做；是发布常用手段，了解即可 |
| **优雅退出（Graceful Shutdown）** | 关服前先把手头的事收尾，不粗暴杀 | 收到 Ctrl+C → 停消费者 → 5s 内等 HTTP 请求收尾再退出 |
| **健康检查（Health Check）** | 让别人确认「我还活着、能用」的接口 | `GET /healthz` → `{status:"ok"}`；compose 探针/负载均衡用它 |
| **探针（Probe）** | 定时来「戳一下」健康检查确认存活 | 平台对外暴露 `/healthz` 供探针用 |
| **种子（Seed）** | 首次启动时预置的默认数据 | 默认剧本、演示工作流、通知渠道 env 首启种子；「只插不覆盖」保证不会覆盖用户改过的 |
| **环境变量（Env Var）** | 进程外的配置开关，不改代码就能改行为 | 全部 `AIOPS_*` 前缀；见 [README 配置项总表](../README.md) |
| **fail-open（故障放行）** | 依赖挂了也不阻塞主流程，降级继续干 | 通知/LLM/工作流 agent 步骤失败都 fail-open：不影响告警主链路 |
| **fail-fast** | 一发现问题立刻失败，别带病跑 | 工作流 steps 后 actions 按序执行 fail-fast：前面失败后面不跑 |
| **爆炸半径** | 见[第 4 节](#4-agent-自愈与安全护栏) | 自愈安全的关键参数 |

---

## 9. 软件设计概念

| 术语 | 大白话 | 在本项目里 |
|---|---|---|
| **异步（Async）** | 发出去先不等着拿结果，过会儿再收 | Agent 任务、工作流执行都走队列异步；webhook 请求不阻塞 |
| **同步（Sync）** | 发出去就死等结果 | 管线主流程（去重、入库）是同步的——快，但占用请求 |
| **队列（Queue）** | 排队的任务清单，一个个被消费 | 内存队列分发 `agent.run` / `workflow.trigger` 两类任务 |
| **消费者（Consumer）** | 从队列取任务并执行的工人 | `queue.Consume` 按任务类型分发到编排器/工作流引擎 |
| **事件总线（Event Bus）** | 发布事件、谁订阅谁接收的「广播台」 | `eventbus`：管线发布事件，审计等订阅；内存实现，可换 Kafka |
| **缓存（Cache）** | 把常用的数据暂存内存，避免反复查库 | 规则缓存、维护窗口缓存（TTL 5s）——热路径不查库 |
| **TTL（存活时间）** | 缓存数据过多久算过期 | 规则/窗口缓存 TTL 5s：过期后自动重新查库 |
| **锁（Lock）** | 同一资源同时只允许一个修改，靠锁保证 | 缓存用 `RWMutex`（读可并发、写互斥）；「锁外查库 + double-check」避免所有请求在锁前排大队 |
| **双检（Double-check）** | 等锁时先看一遍，拿锁后确认没被别家更新过，再决定要不要查库 | 缓存失效瞬间：只让一个人去查库，其他人先用旧值，避免「缓存雪崩」 |
| **状态机** | 见[第 4 节](#4-agent-自愈与安全护栏) | 编排/事故状态流转 |
| **检查点** | 见[第 4 节](#4-agent-自愈与安全护栏) | 编排结果持久化 |
| **租户（Tenant）** | 逻辑上的「客户/项目」隔离单元 | 默认 `default`；所有表带 `tenant_id`；`apikey` 模式按 key 区分租户 |
| **多租户** | 一套系统给多个租户用，数据互相隔离 | 本项目全程按 `tenant_id` 过滤；`noauth` 模式固定 `default` |
| **并发防重（Concurrency Dedup）** | 多个请求同时来，只让一个成功 | `workflow_runs` 唯一索引：并发首达仅一个赢者 |
| **回退/降级（Fallback / Degrade）** | 主路径失败时退到次一级方案 | 批量读失败 → 逐事件处理；批量写失败 → 逐条写。都不丢数据 |
| **幂等** | 见[第 3 节](#3-告警在平台内的处理) | 迁移/回填/种子都幂等 |
| **观察性（Observability）** | 能看清系统内部状态的能力 | `/metrics` 指标 + 审计日志 + 结构化日志 |

---

## 10. AI / LLM

| 术语 | 大白话 | 在本项目里 |
|---|---|---|
| **LLM（大语言模型）** | 能生成自然语言文本的 AI 模型 | 可选增强 RCA 根因描述、AI 聚类摘要（DeepSeek/Ollama 等） |
| **OpenAI 兼容** | 接口格式和 OpenAI 一样，换谁都能接 | `AIOPS_LLMDRIVER=openai-compat`：DeepSeek/vLLM/LiteLLM 都能用 |
| **SSE（流式响应）** | 服务器边生成边推送，不用等全部完成 | `AIOPS_LLMSTREAM=true` 默认流式：前端逐字出摘要 |
| **Prompt（提示词）** | 给 LLM 的指令/问题文本 | 项目内置了 RCA 证据链 prompt、聚类摘要 prompt |
| **确定性（Deterministic）** | 同样的输入永远得到同样的输出（可复现） | 聚类的分组/计数、RCA 证据链都是确定性的；LLM 只是「补充描述」 |
| **fail-open** | 见[第 8 节](#8-可靠性与发布) | LLM 挂了/超时 → 保留确定性结论，不阻塞 |
| **Token** | LLM 计费/计量的基本单位（≈单词/字） | 了解即可；控制 prompt 长度可省 token |

---

## 11. 常用缩写速查表

| 缩写 | 全称 | 意思 |
|---|---|---|
| **AIOps** | AI for IT Operations | 用 AI 做 IT 运维 |
| **SRE** | Site Reliability Engineering | 站点可靠性工程（一种运维理念/岗位） |
| **RCA** | Root Cause Analysis | 根因分析 |
| **QPS / RPS** | Queries/Requests Per Second | 每秒查询/请求数 |
| **P50/P95/P99** | 50th/95th/99th percentile | 第 50/95/99 百分位耗时 |
| **TTL** | Time To Live | 存活时间（缓存多久过期） |
| **CTE** | Common Table Expression | SQL 里的 WITH 临时结果子句 |
| **JSONB** | JSON Binary | PG 的二进制 JSON 类型 |
| **WAL** | Write-Ahead Log | 预写日志 |
| **DSN** | Data Source Name | 数据库连接串 |
| **CEL** | Common Expression Language | 通用表达式语言（规则用） |
| **UI** | User Interface | 用户界面（前端页面） |
| **API** | Application Programming Interface | 程序间调用接口（`/api/v1/...`） |
| **WS** | WebSocket | 双向实时通信协议（本项目用来推前端） |
| **DB** | Database | 数据库 |
| **CI/CD** | Continuous Integration / Deployment | 持续集成/持续部署 |
| **LLM** | Large Language Model | 大语言模型 |
| **SSE** | Server-Sent Events | 服务器单向推送（流式文本） |
| **CRUD** | Create/Read/Update/Delete | 增删改查（接口基本操作） |

---

*遇到文档里没解释的新词，欢迎补充到这里；术语的「项目里对应」部分随代码演进同步更新。*
