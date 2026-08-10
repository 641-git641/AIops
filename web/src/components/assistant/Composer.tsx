import { useState } from 'react'
import { Button, Input } from 'antd'
import { SendOutlined, StopOutlined } from '@ant-design/icons'

// Composer 输入框（Enter 发送、Shift+Enter 换行；运行中禁用并显示停止）。
export default function Composer({
  sending,
  onSend,
  onStop,
}: {
  sending: boolean
  onSend: (content: string) => void
  onStop: () => void
}) {
  const [value, setValue] = useState('')

  const submit = () => {
    const v = value.trim()
    if (!v || sending) return
    onSend(v)
    setValue('')
  }

  return (
    <div style={{ display: 'flex', gap: 8, padding: '12px 16px', borderTop: '1px solid #f0f0f0' }}>
      <Input.TextArea
        value={value}
        onChange={(e) => setValue(e.target.value)}
        onPressEnter={(e) => {
          if (!e.shiftKey) {
            e.preventDefault()
            submit()
          }
        }}
        placeholder="询问平台状态、告警、事故、拓扑影响…（Enter 发送，Shift+Enter 换行）"
        autoSize={{ minRows: 1, maxRows: 5 }}
        disabled={sending}
      />
      {sending ? (
        <Button danger icon={<StopOutlined />} onClick={onStop}>
          停止
        </Button>
      ) : (
        <Button type="primary" icon={<SendOutlined />} onClick={submit}>
          发送
        </Button>
      )}
    </div>
  )
}
