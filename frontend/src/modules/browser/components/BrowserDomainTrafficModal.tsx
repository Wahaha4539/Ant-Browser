import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Search } from 'lucide-react'
import { Button, Modal } from '../../../shared/components'
import { getGoApp } from '../api/runtime'
import type { BrowserTrafficSnapshot } from '../types'
import { formatTrafficBytes } from './trafficFormat'

export function BrowserDomainTrafficModal({ profileId, profileName, onClose }: { profileId: string | null; profileName?: string; onClose: () => void }) {
  const [row, setRow] = useState<BrowserTrafficSnapshot | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [query, setQuery] = useState('')
  const [scope, setScope] = useState<'total' | 'session'>('total')
  const [sort, setSort] = useState('total')
  const previousFocusRef = useRef<HTMLElement | null>(null)
  const focusSearch = useCallback((input: HTMLInputElement | null) => {
    if (input && profileId) {
      previousFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
      input.focus()
    }
  }, [profileId])
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  useEffect(() => {
    if (!profileId) return
    let disposed = false
    let timer: ReturnType<typeof setTimeout>
    setRow(null); setLoading(true); setError(''); setQuery(''); setScope('total'); setSort('total')
    const refresh = async () => {
      if (!document.hidden) {
        try {
          const app = getGoApp()
          if (!app?.BrowserTrafficDetail) throw new Error('当前程序不支持域名统计，请升级后重启实例')
          const result: BrowserTrafficSnapshot = await app.BrowserTrafficDetail(profileId)
          if (!disposed) { setRow(result); setError('') }
        } catch (cause) {
          if (!disposed) setError(cause instanceof Error ? cause.message : String(cause))
        } finally {
          if (!disposed) setLoading(false)
        }
      }
      if (!disposed) timer = setTimeout(refresh, 2000)
    }
    const closeOnEscape = (event: KeyboardEvent) => { if (event.key === 'Escape') onCloseRef.current() }
    document.addEventListener('keydown', closeOnEscape)
    void refresh()
    return () => { disposed = true; clearTimeout(timer); document.removeEventListener('keydown', closeOnEscape); if (previousFocusRef.current?.isConnected) previousFocusRef.current.focus() }
  }, [profileId])

  const domains = useMemo(() => {
    const search = query.trim().toLowerCase()
    return (row?.domains ?? []).map(domain => {
      const up = scope === 'session' ? domain.sessionUploadedBytes : domain.uploadedBytes
      const down = scope === 'session' ? domain.sessionDownloadedBytes : domain.downloadedBytes
      return { ...domain, label: domain.domain || '未识别 / 历史流量', up, down, total: up + down }
    }).filter(domain => domain.total > 0 && domain.label.toLowerCase().includes(search)).sort((a, b) => {
      if (sort === 'domain') return a.label.localeCompare(b.label)
      const delta = sort === 'upload' ? b.up - a.up : sort === 'download' ? b.down - a.down : b.total - a.total
      return delta || a.label.localeCompare(b.label)
    })
  }, [row, scope, query, sort])
  const up = scope === 'session' ? row?.sessionUploadedBytes : row?.uploadedBytes
  const down = scope === 'session' ? row?.sessionDownloadedBytes : row?.downloadedBytes
  const total = (up ?? 0) + (down ?? 0)

  return (
    <Modal open={!!profileId} onClose={onClose} title={profileName ? `${profileName} · 域名流量详情` : '域名流量详情'} width="900px" footer={<Button variant="secondary" onClick={onClose}>关闭</Button>}>
      <div data-testid="domain-traffic-detail" role={profileId ? 'dialog' : undefined} aria-modal={profileId ? true : undefined} aria-label="域名流量详情">
        <div className="mb-5 flex flex-wrap items-end justify-between gap-4 border-b border-[var(--color-border-muted)] pb-4">
          <dl className="flex flex-wrap gap-x-8 gap-y-3">
            {([['上传', up], ['下载', down], ['总流量', row ? total : undefined]] as const).map(([label, value]) => <div key={label}><dt className="text-xs text-[var(--color-text-muted)]">{scope === 'session' ? '本次' : '累计'}{label}</dt><dd className="mt-1 text-xl font-semibold tabular-nums text-[var(--color-text-primary)]">{formatTrafficBytes(value)}</dd></div>)}
          </dl>
          <span className="text-xs text-[var(--color-text-muted)]">{row?.running ? '每 2 秒更新' : '已停止 · 历史保留'}</span>
        </div>
        <div className="mb-4 flex flex-wrap items-center gap-3">
          <label className="flex min-w-[180px] flex-1 items-center gap-2 rounded-lg border border-[var(--color-border)] px-3 py-2 text-[var(--color-text-muted)]"><Search className="h-4 w-4 shrink-0" aria-hidden="true" /><input ref={focusSearch} aria-label="搜索域名" placeholder="搜索域名或 IP" value={query} onChange={event => setQuery(event.target.value)} className="w-full min-w-0 bg-transparent text-sm text-[var(--color-text-primary)] outline-none" /></label>
          <div className="flex rounded-lg bg-[var(--color-bg-muted)] p-1">
            {(['total', 'session'] as const).map(value => <button key={value} type="button" aria-pressed={scope === value} onClick={() => setScope(value)} className={`rounded-md px-3 py-1.5 text-xs ${scope === value ? 'bg-[var(--color-bg-elevated)] text-[var(--color-accent)] shadow-sm' : 'text-[var(--color-text-secondary)]'}`}>{value === 'total' ? '累计' : '本次运行'}</button>)}
          </div>
          <select aria-label="流量排序" value={sort} onChange={event => setSort(event.target.value)} className="rounded-lg border border-[var(--color-border)] bg-[var(--color-bg-elevated)] px-2 py-2 text-xs text-[var(--color-text-secondary)]"><option value="total">总流量从高到低</option><option value="download">下载从高到低</option><option value="upload">上传从高到低</option><option value="domain">域名 A–Z</option></select>
        </div>
        {error && <p role="alert" className="mb-3 text-sm text-[var(--color-warning)]">{error}{row ? '（显示上次数据）' : ''}</p>}
        {row?.message && <p role="status" className="mb-3 text-xs text-[var(--color-warning)]">{row.message}</p>}
        {row && !row.domainStatsAvailable && row.state !== 'not_started' && <p role="status" className="mb-3 text-xs text-[var(--color-warning)]">旧版统计未记录域名。请完全退出旧程序及浏览器，再运行新版并启动实例；旧流量保留在“未识别 / 历史流量”。</p>}
        <div className="max-h-[420px] overflow-auto rounded-lg border border-[var(--color-border)]">
          <table className="w-full min-w-[640px] text-sm" aria-label="各域名流量统计">
            <thead className="sticky top-0 bg-[var(--color-bg-muted)] text-xs text-[var(--color-text-muted)]"><tr><th scope="col" className="px-4 py-3 text-left font-medium">域名 / IP</th><th scope="col" className="px-3 py-3 text-right font-medium">上传</th><th scope="col" className="px-3 py-3 text-right font-medium">下载</th><th scope="col" className="px-3 py-3 text-right font-medium">总流量</th><th scope="col" className="w-32 px-4 py-3 text-right font-medium">占比</th></tr></thead>
            <tbody className="divide-y divide-[var(--color-border-muted)]">
              {domains.map(domain => {
                const percent = total > 0 ? Math.min(100, domain.total / total * 100) : 0
                return <tr key={domain.domain} className="hover:bg-[var(--color-bg-muted)]"><td className="max-w-[300px] px-4 py-3 text-[var(--color-text-primary)]"><span className="block truncate" title={domain.label}>{domain.label}</span></td><td className="whitespace-nowrap px-3 py-3 text-right tabular-nums text-[var(--color-text-secondary)]">{formatTrafficBytes(domain.up)}</td><td className="whitespace-nowrap px-3 py-3 text-right tabular-nums text-[var(--color-text-secondary)]">{formatTrafficBytes(domain.down)}</td><td className="whitespace-nowrap px-3 py-3 text-right font-medium tabular-nums text-[var(--color-text-primary)]">{formatTrafficBytes(domain.total)}</td><td className="px-4 py-3"><div className="text-right text-xs tabular-nums text-[var(--color-text-muted)]">{percent.toFixed(1)}%</div><div className="mt-1 h-1 overflow-hidden rounded-full bg-[var(--color-bg-muted)]"><div className="h-full rounded-full bg-[var(--color-accent)]" style={{ width: `${percent}%` }} /></div></td></tr>
              })}
              {domains.length === 0 && <tr><td colSpan={5} className="px-4 py-12 text-center text-sm text-[var(--color-text-muted)]">{loading ? '读取域名流量中…' : error && !row ? '域名流量暂不可用' : query.trim() ? '没有匹配的域名' : scope === 'session' ? '本次运行暂无域名流量' : '暂无域名流量，启动实例并访问网页后开始统计'}</td></tr>}
            </tbody>
          </table>
        </div>
        <p className="mt-3 text-xs leading-5 text-[var(--color-text-muted)]">按实际连接目标归类，子域名单独统计；仅 IP 的连接保留 IP，不反向猜测域名。未识别、超出归类上限及旧历史流量单独保留。不解密 HTTPS，TCP 代理字节含协议交互，不等同代理商账单。</p>
      </div>
    </Modal>
  )
}
