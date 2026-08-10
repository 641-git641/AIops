import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Alert, Button, Card, Input, Space, Typography, message } from 'antd'
import { KeyOutlined, LockOutlined } from '@ant-design/icons'
import { api, clearAPIKey, setAPIKey } from '../api'

// Unlock 解锁页（KEEPON 4.3）：apikey 模式下凭据仅存 sessionStorage。
// 输入 key → 保存 → 探测一次业务端点 → 成功进首页，401 清空报错。
export default function Unlock({ hasKeys }: { hasKeys: boolean }) {
  const [key, setKey] = useState('')
  const [loading, setLoading] = useState(false)
  const [msg, ctx] = message.useMessage()
  const nav = useNavigate()

  const unlock = async () => {
    const k = key.trim()
    if (!k) {
      msg.warning('请粘贴访问密钥')
      return
    }
    setLoading(true)
    setAPIKey(k)
    try {
      await api.alerts({ page: '1', pageSize: '1' }) // 探测：凭据有效则 200
      nav('/incidents', { replace: true })
    } catch (e) {
      clearAPIKey()
      msg.error(`密钥无效：${(e as Error).message}`)
      setLoading(false)
    }
  }

  return (
    <div
      style={{
        minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center',
        background: '#f5f5f5',
      }}
    >
      <Card style={{ width: 420 }} title={<Space><LockOutlined />解锁 AIOps</Space>}>
        {!hasKeys && (
          <Alert
            type="info" showIcon style={{ marginBottom: 16 }}
            message="首次启用访问保护"
            description={
              <>
                owner key 已生成并打印在<strong>服务端控制台</strong>（仅显示一次）。
                找不到的话，可在设置页「重新生成 owner key」。
              </>
            }
          />
        )}
        <Typography.Paragraph type="secondary">
          请输入访问密钥（仅保存在当前浏览器标签页会话中）。
        </Typography.Paragraph>
        <Space direction="vertical" style={{ width: '100%' }} size="middle">
          <Input.Password
            size="large"
            prefix={<KeyOutlined />}
            placeholder="粘贴访问密钥"
            value={key}
            onChange={(e) => setKey(e.target.value)}
            onPressEnter={unlock}
            autoFocus
          />
          <Button type="primary" size="large" block loading={loading} onClick={unlock}>
            解锁
          </Button>
        </Space>
      </Card>
      {ctx}
    </div>
  )
}
