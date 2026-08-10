import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api } from './api'
import type { WSEvent } from './api'

// useRealtime 建立 WS 连接并监听后端推送，命中业务事件后失效对应查询缓存触发刷新。
// apikey 模式：先经 REST 换取一次性短期 ticket（30s TTL），握手 ?ticket= 校验——长期密钥
// 永不进入 URL；断线 3s 重连，每次重连重新取 ticket（ticket 单次使用 + 服务端内存态）。
export function useRealtime() {
  const qc = useQueryClient()

  useEffect(() => {
    let ws: WebSocket | null = null
    let retry: ReturnType<typeof setTimeout> | null = null
    let closed = false

    const connect = async () => {
      if (closed) return
      let url = `ws://${location.host}/api/v1/ws`
      try {
        const t = await api.wsTicket()
        url += `?ticket=${encodeURIComponent(t.ticket)}`
      } catch {
        // 拿不到 ticket（后端未就绪等）→ 直连兜底；401 场景 req 已自动跳解锁页
      }
      ws = new WebSocket(url)

      ws.onmessage = (ev) => {
        let e: WSEvent
        try {
          e = JSON.parse(ev.data) as WSEvent
        } catch {
          return
        }
        // 告警/事故/编排状态变化 → 刷新对应列表与详情 + 总览卡片
        if (e.type.startsWith('alert.')) {
          qc.invalidateQueries({ queryKey: ['alerts'] })
          qc.invalidateQueries({ queryKey: ['overview'] })
        } else if (e.type.startsWith('incident.')) {
          qc.invalidateQueries({ queryKey: ['incidents'] })
          qc.invalidateQueries({ queryKey: ['overview'] })
        }
      }

      ws.onclose = () => {
        if (!closed) retry = setTimeout(connect, 3000)
      }
    }

    connect()
    return () => {
      closed = true
      if (retry) clearTimeout(retry)
      ws?.close()
    }
  }, [qc])
}
