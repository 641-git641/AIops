import type { ReactNode } from 'react'
import { Card, Space, Typography } from 'antd'
import { getAPIKey } from '../api'

// GuideCard 接入向导卡片（KEEPON 5.2/8.2）：可复制 curl + 测试按钮。
// 用于 Alerts 空状态与总览空状态——"用真实操作替代大段产品说明"。

export interface GuideCardProps {
  title?: string
  description?: string
  extra?: ReactNode // 测试按钮位
}

/** 通用告警入口 curl 示例（origin 取自当前地址；apikey 模式自动带认证头）。 */
export function genericCurl(): string {
  const key = getAPIKey()
  const auth = key ? `  -H 'X-API-Key: ${key}' \\\n` : ''
  return [
    `curl -X POST ${window.location.origin}/api/v1/webhook/generic \\`,
    `  -H 'Content-Type: application/json' \\`,
    auth,
    `  -d '{"title":"磁盘使用率过高","severity":"warning","status":"firing",`,
    `       "source":"my-script","dedupKey":"node-1-root-disk",`,
    `       "labels":{"host":"node-1","disk":"/"},"description":"当前 92%"}'`,
  ]
    .filter((line) => line !== '\n')
    .join('\n')
}

export default function GuideCard({ title, description, extra }: GuideCardProps) {
  return (
    <Card size="small" style={{ maxWidth: 680, margin: '0 auto' }}>
      <Space direction="vertical" size="middle" style={{ width: '100%' }}>
        <div>
          <Typography.Text strong style={{ fontSize: 14 }}>
            {title ?? '还没有告警'}
          </Typography.Text>
          <Typography.Paragraph type="secondary" style={{ marginTop: 4, marginBottom: 0 }}>
            {description ?? '复制下面的 curl，发送你的第一条测试告警——一条命令接入任意脚本/监控系统。'}
          </Typography.Paragraph>
        </div>
        <Typography.Paragraph
          copyable={{ text: genericCurl() }}
          style={{ marginBottom: 0 }}
        >
          <Typography.Text code style={{ whiteSpace: 'pre-wrap', fontSize: 12 }}>
            {genericCurl()}
          </Typography.Text>
        </Typography.Paragraph>
        {extra}
      </Space>
    </Card>
  )
}
