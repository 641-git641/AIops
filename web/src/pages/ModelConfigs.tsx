import { useEffect, useState } from 'react'
import { useQuery, useMutation } from '@tanstack/react-query'
import { Table, Button, Space, Card, Switch, Input, InputNumber, Alert, Typography, message } from 'antd'
import { api } from '../api'

// 支持的用途（Chat LLM 与 Assistant Chat；Embedding 不走 DB——由 AIOPS_EMBEDDING* env 直接配置）
const MODELS: { key: string; label: string; hint: string; placeholder: string }[] = [
  {
    key: 'chat', label: 'Chat LLM', hint: 'RCA 根因补充 + AI 聚类摘要（OpenAI 兼容）',
    placeholder: 'https://api.deepseek.com',
  },
  {
    key: 'assistant_chat', label: 'Assistant Chat', hint: '对话 Agent 生成模型（支持工具调用；可指向本地 mock）',
    placeholder: 'http://localhost:9998',
  },
]

interface EditState {
  enabled: boolean
  baseURL: string
  apiKey: string
  model: string
  timeoutSec: number
  stream: boolean
}

const defaultEdit = (): EditState => ({
  enabled: false, baseURL: '', apiKey: '', model: '', timeoutSec: 15, stream: true,
})

export default function ModelConfigs() {
  const [edits, setEdits] = useState<Record<string, EditState>>({})
  const [msg, contextHolder] = message.useMessage()
  const [forbidden, setForbidden] = useState(false)

  const { data, isLoading, error } = useQuery({
    queryKey: ['model-configs'],
    queryFn: () => api.modelConfigs(),
    retry: false,
  })

  // 列表 403（管理开关未开）时顶部提示
  useEffect(() => {
    if (error?.message.includes('403')) setForbidden(true)
  }, [error])

  // 首次加载（或刷新后）把 DB 配置铺进本地编辑态（apiKey 编辑框恒空，以 apiKeySet 提示）
  useEffect(() => {
    if (!data) return
    const next: Record<string, EditState> = {}
    for (const m of MODELS) next[m.key] = defaultEdit()
    for (const item of data.items) {
      next[item.key] = {
        enabled: item.enabled, baseURL: item.baseURL, apiKey: '', model: item.model,
        timeoutSec: item.timeoutSec || 15, stream: item.stream,
      }
    }
    setEdits(next)
  }, [data])

  const patch = (key: string, p: Partial<EditState>) =>
    setEdits((m) => ({ ...m, [key]: { ...(m[key] ?? defaultEdit()), ...p } }))

  const save = useMutation({
    mutationFn: ({ key }: { key: string }) => {
      const e = edits[key] ?? defaultEdit()
      return api.saveModelConfig(key, {
        enabled: e.enabled, baseURL: e.baseURL, apiKey: e.apiKey, model: e.model,
        timeoutSec: e.timeoutSec, stream: e.stream,
      })
    },
    onSuccess: (dto, { key }) => {
      msg.success(`「${key}」已保存——重启后端后生效`)
      // 只更新被保存行（保留其他行未保存的编辑；apiKey 输入框保持清空）
      setEdits((m) => ({
        ...m,
        [key]: {
          ...(m[key] ?? defaultEdit()),
          enabled: dto.enabled, baseURL: dto.baseURL, model: dto.model,
          timeoutSec: dto.timeoutSec || 15, stream: dto.stream,
        },
      }))
    },
    onError: (err: Error, { key }) => {
      if (err.message.includes('403')) setForbidden(true)
      msg.error(`保存「${key}」失败：${err.message}`)
    },
  })

  const test = useMutation({
    mutationFn: ({ key }: { key: string }) => {
      const e = edits[key] ?? defaultEdit()
      return api.testModelConfig(key, {
        enabled: e.enabled, baseURL: e.baseURL, apiKey: e.apiKey, model: e.model,
        timeoutSec: e.timeoutSec, stream: e.stream,
      })
    },
    onSuccess: (r) => {
      if (r.ok) msg.success(`「${r.key}」连通性测试通过`)
      else msg.error(`「${r.key}」测试失败：${r.error ?? '未知错误'}`)
    },
    onError: (err: Error, { key }) => {
      if (err.message.includes('403')) setForbidden(true)
      msg.error(`测试「${key}」失败：${err.message}`)
    },
  })

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="large">
      <Card
        title="模型配置"
        extra={<Typography.Text type="secondary">DB 持久化 · env 仅首启种子 · 保存后重启后端生效</Typography.Text>}
      >
        {forbidden && (
          <Alert
            type="warning" showIcon style={{ marginBottom: 12 }}
            message="管理权限不足"
            description="模型配置端点需要 AIOPS_MODELCFGADMINAPIS=true（apikey 模式还需 AIOPS_MODELCFGADMINTENANTS 白名单），修改后重启后端。"
          />
        )}
        <Table
          rowKey="key"
          size="small"
          loading={isLoading}
          dataSource={MODELS}
          pagination={false}
          columns={[
            {
              title: '用途', dataIndex: 'key',
              render: (key: string) => {
                const m = MODELS.find((x) => x.key === key)!
                return (
                  <Space direction="vertical" size={0}>
                    <b>{m.label}</b>
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>{m.hint}</Typography.Text>
                  </Space>
                )
              },
            },
            {
              title: '启用', dataIndex: 'key', width: 60,
              render: (key: string) => (
                <Switch checked={edits[key]?.enabled} onChange={(v) => patch(key, { enabled: v })} />
              ),
            },
            {
              title: 'BaseURL', dataIndex: 'key', width: 240,
              render: (key: string) => {
                const m = MODELS.find((x) => x.key === key)!
                return (
                  <Input
                    value={edits[key]?.baseURL}
                    placeholder={m.placeholder}
                    onChange={(e) => patch(key, { baseURL: e.target.value })}
                    onPressEnter={() => save.mutate({ key })}
                  />
                )
              },
            },
            {
              title: 'Model', dataIndex: 'key', width: 160,
              render: (key: string) => (
                <Input
                  value={edits[key]?.model}
                  placeholder="如 deepseek-chat / BAAI/bge-m3"
                  onChange={(e) => patch(key, { model: e.target.value })}
                  onPressEnter={() => save.mutate({ key })}
                />
              ),
            },
            {
              title: 'API Key', dataIndex: 'key', width: 200,
              render: (key: string) => {
                const e = edits[key]
                const set = data?.items.find((x) => x.key === key)?.apiKeySet
                return (
                  <Input.Password
                    value={e?.apiKey ?? ''}
                    placeholder={set ? '已配置，留空保持不变' : '未配置，请输入'}
                    onChange={(ev) => patch(key, { apiKey: ev.target.value })}
                  />
                )
              },
            },
            {
              title: '参数', dataIndex: 'key',
              render: (key: string) => {
                const e = edits[key] ?? defaultEdit()
                return (
                  <Space size={8} wrap>
                    <InputNumber
                      size="small" min={1} max={300} placeholder="超时秒"
                      value={e.timeoutSec}
                      onChange={(v) => patch(key, { timeoutSec: v ?? 15 })}
                    />
                    <span>
                      <Typography.Text type="secondary">流式</Typography.Text>
                      <Switch size="small" checked={e.stream} onChange={(v) => patch(key, { stream: v })} />
                    </span>
                  </Space>
                )
              },
            },
            {
              title: '操作', dataIndex: 'key', width: 140,
              render: (key: string) => (
                <Space>
                  <Button
                    size="small"
                    loading={test.isPending && test.variables?.key === key}
                    onClick={() => test.mutate({ key })}
                  >
                    测试
                  </Button>
                  <Button
                    size="small" type="primary"
                    loading={save.isPending && save.variables?.key === key}
                    onClick={() => save.mutate({ key })}
                  >
                    保存
                  </Button>
                </Space>
              ),
            },
          ]}
        />
        <Typography.Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0 }}>
          说明：Chat LLM 配置保存在 <Typography.Text code>model_configs</Typography.Text> 表（env 的 <Typography.Text code>AIOPS_LLM*</Typography.Text> 仅首启种子）。
          Embedding（RAG 向量化）不走此页——由 <Typography.Text code>AIOPS_EMBEDDING*</Typography.Text> 环境变量直接配置（如千问 text-embedding-v3）。
          API Key 只在保存时写入，列表仅回显"是否已配置"；留空保存 = 保留旧值。
          客户端在启动时装配——<b>保存后需重启后端生效</b>。停用 = Chat 关闭（RCA 纯确定性），不回退 env。
        </Typography.Paragraph>
      </Card>
      {contextHolder}
    </Space>
  )
}
