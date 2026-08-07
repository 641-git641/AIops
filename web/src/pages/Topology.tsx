import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Card, Col, Empty, Row, Spin, Table, Tag, Tree } from 'antd'
import { ArrowUpOutlined } from '@ant-design/icons'
import { api, type TopologyImpactNode, type TopologyService } from '../api'

// buildTree 由影响面路径数组组装嵌套树（同前缀路径合并为同一节点链）。
interface TreeNode {
  key: string
  title: string
  children: TreeNode[]
}

function buildTree(affected: TopologyImpactNode[]): TreeNode[] {
  const root: TreeNode[] = []
  for (const n of affected) {
    let level = root
    for (const s of n.path) {
      let node = level.find((x) => x.key === s)
      if (!node) {
        node = { key: s, title: s, children: [] }
        level.push(node)
      }
      level = node.children
    }
  }
  return root
}

export default function Topology() {
  const [selected, setSelected] = useState<string | null>(null)

  const { data: list, isLoading } = useQuery({
    queryKey: ['topology'],
    queryFn: () => api.topology(),
  })

  const { data: impact, isLoading: impactLoading } = useQuery({
    queryKey: ['topology-impact', selected],
    queryFn: () => api.topologyImpact(selected!, 5),
    enabled: !!selected,
  })

  const columns = [
    { title: '服务', dataIndex: 'name', key: 'name' },
    {
      title: '类型', dataIndex: 'kind', key: 'kind',
      render: (v: string) => <Tag>{v || 'service'}</Tag>,
    },
    {
      title: '依赖（上游）', dataIndex: 'dependsOn', key: 'dependsOn',
      render: (v: string[]) => (v?.length ? v.join('、') : '-'),
    },
    {
      title: '影响分数', dataIndex: 'impactScore', key: 'impactScore',
      render: (v: number) => v?.toFixed(2) ?? '-',
    },
  ]

  return (
    <Row gutter={16}>
      <Col span={12}>
        <Card title="服务拓扑" size="small">
          <Table<TopologyService>
            rowKey="name"
            size="small"
            loading={isLoading}
            dataSource={list?.items ?? []}
            columns={columns}
            pagination={{ pageSize: 10 }}
            rowClassName={(r) => (r.name === selected ? 'ant-table-row-selected' : '')}
            onRow={(r) => ({
              onClick: () => setSelected(r.name),
              style: { cursor: 'pointer' },
            })}
          />
        </Card>
      </Col>
      <Col span={12}>
        <Card
          size="small"
          title={
            <span>
              影响面 <ArrowUpOutlined />（谁依赖它，深度 ≤ 5）
              {selected && (
                <Tag color="blue" style={{ marginLeft: 8 }}>
                  {selected}
                </Tag>
              )}
              {impact && impact.total > 0 && <Tag color="red">{impact.total} 个受影响服务</Tag>}
            </span>
          }
        >
          {!selected ? (
            <Empty description="点击左侧服务查看故障影响面（SQL 递归 CTE 遍历）" />
          ) : (
            <Spin spinning={impactLoading}>
              {impact && impact.affected.length > 0 ? (
                <Tree defaultExpandAll treeData={buildTree(impact.affected)} />
              ) : (
                <Empty description="无下游依赖（该服务不依赖故障源或被遍历深度限制）" />
              )}
            </Spin>
          )}
        </Card>
      </Col>
    </Row>
  )
}
