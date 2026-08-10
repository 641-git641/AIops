// mock_llm.mjs — 本地调试用 OpenAI 兼容 Chat Completions mock。
// 供 LLM 增强（RCA 根因补充 + AI 聚类摘要）与对话式 Agent（AGENT_CHAT_PLAN Phase 1）的端到端验证：
//   - 对话 Agent：支持 tool_calls 多步编排（按最后 user 关键词分发工具序列；
//     role=tool 结果回喂后按工具名推进下一轮；最终回答引用工具结果）
//   - RCA/聚类：按 user 内容区分场景（「告警簇」→ 聚类摘要；否则 → RCA 根因分析）
//   - SSE 流式：stream:true 时逐 chunk 输出 delta（可触发 split-args/duplicate/stall 场景）
//   - fail 参数：任何请求都返回 500（模拟 LLM 故障，验证 fail-open 降级）
// 用法：AIOPS_LLMBASEURL=http://localhost:9998 AIOPS_LLMAPIKEY=dummy AIOPS_LLMMODEL=mock
//       node deploy/mock_llm.mjs [port=9998] [fail]
// 场景参数（?scenario=）：multi-tool（一轮 2 个并行调用）| split-args（跨 chunk 碎片）
//   | stall（流中段挂起）| duplicate（同 delta 发两遍）| error（工具轮后 500）| retry（首轮 500 后正常）
import http from 'node:http'
import { URL } from 'node:url'

const port = Number(process.argv[2] || 9998)
const failMode = process.argv[3] === 'fail'

const RCA_JSON =
  '{"rootCause":"order 数据库连接池耗尽（mock LLM）","rationale":"结合近期变更 v1.2.3 与告警摘要推断","suggestedActions":["重启连接池","回滚 v1.2.3","扩容 db 副本"]}'
const CLUSTER_JSON = '{"summary":"order 服务并发流量突增，连接池与实例负载告警集中爆发，疑似数据库连接池耗尽或近期变更引入回归，建议优先扩容 db 副本并核查 v1.2.3 变更（mock LLM）。"}'

// ---- 工具结果（模拟领域只读工具返回；与 internal/assistant/tools_read.go 形状对齐） ----

