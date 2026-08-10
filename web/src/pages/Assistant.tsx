import { useCallback, useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Alert, Tag, message } from 'antd'
import { api } from '../api'
import type { AssistantRunDTO, AssistantThread } from '../api'
import { useRunStream } from '../assistant/useRunStream'
import type { AssistantMessage } from '../assistant/types'
import ThreadList from '../components/assistant/ThreadList'
import ChatTimeline from '../components/assistant/ChatTimeline'
import Composer from '../components/assistant/Composer'
import RunStatus from '../components/assistant/RunStatus'

// Assistant 对话式 Agent 聊天页（AGENT_CHAT_PLAN §11）：两栏（会话列表 + 对话区）。
// 流式增量走局部 reducer（useRunStream），REST 快照走 react-query；发送带 Idempotency-Key 幂等。
// ?incident=<id>：从事故详情进入——自动新建绑定该事故的会话并询问「分析这个事故」。
export default function Assistant() {
  const qc = useQueryClient()
  const [msg, contextHolder] = message.useMessage()
  const [activeThread, setActiveThread] = useState<string>()
  const [stableMessages, setStableMessages] = useState<AssistantMessage[]>([])
  const [activeRun, setActiveRun] = useState<AssistantRunDTO>()
  const [forbidden, setForbidden] = useState(false)
  const boundInit = useRef(false)

  // 会话列表
  const { data: threadsData } = useQuery({
    queryKey: ['assistant-threads'],
    queryFn: () => api.assistantThreads(),
    retry: false,
  })

  // 会话历史消息
  const { data: messagesData } = useQuery({
    queryKey: ['assistant-thread', activeThread],
    queryFn: () => (activeThread ? api.assistantMessages(activeThread) : Promise.resolve({ items: [] })),
    enabled: !!activeThread,
  })
  useEffect(() => {
    setStableMessages(messagesData?.items ?? [])
  }, [messagesData])

  // 线程动作请求（页面刷新/切会话恢复确认卡；决策后失效重拉）
  const { data: actionsData } = useQuery({
    queryKey: ['assistant-thread-actions', activeThread],
    queryFn: () => (activeThread ? api.assistantThreadActions(activeThread) : Promise.resolve({ items: [] })),
    enabled: !!activeThread,
  })
  const invalidateActions = useCallback(() => {
    if (activeThread) {
      void qc.invalidateQueries({ queryKey: ['assistant-thread-actions', activeThread] })
    }
  }, [activeThread, qc])

  // 流式（当前 run）
  const { state: stream } = useRunStream({
    runId: activeRun?.id,
    onTerminal: () => {
      // 终态后刷新稳定消息（assistant 回复落库）与动作请求
      if (activeThread) {
        void qc.invalidateQueries({ queryKey: ['assistant-thread', activeThread] })
        void qc.invalidateQueries({ queryKey: ['assistant-thread-actions', activeThread] })
      }
      setActiveRun(undefined)
    },
  })

  // 发送消息（幂等键：时间戳+随机）
  const send = useMutation({
    mutationFn: async ({ threadId, content }: { threadId: string; content: string }) => {
      const idemKey = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`
      const res = await api.assistantSendMessage(threadId, content, idemKey)
      return res as { runId: string }
    },
    onSuccess: (res, vars) => {
      const now = new Date().toISOString()
      setActiveRun({
        id: res.runId, threadId: vars.threadId, status: 'queued', model: '', stepCount: 0,
        createdAt: now, updatedAt: now,
      })
      void qc.invalidateQueries({ queryKey: ['assistant-thread', vars.threadId] })
    },
    onError: (err: Error) => {
      if (err.message.includes('404')) setForbidden(true)
      msg.error(`发送失败：${err.message}`)
    },
  })

  const stop = useMutation({
    mutationFn: (runId: string) => api.assistantCancelRun(runId),
  })

  const createThread = useMutation({
    mutationFn: (ctx?: { contextType: string; contextId: string }) => api.assistantCreateThread('', ctx),
    onSuccess: (dto) => {
      const th = dto as AssistantThread
      setActiveThread(th.id)
      void qc.invalidateQueries({ queryKey: ['assistant-threads'] })
    },
    onError: (err: Error) => {
      if (err.message.includes('404')) setForbidden(true)
      msg.error(`新建会话失败：${err.message}`)
    },
  })

  const deleteThread = useMutation({
    mutationFn: (id: string) => api.assistantDeleteThread(id),
    onSuccess: (_dto, id) => {
      if (activeThread === id) {
        setActiveThread(undefined)
        setStableMessages([])
      }
      void qc.invalidateQueries({ queryKey: ['assistant-threads'] })
    },
  })

  // ?incident=<id>：自动创建绑定事故的会话并询问（只执行一次；防 StrictMode 双跑）
  useEffect(() => {
    if (boundInit.current) return
    const incidentId = new URLSearchParams(window.location.search).get('incident')
    if (!incidentId) return
    boundInit.current = true
    createThread.mutate(
      { contextType: 'incident', contextId: incidentId },
      {
        onSuccess: (dto) => {
          const th = dto as AssistantThread
          send.mutate({ threadId: th.id, content: '分析这个事故' })
        },
      },
    )
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const onSend = useCallback(
    (content: string) => {
      if (!activeThread) {
        msg.warning('请先新建或选择一个会话')
        return
      }
      send.mutate({ threadId: activeThread, content })
    },
    [activeThread, send, msg],
  )

  // 当前会话绑定对象（上下文标签）
  const activeContext = threadsData?.items.find((t) => t.id === activeThread && t.contextType === 'incident')

  return (
    <div style={{ display: 'flex', gap: 12, height: 'calc(100vh - 140px)', minHeight: 480 }}>
      {forbidden && (
        <Alert
          type="warning" showIcon style={{ position: 'fixed', top: 64, right: 24, zIndex: 100 }}
          message="AI 助手未启用"
          description="后端需 AIOPS_ASSISTANTENABLED=true（模型配置见模型配置页 Assistant Chat）"
        />
      )}
      {/* 左栏：会话列表 */}
      <div style={{ width: 240, border: '1px solid #f0f0f0', borderRadius: 10, padding: 10, background: '#fff', flexShrink: 0 }}>
        <ThreadList
          threads={threadsData?.items ?? []}
          activeId={activeThread}
          onSelect={setActiveThread}
          onCreate={() => createThread.mutate(undefined)}
          onDelete={(id) => deleteThread.mutate(id)}
        />
      </div>
      {/* 右栏：对话区 */}
      <div style={{ flex: 1, border: '1px solid #f0f0f0', borderRadius: 10, background: '#fff', display: 'flex', flexDirection: 'column', minWidth: 0 }}>
        <div style={{ padding: '12px 16px', borderBottom: '1px solid #f0f0f0', fontWeight: 600, display: 'flex', alignItems: 'center', gap: 8 }}>
          AI 助手
          {activeContext && (
            <Tag color="blue" style={{ marginInlineEnd: 0 }}>
              绑定事故：{activeContext.title}
            </Tag>
          )}
          {activeRun && <RunStatus run={activeRun} />}
        </div>
        <ChatTimeline
          messages={stableMessages}
          stream={stream}
          actions={actionsData?.items ?? []}
          onActionsChanged={invalidateActions}
        />
        <Composer
          sending={!!activeRun || send.isPending}
          onSend={onSend}
          onStop={() => activeRun && stop.mutate(activeRun.id)}
        />
      </div>
      {contextHolder}
    </div>
  )
}
