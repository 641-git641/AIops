// mock_llm.mjs — 本地调试用 OpenAI 兼容 Chat Completions mock。
// 供 LLM 增强（RCA 根因补充 + AI 聚类摘要）的端到端验证：
//   - 按 user 输入内容区分场景：包含「告警簇」→ 聚类摘要；否则 → RCA 根因分析
//   - 支持 SSE 流式（请求 stream:true 时逐 chunk 输出 delta；否则一次返回完整 JSON）
//   - fail 参数：任何请求都返回 500（模拟 LLM 故障，验证 fail-open 降级）
// 用法：AIOPS_LLMBASEURL=http://localhost:9998 AIOPS_LLMAPIKEY=dummy AIOPS_LLMMODEL=mock
//       node deploy/mock_llm.mjs [port=9998] [fail]
import http from 'node:http'

const port = Number(process.argv[2] || 9998)
const fail = process.argv[3] === 'fail'

const RCA_JSON =
  '{"rootCause":"order 数据库连接池耗尽（mock LLM）","rationale":"结合近期变更 v1.2.3 与告警摘要推断","suggestedActions":["重启连接池","回滚 v1.2.3","扩容 db 副本"]}'
const CLUSTER_JSON = '{"summary":"order 服务并发流量突增，连接池与实例负载告警集中爆发，疑似数据库连接池耗尽或近期变更引入回归，建议优先扩容 db 副本并核查 v1.2.3 变更（mock LLM）。"}'

http
  .createServer((req, res) => {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      if (fail) {
        res.writeHead(500, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ error: { message: 'mock llm down' } }))
        return
      }
      let stream = false
      let user = ''
      try {
        const reqBody = JSON.parse(body)
        stream = !!reqBody?.stream
        user = (reqBody?.messages?.[1]?.content || '').slice(0, 300)
      } catch {}
      const cluster = user.includes('告警簇')
      const content = '```json\n' + (cluster ? CLUSTER_JSON : RCA_JSON) + '\n```'
      console.log(`\n[${new Date().toISOString()}] ${req.method} ${req.url} stream=${stream} type=${cluster ? 'cluster' : 'rca'}`)
      if (user) console.log(`  user: ${user}`)

      if (!stream) {
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ choices: [{ message: { content } }] }))
        return
      }

      // SSE 流式：把完整内容切成多个 delta 逐行输出（模拟真实模型逐 token）
      res.writeHead(200, {
        'Content-Type': 'text/event-stream',
        'Cache-Control': 'no-cache',
        Connection: 'keep-alive',
      })
      const chunkSize = 16
      for (let i = 0; i < content.length; i += chunkSize) {
        const delta = content.slice(i, i + chunkSize)
        res.write(`data: ${JSON.stringify({ choices: [{ delta: { content: delta } }] })}\n\n`)
      }
      res.write(`data: ${JSON.stringify({ choices: [{ delta: { content: '' } }] })}\n\n`)
      res.write('data: [DONE]\n\n')
      res.end()
    })
  })
  .listen(port, () => console.log(`mock llm listening on :${port}`))
