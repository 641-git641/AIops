// mock_echo.mjs — 回显收到的请求（method/headers/body），供工作流 http step 验证 method/headers/auth。
// /slow 路径延迟 ~4s 返回（工作流慢步骤，测并发执行不阻塞队列）。
// 用法：node deploy/mock_echo.mjs [port=9996]
import http from 'node:http'

const port = Number(process.argv[2] || 9996)

http
  .createServer((req, res) => {
    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
      const delay = req.url === '/slow' ? 4000 : 0
      setTimeout(() => {
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(
          JSON.stringify({
            method: req.method,
            url: req.url,
            auth: req.headers.authorization || '',
            trace: req.headers['x-trace'] || '',
            body,
          }),
        )
      }, delay)
    })
  })
  .listen(port, () => console.log(`mock echo listening on :${port}`))
