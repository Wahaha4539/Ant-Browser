export function formatTrafficBytes(value: number | undefined): string {
  if (value === undefined || !Number.isFinite(value)) return '—'
  const bytes = Math.max(0, value)
  if (bytes < 1024) return `${Math.round(bytes)} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)) - 1, units.length - 1)
  return `${(bytes / 1024 ** (index + 1)).toFixed(1)} ${units[index]}`
}
