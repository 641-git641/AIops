import { useQuery } from '@tanstack/react-query'
import {
  AlertOutlined, CloudServerOutlined, DatabaseOutlined, FileTextOutlined,
  ProjectOutlined, ThunderboltOutlined,
} from '@ant-design/icons'
import { api } from '../api'

// Wallboard 平板大屏（KEEPON 5.1）：暗色、大字、只读、30s 自动刷新，适合挂墙/平板常驻。
// 与总览共用 /overview 数据；不做复杂轮播（KEEPON 明示暂缓）。

const sevOrder = ['critical', 'high', 'medium', 'low', 'info'] as const
const sevColor: Record<string, string> = {
  critical: '#f5222d', high: '#fa541c', medium: '#faad14', low: '#3b82f6', info: '#6b7280',
}
const bg = '#0b0f19'
const cardBg = '#131a2a'
const border = '#1f2937'

function fmtUptime(sec: number): string {
  if (sec < 3600) return `${Math.floor(sec / 60)}m`
  return `${Math.floor(sec / 3600)}h ${Math.floor((sec % 3600) / 60)}m`
}

// fmtPct 百分比显示（undefined → '-'）。
function fmtPct(v?: number): string {
  return v === undefined ? '-' : `${v.toFixed(1)}%`
}

function fmtMB(mb?: number): string {
  if (mb === undefined) return '-'
  if (mb >= 1024) return `${(mb / 1024).toFixed(1)} GB`
  return `${mb.toFixed(0)} MB`
}

// cpuColor 负载色（<50 蓝 / <85 黄 / ≥85 红）。
function cpuColor(v?: number): string {
  if (v === undefined) return '#9ca3af'
  if (v >= 85) return '#f87171'
  if (v >= 50) return '#fbbf24'
  return '#4ade80'
}

// pgConnColor 连接占比色（<70% 绿 / <90% 黄 / 更高红）。
function pgConnColor(cur?: number, max?: number): string {
  if (cur === undefined || !max || max <= 0) return '#93c5fd'
  const r = cur / max
  if (r >= 0.9) return '#f87171'
  if (r >= 0.7) return '#fbbf24'
  return '#4ade80'
}

