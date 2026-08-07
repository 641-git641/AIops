import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import type { WSEvent } from './api'

// useRealtime 建立 WS 连接并监听后端推送，命中业务事件后失效对应查询缓存触发刷新。
// 自动重连（后端 30s ping 保活，断线重连避免 UI 呆滞）。
export function useRealtime() {
  const qc = useQueryClient()

  useEffect(() => {
    let ws: WebSocket | null = null
    let retry: ReturnType<typeof setTimeout> | null = null
    let closed = false

    const connect = () => {
      if (closed) return
      ws = new WebSocket(`ws://${location.host}/api/v1/ws`)

      ws.onmessage = (ev) => {
        let e: WSEvent
        try {
          e = JSON.parse(ev.data) as WSEvent
        } catch {
          return
        }
        // 告警/事故/编排状态变化 → 刷新对应列表与详情
        if (e.type.startsWith('alert.')) {
          qc.invalidateQueries({ queryKey: ['alerts'] })
        } else if (e.type.startsWith('incident.')) {
          qc.invalidateQueries({ queryKey: ['incidents'] })
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