function toolResult(name, args = {}) {
  switch (name) {
    case 'list_alerts':
      return JSON.stringify({
        count: 3,
        alerts: [
          { name: 'pay-down', severity: 'critical', status: 'firing', service: 'pay', startedAt: '2026-08-10T10:00:00Z' },
          { name: 'pay-slow', severity: 'critical', status: 'firing', service: 'pay', startedAt: '2026-08-10T10:05:00Z' },
          { name: 'db-conn-exhausted', severity: 'high', status: 'firing', service: 'db', startedAt: '2026-08-10T10:06:00Z' },
        ],
      })
    case 'list_incidents':
      return JSON.stringify({
        count: 2,
        incidents: [
          { id: 'inc-1001', name: 'pay 服务不可用', status: 'open', severity: 'critical', summary: '支付链路超时' },
          { id: 'inc-1002', name: 'db 连接池耗尽', status: 'open', severity: 'high', summary: '数据库连接数打满' },
        ],
      })
    case 'get_incident':
      return JSON.stringify({
        id: args.incidentId || 'inc-1001', name: 'pay 服务不可用', status: 'open', severity: 'critical',
        summary: '支付链路超时', alertCount: 3,
        rca: { rootCause: 'db 连接池耗尽', confidence: 0.6, evidence: ['连接池占用 98%'] },
        heal: { status: 'awaiting_approval', action: '扩容 db 副本', level: 'L1', blastRadius: 0.35 },
      })
    case 'get_topology_impact':
      return JSON.stringify({
        service: args.service || 'pay',
        depth: 2,
        impacted: [
          { service: 'api-gateway', depth: 1, path: ['pay', 'api-gateway'] },
          { service: 'web-frontend', depth: 2, path: ['pay', 'api-gateway', 'web-frontend'] },
        ],
      })
    case 'get_platform_overview':
      return JSON.stringify({
        window: '24h',
        counts: { activeAlerts: 3, activeIncidents: 2, runningTasks: 1, knowledgeDocs: 5 },
        system: { dbOk: true, queueDepth: 0, ragBacklog: 0 },
      })
    case 'search_knowledge':
      return JSON.stringify({
        query: args.query || '',
        count: 2,
        degradedReason: 'vector_disabled',
        results: [
          { documentId: 'doc-1', sourceType: 'incident', sourceId: 'inc-1001', title: 'pay 服务连接池耗尽',
            trustLevel: 'T3', score: 0.91, reasons: ['keyword', 'trust:T3'],
            content: '确认根因：db 连接池上限与并发配置不匹配；处置：回滚配置并重启连接池；验证：错误率恢复（2026-07-01 处置记录）' },
          { documentId: 'doc-2', sourceType: 'playbook', sourceId: 'pb-rollback', title: 'rollback 回滚剧本',
            trustLevel: 'T2', score: 0.72, reasons: ['exact:service'],
            content: 'kubectl rollout undo deployment/{{.service}}，L1 级别，爆炸半径 0.35' },
        ],
      })
    case 'get_similar_incidents':
      return JSON.stringify({
        query: 'similar-to:inc-1001',
        count: 1,
        results: [
          { documentId: 'doc-3', sourceType: 'incident', sourceId: 'inc-0999', title: 'pay 数据库连接池耗尽（上月）',
            trustLevel: 'T3', score: 0.88, reasons: ['exact:service', 'vector:0.82'],
            content: '现象：pay 接口超时 ECONNREFUSED；根因：连接池打满；处置：扩容 db 副本并回滚 v1.2.3；验证：P99 恢复' },
        ],
      })
    case 'list_playbooks':
      return JSON.stringify({
        count: 2,
        playbooks: [
          { name: 'rollback', level: 'L1', commandTemplate: 'kubectl rollout undo deployment/{{.service}}', blastRadiusFactor: 0.35, dryRunFirst: true },
          { name: 'scale_up', level: 'L1', commandTemplate: 'kubectl scale deployment/{{.service}} --replicas={{.replicas}}', blastRadiusFactor: 0.2, dryRunFirst: true },
        ],
      })
    default:
      return JSON.stringify({ error: `unknown tool ${name}` })
  }
}

// ---- 场景分发：无工具结果时按最后 user 关键词返回首批 tool_calls ----

function firstToolCalls(user, scenario) {
  if (scenario === 'multi-tool') {
    return [
      { id: 'call-m1', type: 'function', function: { name: 'list_alerts', arguments: '{"limit":5}' } },
      { id: 'call-m2', type: 'function', function: { name: 'get_platform_overview', arguments: '{}' } },
    ]
  }
  // 注意顺序：更具体的关键词（相似/知识/剧本）必须排在宽泛词（事故/告警）之前，
  // 否则「相似历史事故」会被「事故」分支拦截。
  if (user.includes('相似')) {
    return [{ id: 'call-s1', type: 'function', function: { name: 'get_similar_incidents', arguments: '{"incident_id":"inc-1001","limit":5}' } }]
  }
  if (user.includes('知识') || user.includes('历史事故') || user.includes('runbook')) {
    return [{ id: 'call-k1', type: 'function', function: { name: 'search_knowledge', arguments: '{"query":"pay 连接池耗尽处置方法","limit":5}' } }]
  }
  if (user.includes('剧本') || user.includes('playbook')) {
    return [{ id: 'call-pb1', type: 'function', function: { name: 'list_playbooks', arguments: '{"limit":10}' } }]
  }
  if (user.includes('告警')) {
    return [{ id: 'call-a1', type: 'function', function: { name: 'list_alerts', arguments: '{"severity":"critical","limit":10}' } }]
  }
  if (user.includes('事故')) {
    return [{ id: 'call-i1', type: 'function', function: { name: 'list_incidents', arguments: '{"status":"open"}' } }]
  }
  if (user.includes('拓扑')) {
    return [{ id: 'call-t1', type: 'function', function: { name: 'get_topology_impact', arguments: '{"service":"pay","depth":2}' } }]
  }
  if (user.includes('总览') || user.includes('状态')) {
    return [{ id: 'call-o1', type: 'function', function: { name: 'get_platform_overview', arguments: '{}' } }]
  }
  if (user.includes('指标')) {
    return [{ id: 'call-p1', type: 'function', function: { name: 'query_metrics', arguments: '{"query":"container_cpu_usage_seconds_total{service=\\"pay\\"}"}' } }]
  }
  if (user.includes('日志')) {
    return [{ id: 'call-l1', type: 'function', function: { name: 'search_logs', arguments: '{"service":"pay"}' } }]
  }
  return null // 纯问答
}

