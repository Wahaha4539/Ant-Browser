import { useEffect, useState } from 'react'
import { ArrowDown, ArrowUp, Activity } from 'lucide-react'
import { Card, Button } from '../../../shared/components'
import { getGoApp } from '../api/runtime'
import { formatTrafficBytes } from './trafficFormat'
import { BrowserDomainTrafficModal } from './BrowserDomainTrafficModal'
import type { BrowserProfile, BrowserTrafficSnapshot } from '../types'

export function useBrowserTraffic() {
  const [rows, setRows] = useState<Record<string, BrowserTrafficSnapshot>>({})
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  useEffect(() => {
    let disposed = false
    let timer: ReturnType<typeof setTimeout>
    const refresh = async () => {
      if (!document.hidden) {
        try {
          const app = getGoApp()
          if (!app?.BrowserTrafficList) throw new Error('当前应用不支持流量统计，请使用新版程序')
          const result: BrowserTrafficSnapshot[] = await app.BrowserTrafficList()
          if (!disposed) { setRows(Object.fromEntries((result || []).map(row => [row.profileId, row]))); setError('') }
        } catch (cause) {
          if (!disposed) setError(cause instanceof Error ? cause.message : String(cause))
        } finally {
          if (!disposed) setLoading(false)
        }
      }
      if (!disposed) timer = setTimeout(refresh, 2000)
    }
    void refresh()
    return () => { disposed = true; clearTimeout(timer) }
  }, [])
  return { rows, error, loading }
}

export function BrowserTrafficCell({ row, unavailable = false, onOpenDetail }: { row?: BrowserTrafficSnapshot; unavailable?: boolean; onOpenDetail?: () => void }) {
  const detailButton = onOpenDetail && <button type="button" className="ml-auto shrink-0 text-[var(--color-accent)] hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2" onClick={event => { event.stopPropagation(); onOpenDetail() }} aria-label="查看域名流量详情">详情</button>
  if (!row || row.state === 'not_started') return <div className="flex min-w-[165px] items-center gap-3 text-xs text-[var(--color-text-muted)]"><span title={row?.message}>—</span>{detailButton}</div>
  const title = [row.message, `本次上传 ${formatTrafficBytes(row.sessionUploadedBytes)} · 下载 ${formatTrafficBytes(row.sessionDownloadedBytes)}`, '仅计量通过实例代理连接的 TCP 字节，非代理商账单'].filter(Boolean).join('\n')
  return (
    <div className="min-w-[165px] space-y-1 text-xs tabular-nums" title={title} data-testid="traffic-cell">
      <div className="flex items-center gap-3 text-[var(--color-text-primary)]">
        <span className="inline-flex items-center gap-1"><ArrowUp className="h-3 w-3" aria-hidden="true" /><span className="sr-only">累计上传</span>{formatTrafficBytes(row.uploadedBytes)}</span>
        <span className="inline-flex items-center gap-1"><ArrowDown className="h-3 w-3 text-[var(--color-accent)]" aria-hidden="true" /><span className="sr-only">累计下载</span>{formatTrafficBytes(row.downloadedBytes)}</span>
      </div>
      <div className="flex items-center gap-3">
        <span className={unavailable || row.state === 'unmetered' || row.message ? 'text-[var(--color-warning)]' : 'text-[var(--color-text-muted)]'}>
        {unavailable ? '统计暂不可用' : row.state === 'unmetered' ? '当前连接未计量' : row.message ? '历史保存异常' : row.running ? `↑ ${formatTrafficBytes(row.uploadBytesPerSecond)}/s  ↓ ${formatTrafficBytes(row.downloadBytesPerSecond)}/s` : '已停止 · 历史保留'}
        </span>{detailButton}
      </div>
    </div>
  )
}

export function BrowserTrafficSummary({ profiles, rows, error, loading }: { profiles: BrowserProfile[]; rows: Record<string, BrowserTrafficSnapshot>; error: string; loading: boolean }) {
  const visible = profiles.map(profile => rows[profile.profileId]).filter((row): row is BrowserTrafficSnapshot => !!row)
  const sum = (key: 'uploadedBytes' | 'downloadedBytes') => visible.reduce((total, row) => total + row[key], 0)
  return (
    <div className="flex flex-wrap items-center gap-x-5 gap-y-1 border-b border-[var(--color-border-muted)] px-4 py-2.5 text-xs" data-testid="traffic-summary">
      <span className="inline-flex items-center gap-1.5 font-medium text-[var(--color-text-secondary)]"><Activity className="h-3.5 w-3.5" />当前列表累计</span>
      <span className="tabular-nums text-[var(--color-text-primary)]">上传 {loading ? '—' : formatTrafficBytes(sum('uploadedBytes'))}</span>
      <span className="tabular-nums text-[var(--color-text-primary)]">下载 {loading ? '—' : formatTrafficBytes(sum('downloadedBytes'))}</span>
      {error ? <span role="status" className="text-[var(--color-warning)]" title={error}>统计暂不可用，显示上次数据</span> : <span className="text-[var(--color-text-muted)]" title="含经过实例代理的后台请求；不含 UDP 和绕过代理的连接。升级前已运行的实例需重启后开始计量。">TCP 代理流量 · 每 2 秒更新 · 非账单口径</span>}
    </div>
  )
}

export function BrowserTrafficDetail({ profileId }: { profileId: string }) {
  const { rows, error, loading } = useBrowserTraffic()
  const [detailOpen, setDetailOpen] = useState(false)
  const row = rows[profileId]
  const recorded = row && row.state !== 'not_started'
  const active = row?.state === 'active' && !error
  const metrics = [
    ['累计上传', recorded ? formatTrafficBytes(row.uploadedBytes) : '—'],
    ['累计下载', recorded ? formatTrafficBytes(row.downloadedBytes) : '—'],
    ['本次上传', recorded ? formatTrafficBytes(row.sessionUploadedBytes) : '—'],
    ['本次下载', recorded ? formatTrafficBytes(row.sessionDownloadedBytes) : '—'],
    ['上传速度', active ? `${formatTrafficBytes(row.uploadBytesPerSecond)}/s` : '—'],
    ['下载速度', active ? `${formatTrafficBytes(row.downloadBytesPerSecond)}/s` : '—'],
  ]
  return (
    <Card title="流量统计">
      <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3">
        {metrics.map(([label, value]) => <div key={label}><dt className="text-xs text-[var(--color-text-muted)]">{label}</dt><dd className="mt-1 text-lg font-semibold tabular-nums text-[var(--color-text-primary)]">{value}</dd></div>)}
      </dl>
      <div className="mt-4"><Button size="sm" variant="secondary" onClick={() => setDetailOpen(true)}>查看域名流量详情</Button></div>
      <BrowserDomainTrafficModal profileId={detailOpen ? profileId : null} onClose={() => setDetailOpen(false)} />
      <p className="mt-4 text-xs text-[var(--color-text-muted)]">{loading ? '读取统计中…' : error || row?.message || (row?.running ? '正在计量，退出应用并保留浏览器后继续统计。' : '历史数据保留，启动实例后继续累计。')}</p>
      <p className="mt-1 text-xs text-[var(--color-text-muted)]">仅统计通过实例代理连接的 TCP 字节，含后台请求与协议交互；不含 UDP、绕过代理的连接，不等同代理商账单。</p>
    </Card>
  )
}
