import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

export function formatCompactEmail(email?: string | null, fallback = '-'): string {
  const value = email?.trim() ?? ''
  if (!value) return fallback

  const parts = value.split('@')
  if (parts.length !== 2 || !parts[0] || !parts[1]) return value

  const [localPart, domainPart] = parts
  const labels = domainPart.split('.').filter(Boolean)
  if (labels.length < 2) return value
  if (labels.length < 3) return value

  const hiddenLevels = labels.length - 2
  return `${localPart}@${'*'.repeat(hiddenLevels)}.${labels.slice(-2).join('.')}`
}

/**
 * 把代理输入归一化为标准 URL（http://user:pass@host:port）。
 *
 * 支持的输入格式（textarea 一行一个混合粘贴）：
 *   - host:port:user:pass         -> http://user:pass@host:port
 *   - host:port                   -> http://host:port
 *   - http://... / https://... / socks5://...   -> 原样保留（仅 trim）
 *   - user:pass@host:port（漏写 scheme） -> http:// + 原样
 *
 * 设计原则：
 *   - 默认 scheme = http（住宅代理 99% 走 HTTP CONNECT）
 *   - user/pass 含 ':' '@' '/' '?' '#' 时进行 encodeURIComponent，防止破坏 URL
 *   - 解析失败时返回 trim 后的原字符串，让后端报具体错误
 */
export function normalizeProxyUrl(raw: string): string {
  const s = raw.trim()
  if (!s) return ''

  // 已含 scheme，直接返回
  if (s.includes('://')) return s

  // IPv6 简写不支持
  if (s.startsWith('[')) return s

  const parts = s.split(':')
  if (parts.length === 2) {
    // host:port
    return `http://${s}`
  }
  if (parts.length === 4) {
    // host:port:user:pass
    const [host, port, user, pass] = parts.map(p => p.trim())
    if (!host || !port) return s
    if (!user && !pass) return `http://${host}:${port}`
    return `http://${encodeURIComponent(user)}:${encodeURIComponent(pass)}@${host}:${port}`
  }
  // 含 @ 但漏写 scheme
  if (s.includes('@')) return `http://${s}`
  return s
}

/**
 * 批量归一化：去空、去重，保留首次出现顺序。
 */
export function normalizeProxyUrls(raws: string[]): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const r of raws) {
    const n = normalizeProxyUrl(r)
    if (!n) continue
    if (seen.has(n)) continue
    seen.add(n)
    out.push(n)
  }
  return out
}