// 工具结果回喂后推进下一轮：list_alerts → get_incident → 最终回答；其余工具 → 直接最终回答
function nextToolCalls(lastToolName, lastToolArgs) {
  if (lastToolName === 'list_incidents') {
    return [{ id: 'call-i2', type: 'function', function: { name: 'get_incident', arguments: '{"incidentId":"inc-1001"}' } }]
  }
  return null // 生成最终回答
}

// 最终回答（引用工具结果的事实，供「事实一致」验收）
function finalAnswer(lastToolName) {
  switch (lastToolName) {
    case 'list_alerts':
      return '共 3 条活跃告警：pay-down、pay-slow（均为 critical，服务 pay）与 db-conn-exhausted（high，服务 db）。关键风险在 pay 服务链路，建议优先排查支付接口与数据库连接。'
    case 'list_incidents':
      return '当前 2 个未解决事故：inc-1001 pay 服务不可用（critical）与 inc-1002 db 连接池耗尽（high）。'
    case 'get_incident':
      return '事故 inc-1001（pay 服务不可用）：确定性 RCA 认为根因是 db 连接池耗尽（置信度 0.6），当前有 1 个 L1 自愈动作「扩容 db 副本」等待审批，爆炸半径 0.35。'
    case 'get_topology_impact':
      return 'pay 的下游依赖共 2 层：api-gateway（1 层）与 web-frontend（2 层）。pay 故障会直接波及 api-gateway。'
    case 'get_platform_overview':
      return '平台当前状态：3 条活跃告警、2 个活跃事故、1 个执行中任务、知识库 5 条。数据库正常，队列无积压。'
    case 'search_knowledge':
      return '知识库命中 2 条：① 已解决事故「pay 服务连接池耗尽」（T3 已验证）：根因是连接池上限与并发配置不匹配，处置为回滚配置并重启连接池；② 剧本「rollback」（T2）：kubectl rollout undo，L1 级别，爆炸半径 0.35。向量检索降级（vector_disabled），以上为关键词路结果。'
    case 'get_similar_incidents':
      return '检索到 1 条相似历史事故「pay 数据库连接池耗尽（上月）」（T3）：现象为 pay 接口超时 ECONNREFUSED，处置为扩容 db 副本并回滚 v1.2.3，验证后 P99 恢复。与当前事故（inc-1001）症状吻合。'
    case 'list_playbooks':
      return '可用剧本 2 个：rollback（L1，爆炸半径 0.35，强制 dry-run）与 scale_up（L1，爆炸半径 0.2，强制 dry-run）。均为低风险动作，须 dry-run 先行。'
    case 'query_metrics':
      return 'pay 服务 CPU 指标近 30 分钟从 0.5 上升至 1.6（趋势 up），存在明显资源增长，需结合日志确认是否异常。'
    case 'search_logs':
      return 'pay 服务日志含 1 条 ERROR（db connection pool exhausted），另有 1 条请求超时重试。错误集中在连接池方向，与指标上升吻合。'
    default:
      return '已获取工具结果。当前平台运行状态总体正常，无需立即干预。'
  }
}