// Wallboard 监控大屏（KEEPON 5.1）：暗色、大字、只读、30s 自动刷新。
// embedded 模式：作为「总览」首页渲染（保留侧边栏导航）；非 embedded 为全屏挂墙模式（/wallboard）。
export default function Wallboard({ embedded = false }: { embedded?: boolean }) {
  const { data } = useQuery({
    queryKey: ['overview'],
    queryFn: () => api.overview('24h'),
    refetchInterval: 30_000,
  })
  const { data: infra } = useQuery({
    queryKey: ['infra'],
    queryFn: api.infraMetrics,
    refetchInterval: 30_000,
  })
  // 告警数量异常检测（KEEPON 七：只读提示；数据不足时显示样本不足说明）
  const { data: anomalies } = useQuery({
    queryKey: ['anomaly-alerts'],
    queryFn: () => api.alertAnomalies(24, 7),
    refetchInterval: 60_000,
    retry: false,
  })
  const anomalyHits = (anomalies?.items ?? []).filter((a) => a.anomaly)

  const c = data?.counts
  const trend = data?.trend ?? []
  const maxTrend = Math.max(1, ...trend.map((p) => p.count))
  const dist = data?.severityDist ?? {}
  const distTotal = sevOrder.reduce((acc, s) => acc + (dist[s] ?? 0), 0)

  const kpi = [
    { icon: <AlertOutlined />, label: '活跃告警', value: c?.activeAlerts ?? 0, color: (c?.activeAlerts ?? 0) > 0 ? '#f87171' : '#93c5fd' },
    { icon: <ProjectOutlined />, label: '活跃事故', value: c?.activeIncidents ?? 0, color: '#fdba74' },
    { icon: <ThunderboltOutlined />, label: '执行中', value: c?.runningTasks ?? 0, color: '#93c5fd' },
    { icon: <FileTextOutlined />, label: '知识条目', value: c?.knowledgeDocs ?? 0, color: '#93c5fd' },
  ]

  return (
    <div style={{ minHeight: embedded ? undefined : '100vh', background: bg, color: '#e5e7eb', padding: 24, fontFamily: 'system-ui, sans-serif' }}>
      {/* 顶栏 */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', marginBottom: 20 }}>
        <div style={{ fontSize: embedded ? 22 : 28, fontWeight: 700, letterSpacing: 1 }}>AIOps · 监控总览</div>
        <div style={{ fontSize: 14, color: '#9ca3af' }}>
          {embedded && (
            <a href="/wallboard" style={{ color: '#3b82f6', marginRight: 16 }}>全屏大屏 →</a>
          )}
          {new Date().toLocaleString('zh-CN', { hour12: false })} · 30s 自动刷新 · 只读
        </div>
      </div>

      {/* KPI 大字卡片 */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(4, 1fr)', gap: 16, marginBottom: 20 }}>
        {kpi.map((k) => (
          <div key={k.label} style={{ background: cardBg, border: `1px solid ${border}`, borderRadius: 12, padding: '20px 24px' }}>
            <div style={{ fontSize: 16, color: '#9ca3af', marginBottom: 8 }}>
              <span style={{ marginRight: 8 }}>{k.icon}</span>{k.label}
            </div>
            <div style={{ fontSize: 48, fontWeight: 700, color: k.color, lineHeight: 1.1 }}>{k.value}</div>
          </div>
        ))}
      </div>

      {/* 趋势 + 分布 */}
      <div style={{ display: 'grid', gridTemplateColumns: '3fr 1fr', gap: 16, marginBottom: 20 }}>
        <div style={{ background: cardBg, border: `1px solid ${border}`, borderRadius: 12, padding: 20 }}>
          <div style={{ fontSize: 16, color: '#9ca3af', marginBottom: 12 }}>24h 告警趋势（当前小时{trend.length ? ` ${new Date(trend[trend.length - 1].hour).getHours()}时` : ''}）</div>
          <div style={{ display: 'flex', alignItems: 'flex-end', height: 200, gap: 3 }}>
            {trend.map((p) => (
              <div
                key={p.hour}
                title={`${new Date(p.hour).getHours()}时: ${p.count}条`}
                style={{
                  flex: 1,
                  height: p.count > 0 ? `${(p.count / maxTrend) * 100}%` : 0,
                  background: p.count > 0 ? '#3b82f6' : 'transparent',
                  minHeight: p.count > 0 ? 3 : 0,
                  borderRadius: '2px 2px 0 0',
                }}
              />
            ))}
          </div>
        </div>
        <div style={{ background: cardBg, border: `1px solid ${border}`, borderRadius: 12, padding: 20 }}>
          <div style={{ fontSize: 16, color: '#9ca3af', marginBottom: 12 }}>严重度分布（{distTotal}）</div>
          {sevOrder.map((s) => {
            const n = dist[s] ?? 0
            const pct = distTotal > 0 ? (n / distTotal) * 100 : 0
            return (
              <div key={s} style={{ marginBottom: 14 }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 14, marginBottom: 4 }}>
                  <span>{s}</span>
                  <span style={{ color: sevColor[s] }}>{n}</span>
                </div>
                <div style={{ height: 8, background: '#1f2937', borderRadius: 4 }}>
                  <div style={{ width: `${pct}%`, height: '100%', background: sevColor[s], borderRadius: 4 }} />
                </div>
              </div>
            )
          })}
        </div>
      </div>

      {/* 基础设施（主机 / PostgreSQL / docker 容器） */}
      <div style={{ display: 'grid', gridTemplateColumns: '2fr 2fr 2fr', gap: 16, marginBottom: 20 }}>
        <div style={{ background: cardBg, border: `1px solid ${border}`, borderRadius: 12, padding: 20 }}>
          <div style={{ fontSize: 16, color: '#9ca3af', marginBottom: 12 }}>
            <CloudServerOutlined style={{ marginRight: 8 }} />主机
          </div>
          <div style={{ marginBottom: 12 }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 14, marginBottom: 4 }}>
              <span>CPU（系统 / 进程）</span>
              <span style={{ color: cpuColor(infra?.host.cpuPercent) }}>{fmtPct(infra?.host.cpuPercent)} / {fmtPct(infra?.host.processCpuPercent)}</span>
            </div>
            <div style={{ height: 8, background: '#1f2937', borderRadius: 4 }}>
              <div style={{ width: `${Math.min(100, infra?.host.cpuPercent ?? 0)}%`, height: '100%', background: '#3b82f6', borderRadius: 4 }} />
            </div>
          </div>
          <div style={{ marginBottom: 12 }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 14, marginBottom: 4 }}>
              <span>内存</span>
              <span style={{ color: '#93c5fd' }}>{fmtPct(infra?.host.memPercent)}（{fmtMB(infra?.host.memUsedMb)} / {fmtMB(infra?.host.memTotalMb)}）</span>
            </div>
            <div style={{ height: 8, background: '#1f2937', borderRadius: 4 }}>
              <div style={{ width: `${Math.min(100, infra?.host.memPercent ?? 0)}%`, height: '100%', background: '#8b5cf6', borderRadius: 4 }} />
            </div>
          </div>
          <div style={{ fontSize: 14, color: '#9ca3af' }}>
            Go 堆 {fmtMB(infra?.host.goHeapMb)} · {infra?.host.goRoutines ?? 0} goroutine
            {infra && infra.host.diskFreeGb > 0 && ` · 磁盘剩余 ${infra.host.diskFreeGb.toFixed(1)}GB`}
          </div>
          {/* CPU 趋势（历史采样） */}
          {infra && infra.history.length > 1 && (
            <div style={{ display: 'flex', alignItems: 'flex-end', height: 36, gap: 1, marginTop: 10 }}>
              {infra.history.map((h, i) => (
                <div key={i} title={`${new Date(h.sampledAt).toLocaleTimeString('zh-CN', { hour12: false })} CPU ${h.cpuPercent.toFixed(1)}%`}
                  style={{ flex: 1, height: `${Math.max(4, Math.min(100, h.cpuPercent))}%`, background: '#3b82f6', minWidth: 2 }} />
              ))}
            </div>
          )}
        </div>

        <div style={{ background: cardBg, border: `1px solid ${border}`, borderRadius: 12, padding: 20 }}>
          <div style={{ fontSize: 16, color: '#9ca3af', marginBottom: 12 }}>
            <DatabaseOutlined style={{ marginRight: 8 }} />PostgreSQL
          </div>
          <div style={{ marginBottom: 10, fontSize: 15 }}>
            连接 <span style={{ fontWeight: 600, color: pgConnColor(infra?.pg.connections, infra?.pg.backendMax) }}>{infra?.pg.connections ?? 0}</span>
            {infra && infra.pg.backendMax > 0 && <span style={{ color: '#6b7280', fontSize: 13 }}> / {infra.pg.backendMax}</span>}
            {infra && infra.pg.activeQueries > 0 && <span style={{ color: '#fbbf24', fontSize: 13 }}>（活跃 {infra.pg.activeQueries}）</span>}
          </div>
          <div style={{ marginBottom: 10, fontSize: 15 }}>
            缓存命中率 <span style={{ fontWeight: 600, color: '#4ade80' }}>{infra ? `${infra.pg.cacheHitRatio.toFixed(1)}%` : '-'}</span>
          </div>
          <div style={{ marginBottom: 10, fontSize: 15 }}>
            库大小 <span style={{ fontWeight: 600 }}>{infra ? `${infra.pg.dbSizeMb.toFixed(1)} MB` : '-'}</span>
          </div>
          <div style={{ fontSize: 14, color: '#9ca3af' }}>
            提交 {infra?.pg.xactCommit ?? 0} · 回滚 {infra?.pg.xactRollback ?? 0} · 死锁 {infra?.pg.deadlocks ?? 0}
          </div>
        </div>

        <div style={{ background: cardBg, border: `1px solid ${border}`, borderRadius: 12, padding: 20 }}>
          <div style={{ fontSize: 16, color: '#9ca3af', marginBottom: 12 }}>中间件容器（docker）</div>
          {(infra?.containers?.length ?? 0) === 0 && <div style={{ color: '#6b7280', fontSize: 14 }}>docker 不可用或暂无容器</div>}
          {(infra?.containers ?? []).map((c) => (
            <div key={c.name} style={{ marginBottom: 12 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 14, marginBottom: 4 }}>
                <span style={{ fontWeight: 600 }}>{c.name}</span>
                <span>
                  <span style={{ color: cpuColor(c.cpuPercent) }}>CPU {fmtPct(c.cpuPercent)}</span>
                  <span style={{ color: '#93c5fd', marginLeft: 10 }}>MEM {fmtPct(c.memPercent)}</span>
                </span>
              </div>
              <div style={{ display: 'flex', gap: 6 }}>
                <div style={{ flex: 1, height: 6, background: '#1f2937', borderRadius: 3 }}>
                  <div style={{ width: `${Math.min(100, c.cpuPercent)}%`, height: '100%', background: '#3b82f6', borderRadius: 3 }} />
                </div>
                <div style={{ flex: 1, height: 6, background: '#1f2937', borderRadius: 3 }}>
                  <div style={{ width: `${Math.min(100, c.memPercent)}%`, height: '100%', background: '#8b5cf6', borderRadius: 3 }} />
                </div>
              </div>
              <div style={{ fontSize: 12, color: '#6b7280', marginTop: 2 }}>
                {fmtMB(c.memUsedMb)} 内存 · {c.pids} 进程
              </div>
            </div>
          ))}
        </div>
      </div>

      {/* 异常检测（只读提示，不自动动作） */}
      <div style={{ background: cardBg, border: `1px solid ${border}`, borderRadius: 12, padding: 20 }}>
        <div style={{ fontSize: 16, color: '#9ca3af', marginBottom: 12 }}>
          告警异常检测（24h vs 历史同期基线 ±2σ · 仅提示）
        </div>
        {anomalyHits.length === 0 ? (
          <div style={{ color: '#6b7280', fontSize: 15 }}>
            {anomalies ? '当前无异常：各小时告警数均在基线区间内' : '异常检测暂不可用（后端未装配）'}
          </div>
        ) : (
          anomalyHits.slice(0, 5).map((a) => (
            <div key={a.hour} style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '6px 0', fontSize: 15, borderBottom: `1px solid ${border}` }}>
              <span style={{ width: 8, height: 8, borderRadius: 4, background: '#f87171' }} />
              <span style={{ color: '#f87171', fontWeight: 600 }}>{a.count} 条</span>
              <span>（基线 {a.baseline}±{a.sigma}σ，区间 {a.lower}~{a.upper}）</span>
              <span style={{ color: '#6b7280', fontSize: 13 }}>{new Date(a.hour).toLocaleString('zh-CN', { hour12: false })}</span>
              <span style={{ color: '#9ca3af', fontSize: 13, marginLeft: 'auto' }}>{a.reason}</span>
            </div>
          ))
        )}
        {(anomalies?.items ?? []).some((a) => a.reason?.includes('样本不足')) && (
          <div style={{ color: '#6b7280', fontSize: 13, marginTop: 8 }}>
            部分小时历史样本不足（&lt;3 天数据），基线待积累
          </div>
        )}
      </div>

      {/* 最近事件 + 系统状态 */}
      <div style={{ display: 'grid', gridTemplateColumns: '3fr 1fr', gap: 16 }}>
        <div style={{ background: cardBg, border: `1px solid ${border}`, borderRadius: 12, padding: 20 }}>
          <div style={{ fontSize: 16, color: '#9ca3af', marginBottom: 12 }}>最近事件</div>
          {(data?.recentEvents?.length ?? 0) === 0 && <div style={{ color: '#6b7280', fontSize: 15 }}>暂无事件</div>}
          {(data?.recentEvents ?? []).slice(0, 12).map((e) => (
            <div key={`${e.kind}-${e.createdAt}-${e.title}`} style={{ display: 'flex', alignItems: 'center', gap: 12, padding: '6px 0', fontSize: 15, borderBottom: `1px solid ${border}` }}>
              <span style={{ width: 8, height: 8, borderRadius: 4, background: e.kind === 'alert' ? '#3b82f6' : '#6b7280' }} />
              <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{e.title}</span>
              {e.severity && <span style={{ color: sevColor[e.severity] ?? '#9ca3af' }}>{e.severity}</span>}
              <span style={{ color: '#6b7280', fontSize: 13 }}>{new Date(e.createdAt).toLocaleTimeString('zh-CN', { hour12: false })}</span>
            </div>
          ))}
        </div>
        <div style={{ background: cardBg, border: `1px solid ${border}`, borderRadius: 12, padding: 20, fontSize: 15 }}>
          <div style={{ fontSize: 16, color: '#9ca3af', marginBottom: 12 }}>系统状态</div>
          <div style={{ marginBottom: 10 }}>
            数据库 <span style={{ color: data?.system.dbOk ? '#4ade80' : '#f87171', fontWeight: 600 }}>{data?.system.dbOk ? '正常' : '异常'}</span>
          </div>
          <div style={{ marginBottom: 10 }}>队列 <span style={{ fontWeight: 600 }}>{data?.system.queueDepth ?? 0}</span></div>
          <div style={{ marginBottom: 10 }}>RAG 积压 <span style={{ fontWeight: 600 }}>{data?.system.ragBacklog ?? 0}</span></div>
          <div style={{ marginBottom: 10 }}>运行时长 <span style={{ fontWeight: 600 }}>{fmtUptime(data?.system.uptimeSec ?? 0)}</span></div>
          <div>
            最近备份{' '}
            <span style={{ fontWeight: 600, color: data?.latestBackup?.status === 'success' ? '#4ade80' : '#f87171' }}>
              {data?.backupEnabled ? (data.latestBackup?.status ?? '无记录') : '未启用'}
            </span>
          </div>
        </div>
      </div>
    </div>
  )
}
