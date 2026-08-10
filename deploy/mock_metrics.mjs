// mock_metrics.mjs — 本地调试用 Prometheus + Loki 数据源 mock（数据源证据 e2e）。
// 同时 mock Prometheus /api/v1/query_range 与 Loki /loki/api/v1/query_range：
//   - Prometheus：按 query 含 service 标签返回确定性指标（0.5 → 0.8 → 1.6 上升趋势，触发「跳变」证据）
//   - Loki：按 query 的 service 返回确定性日志（含 ERROR 行与脱敏样例行）
// 用法：AIOPS_EVIDENCEENABLED=true AIOPS_PROMETHEUSURL=http://localhost:9977 AIOPS_LOKIURL=http://localhost:9977
//       node deploy/mock_metrics.mjs [port=9977]
import http from 'node:http'

const port = Number(process.argv[2] || 9977)

// 当前时间附近的确定性时间戳（秒/纳秒）
const nowSec = Math.floor(Date.now() / 1000)
const nowNs = nowSec * 1e9

http
  .createServer((req, res) => {
    const url = new URL(req.url, 'http://x')
    const q = url.searchParams.get('query') || ''
    const service = (q.match(/service="([^"]+)"/) || [])[1] || 'unknown'
    console.log(`[${new Date().toISOString()}] ${req.url} service=${service}`)
    res.setHeader('Content-Type', 'application/json')

    if (url.pathname === '/api/v1/query_range') {
      // 上升趋势：0.5 → 0.8 → 1.6（末值 > 2×首值 → RCA「跳变」证据命中）
      res.end(JSON.stringify({
        status: 'success',
        data: {
          resultType: 'matrix',
          result: [
            {
              metric: { __name__: 'container_cpu_usage_seconds_total', service },
              values: [
                [nowSec - 1800, '0.5'],
                [nowSec - 1200, '0.8'],
                [nowSec - 600, '1.2'],
                [nowSec, '1.6'],
              ],
            },
          ],
        },
      }))
      return
    }
    if (url.pathname === '/loki/api/v1/query_range') {
      res.end(JSON.stringify({
        status: 'success',
        data: {
          resultType: 'streams',
          result: [
            {
              stream: { service },
              values: [
                [`${nowNs - 120e9}`, `ERROR: db connection pool exhausted for service=${service}`],
                [`${nowNs - 90e9}`, 'request timeout after 5s, retry=2'],
                [`${nowNs - 60e9}`, `INFO: health check ok password=shouldberedacted123`],
              ],
            },
          ],
        },
      }))
      return
    }
    res.statusCode = 404
    res.end(JSON.stringify({ error: 'mock_metrics: bad path ' + url.pathname }))
  })
  .listen(port, () => console.log(`mock metrics (prometheus+loki) listening on :${port}`))