http
  .createServer((req, res) => {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      if (failMode) {
        res.writeHead(500, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ error: { message: 'mock llm down' } }))
        return
      }
      const scenario = new URL(req.url, 'http://x').searchParams.get('scenario') || ''

      // retry 场景：首轮 500，之后正常（按进程内计数）
      if (scenario === 'retry' && !global.__retried) {
        global.__retried = true
        res.writeHead(500, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ error: { message: 'mock llm transient failure' } }))
        return
      }

      let stream = false
      let messages = []
      try {
        const reqBody = JSON.parse(body)
        stream = !!reqBody?.stream
        messages = reqBody?.messages || []
      } catch {}

      const user = (messages.find((m) => m.role === 'user')?.content || '').slice(0, 300)
      const last = messages[messages.length - 1]
      const lastTool = last?.role === 'tool' ? { name: last.name, args: safeParse(last.content) } : null
      const cluster = user.includes('告警簇')

      // 决定本轮响应：工具结果回喂 → 推进编排；否则按场景分发
      let toolCalls = null
      let content = null
      if (lastTool) {
        toolCalls = nextToolCalls(lastTool.name, lastTool.args)
        content = toolCalls ? null : finalAnswer(lastTool.name)
      } else if (scenario === 'error') {
        res.writeHead(500, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ error: { message: 'mock llm error scenario' } }))
        return
      } else {
        toolCalls = firstToolCalls(user, scenario)
        if (!toolCalls) {
          content = '```json\n' + (cluster ? CLUSTER_JSON : RCA_JSON) + '\n```'
        }
      }

      console.log(`\n[${new Date().toISOString()}] ${req.method} ${req.url} stream=${stream} scenario=${scenario || '-'} tools=${toolCalls ? toolCalls.length : 0} lastTool=${lastTool ? lastTool.name : '-'}`)
      if (user) console.log(`  user: ${user}`)

      const finishReason = toolCalls ? 'tool_calls' : 'stop'
      const usage = { prompt_tokens: 20, completion_tokens: 10, total_tokens: 30 }

      if (!stream) {
        const message = toolCalls ? { role: 'assistant', content: null, tool_calls: toolCalls } : { role: 'assistant', content }
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ choices: [{ message, finish_reason: finishReason }], usage }))
        return
      }

      // SSE 流式：tool_calls 或内容分 chunk 输出
      res.writeHead(200, {
        'Content-Type': 'text/event-stream',
        'Cache-Control': 'no-cache',
        Connection: 'keep-alive',
      })
      const flush = (obj) => res.write(`data: ${JSON.stringify(obj)}\n\n`)
      if (toolCalls) {
        if (scenario === 'split-args') {
          // arguments 跨 3 chunk 碎片
          toolCalls.forEach((tc, i) => {
            const a = tc.function.arguments
            const third = Math.ceil(a.length / 3)
            for (let p = 0; p < 3; p++) {
              flush({ choices: [{ delta: { tool_calls: [{ index: i, id: p === 0 ? tc.id : undefined, function: { name: p === 0 ? tc.function.name : undefined, arguments: a.slice(p * third, (p + 1) * third) } }] } }] })
            }
          })
        } else {
          toolCalls.forEach((tc, i) => {
            flush({ choices: [{ delta: { tool_calls: [{ index: i, id: tc.id, function: { name: tc.function.name, arguments: tc.function.arguments } }] } }] })
          })
        }
        flush({ choices: [{ delta: {}, finish_reason: 'tool_calls' }] })
      } else {
        const chunkSize = 16
        for (let i = 0; i < content.length; i += chunkSize) {
          const delta = content.slice(i, i + chunkSize)
          flush({ choices: [{ delta: { content: delta } }] })
          if (scenario === 'duplicate') flush({ choices: [{ delta: { content: delta } }] }) // 同 delta 发两遍
        }
        flush({ choices: [{ delta: {} }] })
        flush({ choices: [{ delta: {}, finish_reason: 'stop' }] })
      }
      flush({ usage })
      res.write('data: [DONE]\n\n')
      if (scenario === 'stall') {
        // 流中段挂起：不 end（客户端超时/取消路径）
        console.log('  [stall] 挂起连接，模拟流中断')
        setTimeout(() => res.destroy(), 15000)
        return
      }
      res.end()
    })
  })
  .listen(port, () => console.log(`mock llm listening on :${port}`))

function safeParse(s) {
  try {
    return JSON.parse(s)
  } catch {
    return {}
  }
}
