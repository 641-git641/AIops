import { Button, List, Popconfirm } from 'antd'
import { PlusOutlined } from '@ant-design/icons'
import type { AssistantThread } from '../../api'

// ThreadList 会话列表（新建/选中/删除；软删由后端处理）。
export default function ThreadList({
  threads,
  activeId,
  onSelect,
  onCreate,
  onDelete,
}: {
  threads: AssistantThread[]
  activeId?: string
  onSelect: (id: string) => void
  onCreate: () => void
  onDelete: (id: string) => void
}) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%' }}>
      <Button type="primary" icon={<PlusOutlined />} block onClick={onCreate} style={{ marginBottom: 8 }}>
        新建会话
      </Button>
      <List
        size="small"
        dataSource={threads}
        style={{ flex: 1, overflow: 'auto' }}
        renderItem={(t) => (
          <List.Item
            onClick={() => onSelect(t.id)}
            style={{
              cursor: 'pointer',
              background: t.id === activeId ? '#e6f4ff' : undefined,
              borderRadius: 6,
              padding: '6px 10px',
            }}
            actions={
              t.id === activeId
                ? [
                    <Popconfirm
                      key="del"
                      title="删除会话？"
                      onConfirm={(e) => {
                        e?.stopPropagation()
                        onDelete(t.id)
                      }}
                    >
                      <Button
                        size="small"
                        type="text"
                        danger
                        onClick={(e) => e.stopPropagation()}
                      >
                        删除
                      </Button>
                    </Popconfirm>,
                  ]
                : []
            }
          >
            <div style={{ minWidth: 0 }}>
              <div style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{t.title}</div>
              <div style={{ fontSize: 12, color: '#999' }}>
                {new Date(t.updatedAt).toLocaleString('zh-CN', { hour12: false })}
              </div>
            </div>
          </List.Item>
        )}
      />
    </div>
  )
}
