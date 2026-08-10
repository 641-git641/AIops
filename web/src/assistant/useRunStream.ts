// assistant/useRunStream.ts：run 事件 SSE 流 hook。
// 用 fetch + ReadableStream 手工解析 SSE（EventSource 无法携带 X-API-Key 头，apikey 模式必 401）；
// 断线重连带 lastSequence（服务端从 DB 快照续传，不丢事件）；AbortController 支持停止/切会话。
// 按 AGENT_CHAT_PLAN §11.3：delta 进局部 reducer，不写全局 query cache。

import { useEffect, useReducer, useRef } from 'react'
import { getAPIKey } from '../api'
import { streamReducer, initialState } from './streamReducer'
import type { SSEEvent } from './types'

export interface UseRunStreamOptions {
  runId: string | undefined
  enabled?: boolean
  onTerminal?: (status: string) => void // run 终态回调（消息并入稳定列表）
}

export function useRunStream({ runId, enabled = true, onTerminal }: UseRunStreamOptions) {
  const [state, dispatch] = useReducer(streamReducer, '', () => initialState(''))
  const aborter = useRef<AbortController | null>(null)
  const onTerminalRef = useRef(onTerminal)
  onTerminalRef.current = onTerminal

  useEffect(() => {
    if (!runId || !enabled) {
      dispatch({ type: 'reset', runId: runId ?? '' })
      return
    }
    dispatch({ type: 'reset', runId })

    const ac = new AbortController()
    aborter.current = ac
    let lastSeq = 0

    // connect 建立一次 SSE 连接；返回 true 表示 run 已终态（无需重连）。
    const connect = async (): Promise<boolean> => {
      const headers: Record<string, string> = {}
      const key = getAPIKey()
      if (key) headers['X-API-Key'] = key
      const res = await fetch(`/api/v1/assistant/runs/${runId}/events?lastSequence=${lastSeq}`, {
        headers,
        signal: ac.signal,
      })
      if (!res.ok || !res.body) {
        if (res.status === 404) {
          dispatch({ type: 'finish', status: 'failed', error: 'run 不存在' })
          return true
        }
        return false // 服务端异常 → 重连
      }
      const reader = res.body.getReader()
      const decoder = new TextDecoder()
      let buf = ''
      let terminal = false
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })
        let idx: number
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const frame = buf.slice(0, idx)
          buf = buf.slice(idx + 2)
          for (const line of frame.split('\n')) {
            const t = line.trim()
            if (!t.startsWith('data:')) continue
            const payload = t.slice(5).trim()
            if (payload === '[DONE]') {
              terminal = true
              break
            }
            if (!payload) continue
            try {
              const ev = JSON.parse(payload) as SSEEvent
              if (ev.sequence <= lastSeq) continue // 快照/实时重叠去重
              dispatch({ type: 'event', event: ev })
              lastSeq = ev.sequence
              if (ev.type === 'run.completed' || ev.type === 'run.failed' || ev.type === 'run.cancelled') {
                terminal = true
                onTerminalRef.current?.(ev.type)
              }
            } catch {
              // 非 JSON 帧跳过（心跳等容错）
            }
          }
          if (terminal) break
        }
        if (terminal) break
      }
      return terminal // 流结束但未终态（断线）→ false → 重连
    }

    const loop = async () => {
      while (!ac.signal.aborted) {
        try {
          const terminal = await connect()
          if (terminal || ac.signal.aborted) return
        } catch (err) {
          if ((err as Error).name === 'AbortError') return
        }
        await new Promise((r) => setTimeout(r, 1500)) // 断线重连间隔
      }
    }
    void loop()
    return () => ac.abort()
  }, [runId, enabled])

  return { state }
}
