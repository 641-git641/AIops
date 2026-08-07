import { useEffect, useMemo, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Table, Tag, Button, Space, Card, Switch, Input, message } from 'antd'
import { api } from '../api'

// 支持的渠道（固定展示全部 4 种；DB 中未配置的按默认值渲染）
const CHANNELS: { name: string; label: string; hint: string; placeholder: string }[] = [
  { name: 'dingtalk', label: '钉钉', hint: '群机器人自定义 webhook', placeholder: 'https://oapi.dingtalk.com/robot/send?access_token=…' },
  { name: 'feishu', label: '飞书', hint: '群机器人自定义 webhook', placeholder: 'https://open.feishu.cn/open-apis/bot/v2/hook/…' },
  { name: 'wecom', label: '企业微信', hint: '群机器人 webhook', placeholder: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…' },
  { name: 'http', label: 'HTTP 通用', hint: 'POST JSON Message（可接本地 mock）', placeholder: 'https://example.com/webhook' },
]

interface EditState {
  enabled: boolean
  webhook: string
}

const defaultEdit = (): EditState => ({ enabled: false, webhook: '' })

export default function NotificationChannels() {
  const qc = useQueryClient()
  const [edits, setEdits] = useState<Record<string, EditState>>({})
  const [msg, contextHolder] = message.useMessage()

  const { data, isLoading } = useQuery({
    queryKey: ['notify-channels'],
    queryFn: () => api.notifyChannels(),
  })

  // 首次加载（或刷新后）把 DB 配置铺进本地编辑态
  useEffect(() => {
    if (!data) return
    const next: Record<string, EditState> = {}
    for (const c of CHANNELS) next[c.name] = defaultEdit()
    for (const item of data.items) {
      next[item.name] = { enabled: item.enabled, webhook: item.webhook ?? '' }
    }
    setEdits(next)
  }, [data])

  const save = useMutation({
    mutationFn: ({ name }: { name: string }) => {
      const e = edits[name] ?? defaultEdit()
      return api.saveNotifyChannel(name, { enabled: e.enabled, webhook: e.webhook })
    },
    onSuccess: (_, { name }) => {
      msg.success(`渠道 ${name} 已保存并生效`)
      qc.invalidateQueries({ queryKey: ['notify-channels'] })
    },
    onError: (err: Error, { name }) => msg.error(`保存 ${name} 失败：${err.message}`),
  })

  const test = useMutation({
    mutationFn: (name: string) => api.testNotifyChannel(name),
    onSuccess: (r) => {
      if (r.ok) msg.success(`${r.channel} 测试成功`)
      else msg.error(`${r.channel} 测试失败：${r.error ?? '未知错误'}`)
    },
    onError: (err: Error, name) => msg.error(`测试 ${name} 失败：${err.message}`),
  })

  const setEdit = (name: string, patch: Partial<EditState>) =>
    setEdits((prev) => ({ ...prev, [name]: { ...(prev[name] ?? defaultEdit()), ...patch } }))

  const rows = useMemo(
    () =>
      CHANNELS.map((c) => {
        const e = edits[c.name] ?? defaultEdit()
        return { ...c, enabled: e.enabled, webhook: e.webhook }
      }),
    [edits],
  )

  return (
    <Space direction="vertical" style={{ width: '100%' }} size="middle">
      {contextHolder}
      <Card size="small" title="通知渠道">
        <Table
          rowKey="name"
          size="small"
          loading={isLoading}
          dataSource={rows}
          pagination={false}
          columns={[
            {
              title: '渠道', key: 'channel',
              render: (_, r) => (
                <Space direction="vertical" size={0}>
                  <span style={{ fontWeight: 500 }}>{r.label}</span>
                  <span style={{ color: '#999', fontSize: 12 }}>{r.hint}</span>
                </Space>
              ),
            },
            {
              title: '启用', key: 'enabled',
              render: (_, r) => (
                <Switch
                  checked={r.enabled}
                  onChange={(v) => setEdit(r.name, { enabled: v })}
                />
              ),
            },
            {
              title: 'Webhook 地址', key: 'webhook',
              render: (_, r) => (
                <Input
                  value={r.webhook}
                  placeholder={r.placeholder}
                  onChange={(e) => setEdit(r.name, { webhook: e.target.value })}
                  onPressEnter={() => save.mutate({ name: r.name })}
                />
              ),
            },
            {
              title: '状态', key: 'status',
              render: (_, r) =>
                r.enabled ? <Tag color="green">启用</Tag> : <Tag color="red">停用</Tag>,
            },
            {
              title: '操作', key: 'op',
              render: (_, r) => (
                <Space>
                  <Button size="small" onClick={() => test.mutate(r.name)}>测试</Button>
                  <Button
                    size="small"
                    type="primary"
                    loading={save.isPending && save.variables?.name === r.name}
                    onClick={() => save.mutate({ name: r.name })}
                  >
                    保存
                  </Button>
                </Space>
              ),
            },
          ]}
        />
        <p style={{ color: '#999', fontSize: 12, marginTop: 8 }}>
          配置持久化到 <Tag style={{ fontSize: 11 }}>notification_channels</Tag>{' '}
          表，保存即热生效（无需重启）；env（AIOPS_NOTIFIERCHANNELS + webhook）仅作首次种子，此后以本页配置为准。
          事故新建 / 解决、进入待审批、审批通过 / 拒绝都会推送已启用渠道。
        </p>
      </Card>
    </Space>
  )
}
