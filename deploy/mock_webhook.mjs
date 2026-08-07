// mock_webhook.mjs — 本地调试用通用 webhook：打印收到的 POST body 并返回 200。
// 配合 notifier 的 http 渠道验证通知链路：
//   AIOPS_NOTIFIERCHANNELS=http AIOPS_NOTIFIERHTTP=http://localhost:9999/webhook
// 用法：node deploy/mock_webhook.mjs [port=9999]
import http from 'node:http'

const port = Number(process.argv[2] || 9999)

http
  .createServer((req, res) => {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      console.log(
        `\n[${new Date().toISOString()}] ${req.method} ${req.url} ${req.headers['content-type'] || ''}`,
      )
      try {
        console.log(JSON.stringify(JSON.parse(body), null, 2))
      } catch {
        console.log(body)
      }
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end('{"ok":true}')
    })
  })
  .listen(port, () => console.log(`mock webhook listening on :${port}`))
